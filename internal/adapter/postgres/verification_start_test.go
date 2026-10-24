package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/deletion"
	"github.com/fin-assist/user/internal/ratelimit"
	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/verification"
)

func startRecord(key, phone string, now time.Time) verification.StartRecord {
	return verification.StartRecord{Key: key, Hash: token.Sum("request:" + phone), Windows: make([]ratelimit.Window, 4), Request: verification.Request{Phone: phone, Provider: "test", Hash: token.Sum(key), CreatedAt: now, ExpiresAt: now.Add(verification.RequestTTL)}}
}

const startKeyA = "019c7714-3b77-74d1-9866-000000000001"
const startKeyB = "019c7714-3b77-74d1-9866-000000000002"

func TestIntegrationStartPurgeParentCancellationReleasesGuard(t *testing.T) {
	db, _, _ := profileDB(t)
	a := startRecord(startKeyA, "+79001234567", time.Now().UTC())
	if err := db.Run(t.Context(), func(ctx context.Context) error {
		var err error
		a, _, err = NewVerificationStarts(db).Claim(ctx, a)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	parent, err := db.Primary().Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parent.Rollback(context.Background()) }()
	if _, err = parent.Exec(t.Context(), `SELECT 1 FROM phone_verification WHERE id=$1 FOR KEY SHARE`, a.Request.ID); err != nil {
		t.Fatal(err)
	}
	guard, err := tombstone.New(db, db.Primary(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	waiter, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- db.Run(waiter, func(ctx context.Context) error { return guard.Purge(ctx, profileID, NewAccounts(db).Remove) })
	}()
	waitPhoneCondition(t, func() bool {
		var waiting bool
		err := db.Primary().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock')`).Scan(&waiting)
		return err == nil && waiting
	})
	cancel()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err = db.Run(ctx, func(ctx context.Context) error {
		tx, _ := db.Tx(ctx)
		// Cancellation may close the old connection asynchronously. Wait for its
		// server-side rollback, bounded by ctx, before checking the request survived.
		_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,110))`, profileID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = NewVerificationStarts(db).Find(ctx, a.Key); err != nil {
		t.Fatal("cancelled purge removed attempt", err)
	}
}

func TestIntegrationStartPurgeIncludesBusyKeyCommittedWhileDeletionWaits(t *testing.T) {
	db, _, _ := profileDB(t)
	a := startRecord(startKeyA, "+79001234567", time.Now().UTC())
	if err := db.Run(t.Context(), func(ctx context.Context) error {
		var err error
		a, _, err = NewVerificationStarts(db).Claim(ctx, a)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	busy, err := db.Primary().Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Rollback(context.Background()) }()
	// This is the claim's parent lock, taken after an earlier owner lookup.
	if _, err = busy.Exec(t.Context(), `SELECT 1 FROM phone_verification WHERE id=$1 FOR KEY SHARE`, a.Request.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = busy.Exec(t.Context(), `INSERT INTO idempotency_keys(scope,endpoint,key,request_hash,expires_at,resource_type,resource_id) VALUES('00000000-0000-0000-0000-000000000000','/finassist.user.v1.AuthService/StartPhoneVerification',$1,$2,$3,'phone_verification',$4::integer::text)`, startKeyB, a.Hash[:], time.Now().Add(time.Minute), a.Request.ID); err != nil {
		t.Fatal(err)
	}
	guard, err := tombstone.New(db, db.Primary(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- db.Run(ctx, func(ctx context.Context) error { return guard.Purge(ctx, profileID, NewAccounts(db).Remove) })
	}()
	waitPhoneCondition(t, func() bool {
		var waiting bool
		err := db.Primary().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock')`).Scan(&waiting)
		return err == nil && waiting
	})
	if err = busy.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var count int
	if err = db.Primary().QueryRow(ctx, `SELECT (SELECT count(*) FROM idempotency_keys WHERE key=$1)+(SELECT count(*) FROM phone_verification WHERE id=$2)`, startKeyB, a.Request.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("busy key committed behind deletion's key snapshot", count, err)
	}
}

func TestIntegrationStartDeletionRollsBackOnLockedNoOwnerKey(t *testing.T) {
	db, _, _ := profileDB(t)
	now := time.Now().UTC()
	a := startRecord(startKeyA, "+79001234567", now)
	if err := db.Run(t.Context(), func(ctx context.Context) error {
		var err error
		a, _, err = NewVerificationStarts(db).Claim(ctx, a)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Primary().Exec(t.Context(), `INSERT INTO idempotency_keys(scope,endpoint,key,request_hash,expires_at,resource_type,resource_id) VALUES('00000000-0000-0000-0000-000000000000','/finassist.user.v1.AuthService/StartPhoneVerification',$1,$2,$3,'phone_verification',$4::integer::text)`, a.Key, a.Hash[:], now.Add(24*time.Hour), a.Request.ID); err != nil {
		t.Fatal(err)
	}
	locked, err := db.Primary().Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locked.Rollback(context.Background()) }()
	if _, err = locked.Exec(t.Context(), `SELECT 1 FROM idempotency_keys WHERE key=$1 FOR UPDATE`, a.Key); err != nil {
		t.Fatal(err)
	}
	guard, err := tombstone.New(db, db.Primary(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	remove := func(ctx context.Context) error { return guard.Purge(ctx, profileID, NewAccounts(db).Remove) }
	if err = db.Run(ctx, remove); !errors.Is(err, deletion.ErrReplayBusy) {
		t.Fatal("deletion waited for no-owner key while holding guard/user", err)
	}
	// This runs before unlocking the key: rollback must have released both locks.
	if err = db.Run(ctx, func(ctx context.Context) error {
		tx, _ := db.Tx(ctx)
		var free bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,110))`, profileID).Scan(&free); err != nil || !free {
			t.Fatal("deletion retained guard", free, err)
		}
		_, err := tx.Exec(ctx, `SELECT 1 FROM app_user WHERE id=$1 FOR UPDATE NOWAIT`, profileID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = NewVerificationStarts(db).Find(ctx, a.Key); err != nil {
		t.Fatal("partial deletion before rollback", err)
	}
	if err = locked.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = db.Run(ctx, remove); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.Primary().QueryRow(ctx, `SELECT (SELECT count(*) FROM idempotency_keys WHERE key=$1)+(SELECT count(*) FROM phone_verification WHERE start_key=$1)+(SELECT count(*) FROM phone_verification WHERE id=$2)`, a.Key, a.Request.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("phone caches survived successful retry", count, err)
	}
}

func TestIntegrationStartRepositoryOwnershipBusyAndAtomicCompletion(t *testing.T) {
	db, _, _ := profileDB(t)
	var absent bool
	if err := db.Primary().QueryRow(t.Context(), `SELECT to_regclass('phone_verification_start') IS NULL`).Scan(&absent); err != nil || !absent {
		t.Fatal("separate workflow table still exists", absent, err)
	}
	repo := NewVerificationStarts(db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	a := startRecord(startKeyA, "+79001234567", now)
	if _, err := repo.Find(t.Context(), a.Key); !errors.Is(err, verification.ErrMissing) {
		t.Fatal(err)
	}
	for _, fn := range []func(context.Context) error{
		func(ctx context.Context) error { return repo.Forget(ctx, a.Key) }, func(ctx context.Context) error { _, _, err := repo.Claim(ctx, a); return err },
		func(ctx context.Context) error { return repo.Complete(ctx, a, verification.Call{}, token.Hash{}) }, func(ctx context.Context) error { return repo.Reject(ctx, a, now) }, func(ctx context.Context) error { return repo.Duplicate(ctx, a, now) },
	} {
		if err := fn(t.Context()); !errors.Is(err, errVerificationTx) {
			t.Fatal(err)
		}
	}
	if err := db.Run(t.Context(), func(ctx context.Context) error {
		var busy time.Time
		var err error
		a, busy, err = repo.Claim(ctx, a)
		if !busy.IsZero() {
			t.Fatal("free phone busy")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	found, err := repo.Find(t.Context(), a.Key)
	if err != nil || found.Request.ID != a.Request.ID || len(found.Windows) != 4 {
		t.Fatal(found, err)
	}
	b := startRecord(startKeyB, a.Request.Phone, now)
	if err = db.Run(t.Context(), func(ctx context.Context) error {
		var busy time.Time
		var err error
		b, busy, err = repo.Claim(ctx, b)
		if !busy.Equal(a.Request.ExpiresAt) || b.Request.ID != a.Request.ID || b.Windows != nil {
			t.Fatal("busy ownership", busy, b)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err = db.Run(t.Context(), func(ctx context.Context) error { return repo.Forget(ctx, b.Key) }); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.Find(t.Context(), a.Key); err != nil {
		t.Fatal("busy key deleted owner's request", err)
	}
	call := verification.Call{ID: "provider-call", StartedAt: now, Phone: a.Request.Phone, Numbers: []string{"+74951234567"}}
	h := token.Sum("client secret")
	rolled := errors.New("service rollback")
	err = db.Run(t.Context(), func(ctx context.Context) error {
		if err := repo.Complete(ctx, a, call, h); err != nil {
			return err
		}
		return rolled
	})
	if !errors.Is(err, rolled) {
		t.Fatal(err)
	}
	found, err = repo.Find(t.Context(), a.Key)
	if err != nil || found.Request.CallID != "" || found.Request.Hash != a.Request.Hash || len(found.Windows) != 4 {
		t.Fatal("completion leaked rollback", found, err)
	}
	if err = db.Run(t.Context(), func(ctx context.Context) error { return repo.Complete(ctx, a, call, h) }); err != nil {
		t.Fatal(err)
	}
	found, err = repo.Find(t.Context(), a.Key)
	if err != nil || found.Request.CallID != call.ID || found.Request.Hash != h || len(found.Windows) != 0 {
		t.Fatal(found, err)
	}
	if err = db.Run(t.Context(), func(ctx context.Context) error { return repo.Complete(ctx, a, call, h) }); !errors.Is(err, verification.ErrMissing) {
		t.Fatal("repeated completion mutated", err)
	}
	if err = db.Run(t.Context(), func(ctx context.Context) error { return repo.Reject(ctx, a, now) }); !errors.Is(err, verification.ErrMissing) {
		t.Fatal("completed call rejected", err)
	}
	if err = db.Run(t.Context(), func(ctx context.Context) error { return repo.Duplicate(ctx, a, now) }); !errors.Is(err, verification.ErrMissing) {
		t.Fatal("completed call relabeled", err)
	}
}
func TestIntegrationStartRepositoryRefundUsesPersistedOriginalWindows(t *testing.T) {
	db, _, _ := profileDB(t)
	repo := NewVerificationStarts(db)
	now := time.Now().UTC().Truncate(time.Microsecond)
	limiter, err := ratelimit.New(ratelimit.Defaults(), NewLimits(db), func() time.Time { return now }, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	a := startRecord(startKeyA, "+79001234567", now)
	if err = db.Run(t.Context(), func(ctx context.Context) error {
		_, receipt, err := limiter.Check(ctx, ratelimit.Start, a.Request.Phone, "192.0.2.1")
		if err != nil {
			return err
		}
		a.Windows = receipt.ReservedWindows()
		a, _, err = repo.Claim(ctx, a)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	found, err := repo.Find(t.Context(), a.Key)
	if err != nil {
		t.Fatal(err)
	}
	rules := ratelimit.Defaults()
	rules[ratelimit.VerificationPhoneShort] = ratelimit.Rule{Limit: 99, Window: 2 * time.Hour}
	changed, err := ratelimit.New(rules, NewLimits(db), time.Now, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Run(t.Context(), func(ctx context.Context) error {
		if _, err := changed.Refund(ctx, found.Windows); err != nil {
			return err
		}
		return repo.Reject(ctx, found, now)
	}); err != nil {
		t.Fatal(err)
	}
	var hits, held, keys int
	if err = db.Primary().QueryRow(t.Context(), `SELECT (SELECT sum(hits) FROM rate_limit_window),(SELECT count(*) FROM phone_verification WHERE released_at IS NULL),(SELECT count(*) FROM phone_verification WHERE start_key IS NOT NULL)`).Scan(&hits, &held, &keys); err != nil || hits != 0 || held != 0 || keys != 0 {
		t.Fatal(hits, held, keys, err)
	}
	if _, err = db.Primary().Exec(t.Context(), `DELETE FROM rate_limit_window`); err != nil {
		t.Fatal(err)
	}
	if err = db.Run(t.Context(), func(ctx context.Context) error { _, err := changed.Refund(ctx, found.Windows); return err }); err != nil {
		t.Fatal("old refund recreated retention windows", err)
	}
}
func TestIntegrationStartRepositoryCorruptionAndCleanupMappingLifetime(t *testing.T) {
	db, service, _, now := phoneState(t)
	repo := NewVerificationStarts(db)
	a := startRecord(startKeyA, "+79001234567", now.Add(-24*time.Hour-2*time.Second))
	if err := db.Run(t.Context(), func(ctx context.Context) error { var err error; a, _, err = repo.Claim(ctx, a); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Primary().Exec(t.Context(), `INSERT INTO idempotency_keys(scope,endpoint,key,request_hash,expires_at,resource_type,resource_id) VALUES('00000000-0000-0000-0000-000000000000','/finassist.user.v1.AuthService/StartPhoneVerification',$1,$2,$3,'phone_verification',$4::integer::text)`, a.Key, a.Hash[:], now.Add(5*time.Second), a.Request.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Cleanup(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Find(t.Context(), a.Key); err != nil {
		t.Fatal("live cache lost deletion mapping", err)
	}
	for _, sql := range []string{`UPDATE phone_verification SET start_request_hash='\x01'`, `UPDATE phone_verification SET start_request_hash=decode(repeat('01',32),'hex'),start_reserved_windows='null'`, `UPDATE phone_verification SET start_reserved_windows='[{}]'`} {
		if _, err := db.Primary().Exec(t.Context(), sql); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.Find(t.Context(), a.Key); err == nil {
			t.Fatal("corrupt workflow state accepted")
		}
	}
	if _, err := db.Primary().Exec(t.Context(), `UPDATE idempotency_keys SET expires_at=$1`, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Cleanup(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Find(t.Context(), a.Key); !errors.Is(err, verification.ErrMissing) {
		t.Fatal("old cache mapping retained", err)
	}
}
