package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/ratelimit"
	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/verification"
)

func startRecord(key, phone string, now time.Time) verification.StartRecord {
	return verification.StartRecord{Key: key, Hash: token.Sum("request:" + phone), Windows: make([]ratelimit.Window, 4), Request: verification.Request{Phone: phone, Provider: "test", Hash: token.Sum(key), CreatedAt: now, ExpiresAt: now.Add(verification.RequestTTL)}}
}

const startKeyA = "019c7714-3b77-74d1-9866-000000000001"
const startKeyB = "019c7714-3b77-74d1-9866-000000000002"

func TestStartRepositoryOwnershipBusyAndAtomicCompletion(t *testing.T) {
	db, _, _ := profileDB(t)
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
func TestStartRepositoryRefundUsesPersistedOriginalWindows(t *testing.T) {
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
	if err = db.Primary().QueryRow(t.Context(), `SELECT (SELECT sum(hits) FROM rate_limit_window),(SELECT count(*) FROM phone_verification WHERE released_at IS NULL),(SELECT count(*) FROM phone_verification_start)`).Scan(&hits, &held, &keys); err != nil || hits != 0 || held != 0 || keys != 0 {
		t.Fatal(hits, held, keys, err)
	}
	if _, err = db.Primary().Exec(t.Context(), `DELETE FROM rate_limit_window`); err != nil {
		t.Fatal(err)
	}
	if err = db.Run(t.Context(), func(ctx context.Context) error { _, err := changed.Refund(ctx, found.Windows); return err }); err != nil {
		t.Fatal("old refund recreated retention windows", err)
	}
}
func TestStartRepositoryCorruptionAndCleanupMappingLifetime(t *testing.T) {
	db, service, _, now := phoneState(t)
	repo := NewVerificationStarts(db)
	a := startRecord(startKeyA, "+79001234567", now.Add(-24*time.Hour-2*time.Second))
	if err := db.Run(t.Context(), func(ctx context.Context) error { var err error; a, _, err = repo.Claim(ctx, a); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Primary().Exec(t.Context(), `INSERT INTO idempotency_keys(scope,endpoint,key,request_hash,expires_at) VALUES('00000000-0000-0000-0000-000000000000','/finassist.user.v1.AuthService/StartPhoneVerification',$1,$2,$3)`, a.Key, a.Hash[:], now.Add(5*time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Cleanup(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Find(t.Context(), a.Key); err != nil {
		t.Fatal("live cache lost deletion mapping", err)
	}
	for _, sql := range []string{`UPDATE phone_verification_start SET request_hash='\x01'`, `UPDATE phone_verification_start SET request_hash=decode(repeat('01',32),'hex'),reserved_windows='null'`, `UPDATE phone_verification_start SET reserved_windows='[{}]'`} {
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
