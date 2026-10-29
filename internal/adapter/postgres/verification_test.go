package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/user/internal/adapter/verificationcrypto"
	"github.com/fin-assist/user/internal/auth"
	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/verification"
)

type phoneSigner struct{ fail bool }

func (s phoneSigner) Sign(context.Context, string) (string, error) {
	if s.fail {
		return "", errors.New("signer failed")
	}
	return "access-for-verified-user", nil
}

type noPhoneNotifications struct{}

func (noPhoneNotifications) Subscribe(int32) (verification.Subscription, error) {
	return verification.Subscription{}, errors.New("not used")
}
func phoneState(t *testing.T) (*DB, *verification.Service, *Verifications, time.Time) {
	t.Helper()
	db, _, _ := profileDB(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	guard, err := tombstone.New(db, db.Primary(), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	codec, err := verificationcrypto.New(bytes.Repeat([]byte{42}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	repo := NewVerifications(db)
	return db, verification.New(db, repo, NewAuth(db), guard, phoneSigner{}, codec, noPhoneNotifications{}, func() time.Time { return now }, token.New, 60*24*time.Hour, time.Second), repo, now
}
func seedPendingPhone(t *testing.T, db *DB, raw, phone string, now time.Time) int32 {
	t.Helper()
	h := token.Sum(raw)
	var id int32
	err := db.Primary().QueryRow(t.Context(), `INSERT INTO phone_verification(phone,token_hash,provider,provider_call_id,created_at,expires_at) VALUES($1,$2,'test',$3,$4,$5) RETURNING id`, phone, h[:], "call:"+raw, now.Add(-time.Minute), now.Add(4*time.Minute)).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	seedStoredEvidence(t, db.Primary(), id)
	return id
}
func proofFor(raw, phone string) verification.Proof {
	return verification.Proof{Provider: "test", CallID: "call:" + raw, Phone: phone}
}
func phoneListen(t *testing.T, db *DB) *pgx.Conn {
	t.Helper()
	c, err := pgx.ConnectConfig(t.Context(), db.Primary().Config().ConnConfig.Copy())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Exec(t.Context(), `LISTEN phone_verification`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closePhoneListener(c) })
	return c
}
func noPhoneNotification(t *testing.T, c *pgx.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer cancel()
	if n, err := c.WaitForNotification(ctx); !errors.Is(err, context.DeadlineExceeded) || n != nil {
		t.Fatal("notification before commit", n, err)
	}
}
func TestIntegrationPhoneConfirmationAtomicReplayAndPrimaryReads(t *testing.T) {
	db, s, repo, now := phoneState(t)
	id := seedPendingPhone(t, db, "existing-secret", "+79001234567", now)
	listener := phoneListen(t, db)
	if _, err := repo.ByToken(t.Context(), token.Sum("existing-secret")); err != nil {
		t.Fatal("read replica used", err)
	}
	var changed bool
	err := db.Run(t.Context(), func(ctx context.Context) error {
		var err error
		changed, err = s.Confirm(ctx, proofFor("existing-secret", "+79001234567"))
		if err != nil {
			return err
		}
		noPhoneNotification(t, listener)
		var sessions int
		if err := db.Primary().QueryRow(ctx, `SELECT count(*) FROM auth_session`).Scan(&sessions); err != nil || sessions != 0 {
			t.Fatal("uncommitted session visible", sessions, err)
		}
		return nil
	})
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	n, err := listener.WaitForNotification(ctx)
	if err != nil || n.Payload != strconv.Itoa(int(id)) {
		t.Fatal(n, err)
	}
	r, o, err := s.Snapshot(t.Context(), "existing-secret")
	if err != nil || o.State != verification.Verified || o.IsNewUser || r.UserID != profileID || o.Pair.Refresh == "" {
		t.Fatal(r, o, err)
	}
	if bytes.Contains(r.Ciphertext, []byte(o.Pair.Refresh)) || bytes.Contains(r.Ciphertext, []byte(o.Pair.Access)) {
		t.Fatal("secret stored in plaintext")
	}
	for range 2 {
		if again, err := s.Confirm(t.Context(), proofFor("existing-secret", "+79001234567")); again || err != nil {
			t.Fatal(again, err)
		}
	}
	_, replayed, err := s.Snapshot(t.Context(), "existing-secret")
	if err != nil || replayed != o {
		t.Fatal("issued different tokens", err)
	}
	var sessions int
	if err := db.Primary().QueryRow(t.Context(), `SELECT count(*) FROM auth_session WHERE user_id=$1`, profileID).Scan(&sessions); err != nil || sessions != 1 {
		t.Fatal(sessions, err)
	}
	noPhoneNotification(t, listener)
	if err := db.Run(t.Context(), func(ctx context.Context) error {
		_, err := s.Confirm(ctx, proofFor("existing-secret", "+79001234568"))
		return err
	}); !errors.Is(err, verification.ErrProof) {
		t.Fatal(err)
	}
	newID := seedPendingPhone(t, db, "new-secret", "+79001234569", now)
	rollback := errors.New("rollback confirmation")
	err = db.Run(t.Context(), func(ctx context.Context) error {
		if _, err := s.Confirm(ctx, proofFor("new-secret", "+79001234569")); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	noPhoneNotification(t, listener)
	r, err = repo.ByToken(t.Context(), token.Sum("new-secret"))
	if err != nil || r.VerifiedAt != nil || len(r.Ciphertext) != 0 {
		t.Fatal("partial result", r, err)
	}
	if changed, err := s.Confirm(t.Context(), proofFor("new-secret", "+79001234569")); err != nil || !changed {
		t.Fatal(changed, err)
	}
	n, err = listener.WaitForNotification(ctx)
	if err != nil || n.Payload != strconv.Itoa(int(newID)) {
		t.Fatal(n, err)
	}
	_, o, err = s.Snapshot(t.Context(), "new-secret")
	if err != nil || !o.IsNewUser || o.RegistrationToken == "" || o.Pair != (auth.Pair{}) {
		t.Fatal(o, err)
	}
}
func TestIntegrationPhoneConcurrentConfirmationAndStorageFailures(t *testing.T) {
	db, s, repo, now := phoneState(t)
	id := seedPendingPhone(t, db, "concurrent", "+79001234567", now)
	var wg sync.WaitGroup
	var issued atomic.Int32
	errs := make(chan error, 12)
	for range 12 {
		wg.Go(func() {
			changed, err := s.Confirm(t.Context(), proofFor("concurrent", "+79001234567"))
			if changed {
				issued.Add(1)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if issued.Load() != 1 {
		t.Fatal("multiple issuers", issued.Load())
	}
	if _, err := repo.Lock(t.Context(), id, true); err == nil {
		t.Fatal("lock without transaction")
	}
	if err := repo.Verify(t.Context(), verification.Request{}, now, verification.Outcome{}, nil, nil, ""); err == nil {
		t.Fatal("verify without transaction")
	}
	for _, stage := range []func(context.Context, time.Time, int) (int64, error){repo.CleanupResults, repo.DetachOldRequests, repo.DeleteOldRequests} {
		if _, err := stage(t.Context(), now, 1000); err == nil {
			t.Fatal("cleanup without transaction")
		}
		if err := db.Run(t.Context(), func(ctx context.Context) error { _, err := stage(ctx, now, 0); return err }); err == nil {
			t.Fatal("invalid batch")
		}
	}
	seedPendingPhone(t, db, "corrupt", "+79001234569", now)
	if _, err := db.Primary().Exec(t.Context(), `UPDATE phone_verification SET token_hash=$1 WHERE provider_call_id='call:corrupt'`, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ByCall(t.Context(), "test", "call:corrupt"); err == nil {
		t.Fatal("bad stored hash accepted")
	}
	if _, err := repo.ByCall(t.Context(), "test", "absent"); !errors.Is(err, verification.ErrMissing) {
		t.Fatal(err)
	}
}
func TestIntegrationPhoneCleanupSkipsBusySessionsAndPreservesRefresh(t *testing.T) {
	db, s, _, now := phoneState(t)
	id := seedPendingPhone(t, db, "old-existing", "+79001234567", now)
	if _, err := s.Confirm(t.Context(), proofFor("old-existing", "+79001234567")); err != nil {
		t.Fatal(err)
	}
	_, result, err := s.Snapshot(t.Context(), "old-existing")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Primary().Exec(t.Context(), `UPDATE phone_verification SET created_at=$2,expires_at=$3,verified_at=$3 WHERE id=$1`, id, now.Add(-25*time.Hour), now.Add(-25*time.Hour+5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	locked, err := db.Primary().Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locked.Rollback(context.Background()) }()
	if _, err = locked.Exec(t.Context(), `SELECT id FROM auth_session FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err = s.Cleanup(ctx, 1000); err != nil {
		t.Fatal("cleanup waited for session", err)
	}
	var exists bool
	if err = db.Primary().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM phone_verification WHERE id=$1)`, id).Scan(&exists); err != nil || !exists {
		t.Fatal("request deleted under busy refresh", err)
	}
	if err = locked.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Cleanup(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	var links, requests int
	err = db.Primary().QueryRow(ctx, `SELECT (SELECT count(*) FROM phone_verification WHERE id=$1),(SELECT count(*) FROM auth_session WHERE phone_verification_id IS NULL)`, id).Scan(&requests, &links)
	if err != nil || requests != 0 || links != 1 {
		t.Fatal(requests, links, err)
	}
	guard, _ := tombstone.New(db, db.Primary(), func() time.Time { return now })
	sessions := auth.New(db, NewAuth(db), guard, phoneSigner{}, nil, func() time.Time { return now }, token.New, 60*24*time.Hour)
	if out, err := sessions.Refresh(ctx, result.Pair.Refresh); err != nil || out.Pair.Refresh == "" {
		t.Fatal("live refresh lost", err)
	}
	// A just-confirmed replay and a still-active request survive the age cleanup.
	for i, mode := range []string{"live-result", "active", "expired-result"} {
		phone := fmt.Sprintf("+7900123400%d", i)
		v := seedPendingPhone(t, db, mode, phone, now)
		switch mode {
		case "live-result":
			if _, err = s.Confirm(ctx, proofFor(mode, phone)); err != nil {
				t.Fatal(err)
			}
			_, err = db.Primary().Exec(ctx, `UPDATE phone_verification SET created_at=$2 WHERE id=$1`, v, now.Add(-25*time.Hour))
		case "active":
			_, err = db.Primary().Exec(ctx, `UPDATE phone_verification SET created_at=$2 WHERE id=$1`, v, now.Add(-25*time.Hour))
		case "expired-result":
			if _, err = s.Confirm(ctx, proofFor(mode, phone)); err != nil {
				t.Fatal(err)
			}
			_, err = db.Primary().Exec(ctx, `UPDATE phone_verification SET verified_at=$2 WHERE id=$1`, v, now.Add(-verification.ResultTTL))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.Cleanup(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	var count, ciphertexts int
	err = db.Primary().QueryRow(ctx, `SELECT count(*),count(result_ciphertext) FROM phone_verification`).Scan(&count, &ciphertexts)
	if err != nil || count != 3 || ciphertexts != 1 {
		t.Fatal("retention boundary broken", count, ciphertexts, err)
	}
}

func TestIntegrationPhoneCleanupDoesNotDeadlockRefreshClosingOldRequest(t *testing.T) {
	db, s, _, now := phoneState(t)
	id := seedPendingPhone(t, db, "refresh-cleanup", "+79001234567", now)
	if _, err := s.Confirm(t.Context(), proofFor("refresh-cleanup", "+79001234567")); err != nil {
		t.Fatal(err)
	}
	_, result, err := s.Snapshot(t.Context(), "refresh-cleanup")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Primary().Exec(t.Context(), `UPDATE phone_verification SET created_at=$2,expires_at=$3,verified_at=$3 WHERE id=$1`, id, now.Add(-25*time.Hour), now.Add(-25*time.Hour+5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	guard, _ := tombstone.New(db, db.Primary(), func() time.Time { return now })
	sessions := auth.New(db, NewAuth(db), guard, phoneSigner{}, nil, func() time.Time { return now }, token.New, 60*24*time.Hour)
	hold, err := db.Primary().Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(context.Background()) }()
	if _, err = hold.Exec(t.Context(), `SELECT id FROM phone_verification WHERE id=$1 FOR UPDATE`, id); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	type answer struct {
		o   auth.Rotation
		err error
	}
	done := make(chan answer, 1)
	go func() { o, err := sessions.Refresh(ctx, result.Pair.Refresh); done <- answer{o, err} }()
	waitPhoneCondition(t, func() bool {
		var waiting bool
		err := db.Primary().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE 'UPDATE phone_verification SET user_id=%')`).Scan(&waiting)
		return err == nil && waiting
	})
	short, cancelShort := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancelShort()
	if _, err = s.Cleanup(short, 1000); err != nil {
		t.Fatal("cleanup waited for refresh lock", err)
	}
	if err = hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rotated := <-done
	if rotated.err != nil {
		t.Fatal(rotated.err)
	}
	if _, err = s.Cleanup(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	if _, err = sessions.Refresh(ctx, rotated.o.Pair.Refresh); err != nil {
		t.Fatal("new live refresh damaged by cleanup", err)
	}
}
