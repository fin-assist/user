package postgres

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/verification"
)

func TestIntegrationRegistrationPhaseBoundariesNeverPublishLateOrUnmarkedEffects(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{
		{"complete-without-dispatch", verification.ErrMissing},
		{"finalize-pending-request", verification.ErrMissing},
		{"dispatch-at-unknown-deadline", verification.ErrMissing},
		{"duplicate-with-expired-known-busy", verification.ErrProviderTerminal},
		{"complete-with-different-campaign", verification.ErrProviderUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, _, _ := profileDB(t)
			base := time.Now().UTC().Truncate(time.Microsecond)
			var clock atomic.Int64
			clock.Store(base.UnixNano())
			now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
			repo := NewVerificationStarts(db).WithClock(now)
			a := claimEvidence(t, db, repo, startKeyA, "+79001234567", base)
			dispatched := tc.name == "duplicate-with-expired-known-busy" || tc.name == "complete-with-different-campaign"
			if dispatched {
				a = dispatchEvidence(t, db, repo, a, base)
			}
			call := verification.Call{ID: "phase-candidate", Phone: a.Request.Phone, Campaign: a.Request.Campaign, StartedAt: base, ReceivedAt: base, State: verification.Pending, Numbers: []string{"+74951234567"}, ClockSkewTolerance: time.Second}
			if tc.name == "dispatch-at-unknown-deadline" {
				clock.Store(a.Request.UnknownDeadline.UnixNano())
			}
			if tc.name == "duplicate-with-expired-known-busy" {
				// The authenticated foreign holder expired while this attempt's
				// independently captured unknown deadline remains in the future.
				a.Request.KnownBusyUntil = base.Add(30 * time.Second)
				clock.Store(base.Add(time.Minute).UnixNano())
			}
			if tc.name == "complete-with-different-campaign" {
				call.Campaign = "another-campaign"
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			err := db.Run(ctx, func(tx context.Context) error {
				switch tc.name {
				case "complete-without-dispatch", "complete-with-different-campaign":
					return repo.Complete(tx, a, call, token.Sum("unpublished-phase-token"))
				case "finalize-pending-request":
					return repo.FinalizeKey(tx, a)
				case "dispatch-at-unknown-deadline":
					_, first, err := repo.Dispatch(tx, a, now())
					if first {
						return errors.New("expired request acquired its first dispatch marker")
					}
					return err
				case "duplicate-with-expired-known-busy":
					return repo.Duplicate(tx, a, now())
				default:
					return errors.New("unknown phase boundary fixture")
				}
			})
			if !errors.Is(err, tc.want) {
				t.Fatal("out-of-order or late provider event crossed its admission phase", err)
			}
			assertDeadlineAttemptUnpublished(t, db, a)
			var marker, dispatchBound, untouched bool
			var busy, cached int
			if err := db.Primary().QueryRow(ctx, `SELECT first_dispatch_at IS NOT NULL,dispatch_not_after IS NOT NULL,released_at IS NULL AND closed_at IS NULL AND NOT start_terminal AND jsonb_array_length(start_reserved_windows)=4,(SELECT count(*) FROM phone_known_busy),(SELECT count(*) FROM idempotency_keys) FROM phone_verification WHERE id=$1`, a.Request.ID).Scan(&marker, &dispatchBound, &untouched, &busy, &cached); err != nil || marker != dispatched || dispatchBound != dispatched || !untouched || busy != 0 || cached != 0 {
				t.Fatal("refused phase published a marker, cache, release or busy fence", marker, dispatchBound, dispatched, untouched, busy, cached, err)
			}
		})
	}
}
