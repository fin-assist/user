package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fin-assist/user/internal/auth"
	"github.com/fin-assist/user/internal/token"
)

func TestAuthRepositoryPrimaryLocksAndConstraintTranslation(t *testing.T) {
	db, _, _ := profileDB(t)
	repo := NewAuth(db)
	ctx := t.Context()
	now := time.Now().UTC()
	if version, err := repo.CurrentPD(ctx); err != nil || version != "" {
		t.Fatal("unpublished document", version, err)
	}
	if err := db.Run(ctx, func(ctx context.Context) error { _, err := repo.LockLink(ctx, token.Sum("missing"), now); return err }); !errors.Is(err, auth.ErrMissing) {
		t.Fatal("missing session", err)
	}
	if err := db.Run(ctx, func(ctx context.Context) error { return repo.CloseVerification(ctx, 999, profileID, now) }); !errors.Is(err, auth.ErrMissing) {
		t.Fatal("missing verification", err)
	}
	if _, err := db.Primary().Exec(ctx, `INSERT INTO consent_document(type,version,title,url,is_current) VALUES('personal_data','v1','Test only','https://example.test/pd',true)`); err != nil {
		t.Fatal(err)
	}
	version, err := repo.CurrentPD(ctx)
	if err != nil || version != "v1" {
		t.Fatal("read used empty replica", version, err)
	}
	if err := db.Run(ctx, func(ctx context.Context) error {
		id, created, err := repo.CreateUser(ctx, "+79001234567", "UTC", now)
		if err != nil || created || id != profileID {
			t.Fatal(id, created, err)
		}
		if err := repo.LockUser(ctx, id); err != nil {
			return err
		}
		var zone string
		if err := db.Writer(ctx).QueryRow(ctx, `SELECT timezone FROM app_user WHERE id=$1`, id).Scan(&zone); err != nil || zone != "Europe/Moscow" {
			t.Fatal("existing profile overwritten", zone, err)
		}
		return repo.GrantPD(ctx, id, "v1", now)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.RunMapped(ctx, pgx.TxOptions{}, registrationViolation, func(ctx context.Context) error { return repo.GrantPD(ctx, otherProfileID, "missing", now) }); profileReason(auth.ContractError(err)) != "CONSENT_OUTDATED" {
		t.Fatal(err)
	}
	if err := db.RunMapped(ctx, pgx.TxOptions{}, registrationViolation, func(ctx context.Context) error {
		return repo.CreateSession(ctx, profileID, 999, token.Sum("secret"), now, now.Add(time.Hour))
	}); !errors.Is(err, auth.ErrMissing) {
		t.Fatal(err)
	}
	if err := db.Run(ctx, func(ctx context.Context) error { return repo.LockUser(ctx, "019c7714-3b77-74d1-9866-e1f484aae200") }); !errors.Is(err, auth.ErrMissing) {
		t.Fatal(err)
	}
	for _, call := range []func() error{
		func() error { _, e := repo.LockLink(ctx, token.Sum("x"), now); return e }, func() error { return repo.Rotate(ctx, token.Sum("x"), token.Sum("y"), auth.Link{}, now, now) },
		func() error { return repo.Revoke(ctx, auth.Link{}, now) }, func() error { return repo.Logout(ctx, token.Sum("x")) },
		func() error { _, e := repo.Registration(ctx, token.Sum("x"), true); return e }, func() error { _, _, e := repo.CreateUser(ctx, "+79000000001", "UTC", now); return e },
		func() error { return repo.LockUser(ctx, profileID) }, func() error { return repo.GrantPD(ctx, profileID, "v1", now) },
		func() error { return repo.CreateSession(ctx, profileID, 1, token.Sum("x"), now, now) }, func() error { return repo.CloseVerification(ctx, 1, profileID, now) },
		func() error { _, e := repo.CleanupSessions(ctx, now, 1); return e },
	} {
		if err := call(); !errors.Is(err, errAuthTx) {
			t.Fatal("write outside transaction", err)
		}
	}
	for _, constraint := range []string{"auth_session_user_fkey", "auth_session_phone_verification_fkey", "phone_verification_user_fkey", "user_consent_user_fkey"} {
		if !errors.Is(registrationViolation(&pgconn.PgError{Code: "23503", ConstraintName: constraint}), auth.ErrMissing) {
			t.Fatal(constraint)
		}
	}
	if !errors.Is(authError(context.Canceled), context.Canceled) {
		t.Fatal("context lost")
	}
}

func TestAuthRepositoryStorageFailurePreservesAtomicity(t *testing.T) {
	db, _, _ := profileDB(t)
	repo := NewAuth(db)
	now := time.Now().UTC()
	ctx := t.Context()
	// A cancelled statement is a real pgx failure, not a mock of the driver.
	for _, call := range []func(context.Context) error{
		func(ctx context.Context) error {
			_, _, err := repo.CreateUser(ctx, "+79000000009", "UTC", now)
			return err
		},
		func(ctx context.Context) error {
			return repo.Rotate(ctx, token.Sum("old"), token.Sum("next"), auth.Link{}, now, now.Add(time.Hour))
		},
		func(ctx context.Context) error { return repo.CloseVerification(ctx, 999, profileID, now) },
		func(ctx context.Context) error { _, err := repo.CleanupSessions(ctx, now, 1000); return err },
	} {
		err := db.Run(ctx, func(ctx context.Context) error {
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			return call(cancelled)
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatal("storage cancellation lost", err)
		}
	}
	if err := db.Run(ctx, func(ctx context.Context) error {
		return repo.Rotate(ctx, token.Sum("missing"), token.Sum("new"), auth.Link{}, now, now.Add(time.Hour))
	}); !errors.Is(err, auth.ErrMissing) {
		t.Fatal("missing live link", err)
	}
	var n int
	if err := db.Primary().QueryRow(ctx, `SELECT count(*) FROM app_user`).Scan(&n); err != nil || n != 2 {
		t.Fatal("partial user", n, err)
	}
}

func TestAuthRepositoryCleanupKeepsLiveLinks(t *testing.T) {
	db, _, _ := profileDB(t)
	repo := NewAuth(db)
	ctx := t.Context()
	now := time.Now().UTC()
	var verification int32
	h := token.Sum("verification")
	if err := db.Primary().QueryRow(ctx, `INSERT INTO phone_verification(phone,token_hash,provider,created_at,expires_at,verified_at,is_new_user,user_id) VALUES('+79001234567',$1,'test',$2,$3,$2,false,$4) RETURNING id`, h[:], now, now.Add(5*time.Minute), profileID).Scan(&verification); err != nil {
		t.Fatal(err)
	}
	old := token.Sum("old")
	next := token.Sum("new")
	if err := db.RunMapped(ctx, pgx.TxOptions{}, registrationViolation, func(ctx context.Context) error {
		return repo.CreateSession(ctx, profileID, verification, old, now.Add(-time.Hour), now.Add(time.Minute))
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.FindLink(ctx, old, now); err != nil {
		t.Fatal("primary bypassed", err)
	}
	if err := db.Run(ctx, func(ctx context.Context) error {
		l, err := repo.LockLink(ctx, old, now)
		if err != nil {
			return err
		}
		return repo.Rotate(ctx, old, next, l, now, now.Add(time.Hour))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Run(ctx, func(ctx context.Context) error {
		_, err := repo.CleanupSessions(ctx, now.Add(2*time.Minute), 1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.FindLink(ctx, next, now.Add(2*time.Minute)); err != nil {
		t.Fatal("live successor removed", err)
	}
	if err := db.Run(ctx, func(ctx context.Context) error { return repo.CloseVerification(ctx, verification, profileID, now) }); err != nil {
		t.Fatal(err)
	}
	if err := db.Run(ctx, func(ctx context.Context) error { return repo.CloseVerification(ctx, verification, profileID, now) }); err != nil {
		t.Fatal("repeat close", err)
	}
	if err := db.Run(ctx, func(ctx context.Context) error { return repo.CloseVerification(ctx, verification, otherProfileID, now) }); !errors.Is(err, auth.ErrMissing) {
		t.Fatal("foreign result", err)
	}
	if err := db.Run(ctx, func(ctx context.Context) error {
		_, err := repo.CleanupSessions(ctx, now.Add(2*time.Hour), 10)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.Primary().QueryRow(ctx, `SELECT count(*) FROM auth_session`).Scan(&n); err != nil || n != 0 {
		t.Fatal("empty chain kept", n, err)
	}
}
