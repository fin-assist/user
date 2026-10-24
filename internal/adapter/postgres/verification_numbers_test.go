package postgres

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/verification"
)

func TestCallbackPoolRollsBackWithStartAndReportsLifecycle(t *testing.T) {
	db, _, repo, now := phoneState(t)
	id := seedPendingPhone(t, db, "pool-secret", "+79001234567", now)
	pool := []string{"+74951234567", "+74957654321"}
	if err := repo.SavePool(t.Context(), id, pool, pool[0]); !errors.Is(err, errVerificationTx) {
		t.Fatal(err)
	}
	rollback := errors.New("rollback start")
	err := db.Run(t.Context(), func(ctx context.Context) error {
		if err := repo.SavePool(ctx, id, pool, pool[0]); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	var selected *string
	if err = db.Primary().QueryRow(t.Context(), `SELECT callback_number FROM phone_verification WHERE id=$1`, id).Scan(&selected); err != nil || selected != nil {
		t.Fatal(selected, err)
	}
	err = db.Run(t.Context(), func(ctx context.Context) error { return repo.SavePool(ctx, id, pool, pool[1]) })
	if err != nil {
		t.Fatal(err)
	}
	var stored []string
	if err = db.Primary().QueryRow(t.Context(), `SELECT allowed_phones_for_call,callback_number FROM phone_verification WHERE id=$1`, id).Scan(&stored, &selected); err != nil || !slices.Equal(stored, pool) || selected == nil || *selected != pool[1] {
		t.Fatal(stored, selected, err)
	}
	err = db.Run(t.Context(), func(ctx context.Context) error {
		return repo.SavePool(ctx, id, []string{"+74950000000"}, "+74950000000")
	})
	if !errors.Is(err, verification.ErrMissing) {
		t.Fatal("selection overwritten", err)
	}
	for i, state := range []string{"confirmed", "expired", "old"} {
		h := token.Sum(state)
		created := now.Add(-time.Minute)
		expires := now.Add(time.Minute)
		var verified *time.Time
		if state == "confirmed" {
			verified = &now
		}
		if state == "expired" {
			expires = now.Add(-time.Second)
		}
		if state == "old" {
			created = now.Add(-25 * time.Hour)
			expires = now.Add(-24 * time.Hour)
		}
		_, err = db.Primary().Exec(t.Context(), `INSERT INTO phone_verification(phone,token_hash,provider,provider_call_id,created_at,expires_at,verified_at,callback_number) VALUES($1,$2,'test',$3,$4,$5,$6,$7)`, fmt.Sprintf("+7900123457%d", i), h[:], "pool:"+state, created, expires, verified, pool[1])
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, totals, err := repo.PoolOutcomes(t.Context(), now.Add(-24*time.Hour))
	if err != nil || len(rows) != 3 || totals["pending"] != 1 || totals["confirmed"] != 1 || totals["expired"] != 1 {
		t.Fatal(rows, totals, err)
	}
	n, err := verification.NewNumbers(repo, slog.New(slog.DiscardHandler), prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if err = n.Report(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Primary().Exec(t.Context(), `DELETE FROM phone_verification WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	_, totals, err = repo.PoolOutcomes(t.Context(), now.Add(-24*time.Hour))
	if err != nil || totals["pending"] != 0 {
		t.Fatal(totals, err)
	}
}

func TestZvonokAPIGateIsSharedByKeyAndHonorsCancellation(t *testing.T) {
	db, _, _, _ := phoneState(t)
	a, b := NewZvonokGate(db, "account-key"), NewZvonokGate(db, "account-key")
	if a.subject != b.subject || a.subject == "account-key" {
		t.Fatal("credential storage")
	}
	// Exhaust the current window directly, then verify that another instance
	// cannot acquire a slot before cancellation. This does not depend on timing
	// twenty live network requests around a wall-clock second boundary.
	start := time.Now().UTC().Truncate(time.Second)
	_, err := db.Primary().Exec(t.Context(), `INSERT INTO rate_limit_window(scope,subject,window_start,window_end,hits) VALUES('zvonok_api:1',$1,$2,$3,20)`, a.subject, start, start.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err = b.Wait(ctx); err == nil {
		t.Fatal("cancelled gate allowed")
	}
	other := NewZvonokGate(db, "other-account-key")
	if err = other.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.Primary().QueryRow(t.Context(), `SELECT sum(hits) FROM rate_limit_window WHERE subject=$1`, other.subject).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

func TestZvonokConcurrentGatesCannotMultiplyAccountAllowance(t *testing.T) {
	db, _, _, _ := phoneState(t)
	a, b := NewZvonokGate(db, "shared-key"), NewZvonokGate(db, "shared-key")
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	failures := make(chan error, 45)
	for i := range 45 {
		wg.Go(func() {
			gate := a
			if i%2 == 0 {
				gate = b
			}
			failures <- gate.Wait(ctx)
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	var maxHits, total int
	if err := db.Primary().QueryRow(t.Context(), `SELECT max(hits),sum(hits) FROM rate_limit_window WHERE scope='zvonok_api:1' AND subject=$1`, a.subject).Scan(&maxHits, &total); err != nil || maxHits > 20 || total < 45 {
		t.Fatal(maxHits, total, err)
	}
}
