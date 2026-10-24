package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fin-assist/user/internal/auth"
	"github.com/fin-assist/user/internal/profile"
)

func TestUnitOperationViolationMappingFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, code, constraint string
		mapper                 pgstore.ErrorMapper
		want                   error
	}{
		{"registration document", "23503", "user_consent_document_fkey", registrationViolation, auth.ErrConsentOutdated},
		{"registration user", "23503", "user_consent_user_fkey", registrationViolation, auth.ErrMissing},
		{"session owner", "23503", "auth_session_user_fkey", refreshViolation, auth.ErrMissing},
		{"session verification", "23503", "auth_session_phone_verification_fkey", refreshViolation, auth.ErrMissing},
		{"verification owner", "23503", "phone_verification_user_fkey", refreshViolation, auth.ErrMissing},
		{"grant document", "23503", "user_consent_document_fkey", grantViolation, profile.ErrConsentOutdated},
		{"grant owner", "23503", "user_consent_user_fkey", grantViolation, profile.ErrConsentUserMissing},
		{"grant conflict", "23505", "user_consent_active_key", grantViolation, profile.ErrConsentConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cause := &pgconn.PgError{Code: tc.code, ConstraintName: tc.constraint, Detail: "private-key"}
			if got := tc.mapper(fmt.Errorf("write: %w", cause)); !errors.Is(got, tc.want) {
				t.Fatalf("mapped=%v", got)
			}
			for _, err := range []error{nil, errors.New("domain"), &pgconn.PgError{Code: "99999", ConstraintName: tc.constraint}, &pgconn.PgError{Code: tc.code, ConstraintName: "unknown"}, errors.Join(cause, errors.New("cleanup"))} {
				if got := tc.mapper(err); got != nil {
					t.Fatalf("unexpected translation %v -> %v", err, got)
				}
			}
		})
	}
	if got := refreshViolation(&pgconn.PgError{Code: "23503", ConstraintName: "user_consent_document_fkey"}); got != nil {
		t.Fatal("refresh inherited registration mapping", got)
	}
}

func TestIntegrationGrantMapsDeferredCommitAndAllowsRepair(t *testing.T) {
	db, _, repo := profileDB(t)
	ctx := t.Context()
	if _, err := db.Primary().Exec(ctx, `ALTER TABLE user_consent ALTER CONSTRAINT user_consent_document_fkey DEFERRABLE INITIALLY DEFERRED`); err != nil {
		t.Fatal(err)
	}
	callbackFinished := false
	err := NewProfileTransactions(db).RunGrant(ctx, func(ctx context.Context) error {
		if err := repo.Grant(ctx, profileID, "later", time.Now()); err != nil {
			return fmt.Errorf("must defer FK: %w", err)
		}
		callbackFinished = true
		return nil
	})
	if !callbackFinished || !errors.Is(err, profile.ErrConsentOutdated) || profileReason(profile.GrantError(err)) != "CONSENT_OUTDATED" {
		t.Fatal(callbackFinished, err)
	}
	var n int
	if err := db.Primary().QueryRow(ctx, `SELECT count(*) FROM user_consent`).Scan(&n); err != nil || n != 0 {
		t.Fatal("failed commit persisted grant", n, err)
	}
	err = NewProfileTransactions(db).RunGrant(ctx, func(ctx context.Context) error {
		if err := repo.Grant(ctx, profileID, "later", time.Now()); err != nil {
			return err
		}
		return db.Run(ctx, func(ctx context.Context) error {
			_, err := db.Writer(ctx).Exec(ctx, `INSERT INTO consent_document(type,version,title,url,is_current) VALUES('assistant','later','Future','https://example.test/later',false)`)
			return err
		})
	})
	if err != nil {
		t.Fatal("repair before commit", err)
	}
}
