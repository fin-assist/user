package postgres

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/ratelimit"
	"github.com/fin-assist/user/internal/verification"
)

func TestIntegrationProviderRefusalRechecksDeadlineAfterPostgresWaits(t *testing.T) {
	for _, tc := range []struct {
		name, operation, barrier string
		busyExpiry               bool
		want                     error
	}{
		{"reject-own-row-refund", "reject", "row", false, verification.ErrMissing},
		{"duplicate-own-row", "duplicate", "row", false, verification.ErrMissing},
		{"duplicate-busy-upsert", "duplicate", "busy", false, verification.ErrMissing},
		{"duplicate-busy-expires-before-unknown", "duplicate", "busy", true, verification.ErrProviderTerminal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _, _ := profileDB(t)
			base := time.Now().UTC().Truncate(time.Microsecond)
			var clock atomic.Int64
			clock.Store(base.UnixNano())
			now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
			repo := NewVerificationStarts(db).WithClock(now)
			limiter, err := ratelimit.New(ratelimit.Defaults(), NewLimits(db), now, prometheus.NewRegistry())
			if err != nil {
				t.Fatal(err)
			}
			a := startRecord(startKeyA, "+79001234567", base)
			var reserved ratelimit.Receipt
			if err := db.Run(t.Context(), func(ctx context.Context) error {
				decision, receipt, err := limiter.Check(ctx, ratelimit.Start, a.Request.Phone, "203.0.113.7")
				if err != nil {
					return err
				}
				if !decision.Allowed {
					return errors.New("fresh refusal fixture exhausted its limits")
				}
				reserved = receipt
				a.Windows = receipt.ReservedWindows()
				var busy time.Time
				a, busy, err = repo.Claim(ctx, a)
				if err == nil && !busy.IsZero() {
					return errors.New("fresh refusal fixture did not own its claim")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			reserved.Committed()
			a = dispatchEvidence(t, db, repo, a, base)
			a.Request.KnownBusyUntil = a.Request.UnknownDeadline.Add(time.Minute)
			if tc.busyExpiry {
				a.Request.KnownBusyUntil = base.Add(time.Minute)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			blocker, err := db.Primary().Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = blocker.Rollback(context.Background()) }()
			fragment := "%phone_verification%"
			if tc.barrier == "row" {
				_, err = blocker.Exec(ctx, `SELECT id FROM phone_verification WHERE id=$1 FOR UPDATE`, a.Request.ID)
			} else {
				fragment = "%INSERT INTO phone_known_busy%"
				_, err = blocker.Exec(ctx, `INSERT INTO phone_known_busy(phone,origin_attempt_id,busy_until) VALUES($1,$2,$3)`, a.Request.Phone, a.Request.ID, a.Request.KnownBusyUntil)
			}
			if err != nil {
				t.Fatal(err)
			}
			ready := make(chan int32, 1)
			done := make(chan error, 1)
			go func() {
				done <- db.Run(ctx, func(tx context.Context) error {
					var pid int32
					if err := db.Writer(tx).QueryRow(tx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
						return err
					}
					ready <- pid
					if tc.operation == "reject" {
						if _, err := limiter.Refund(tx, a.Windows); err != nil {
							return err
						}
						var hits int
						if err := db.Writer(tx).QueryRow(tx, `SELECT sum(hits) FROM rate_limit_window`).Scan(&hits); err != nil {
							return err
						}
						if hits != 0 {
							return errors.New("real refund did not decrement all four slots")
						}
						return repo.Reject(tx, a, base)
					}
					return repo.Duplicate(tx, a, base)
				})
			}()
			var pid int32
			select {
			case pid = <-ready:
			case err := <-done:
				t.Fatal("refusal exited before its database barrier", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			waitPhoneCondition(t, func() bool {
				var waiting bool
				err := db.Primary().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock' AND query LIKE $2)`, pid, fragment).Scan(&waiting)
				return err == nil && waiting
			})
			// Reject's refund has already happened in this uncommitted transaction.
			// Duplicate's UPSERT would succeed once its competing insert rolls back.
			at := a.Request.UnknownDeadline
			if tc.busyExpiry {
				at = a.Request.KnownBusyUntil
			}
			clock.Store(at.UnixNano())
			if err := blocker.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, tc.want) {
					t.Fatal("refusal used a timestamp captured before the lock wait", err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			assertDeadlineAttemptUnpublished(t, db, a)
			var windows, hits, minimum, maximum, busy int
			var untouched bool
			if err := db.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM rate_limit_window),(SELECT sum(hits) FROM rate_limit_window),(SELECT min(hits) FROM rate_limit_window),(SELECT max(hits) FROM rate_limit_window),(SELECT count(*) FROM phone_known_busy),released_at IS NULL AND closed_at IS NULL AND NOT start_terminal AND jsonb_array_length(start_reserved_windows)=4 FROM phone_verification WHERE id=$1`, a.Request.ID).Scan(&windows, &hits, &minimum, &maximum, &busy, &untouched); err != nil || windows != 4 || hits != 4 || minimum != 1 || maximum != 1 || busy != 0 || !untouched {
				t.Fatal("late refusal committed a refund, release or busy fence", windows, hits, minimum, maximum, busy, untouched, err)
			}
		})
	}
}
