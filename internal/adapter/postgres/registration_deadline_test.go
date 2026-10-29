package postgres

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/verification"
)

func assertDeadlineAttemptUnpublished(t *testing.T, db *DB, a verification.StartRecord) {
	t.Helper()
	var tokenHash, requestHash, receiptHash []byte
	var key string
	var noCall, noResult bool
	var bindings, sessions int
	var created, expiry, unknown, accepted, age, end, retained time.Time
	err := db.Primary().QueryRow(t.Context(), `SELECT v.token_hash,v.start_request_hash,v.start_key::text,v.provider_call_id IS NULL,v.result_ciphertext IS NULL,v.created_at,v.expires_at,v.unknown_deadline,e.request_hash,e.consent_accepted_at,e.age_confirmed_at,e.attempt_end,e.retain_until,(SELECT count(*) FROM phone_provider_binding WHERE attempt_id=v.id),(SELECT count(*) FROM auth_session WHERE phone_verification_id=v.id) FROM phone_verification v JOIN phone_consent_receipt e ON e.attempt_id=v.consent_receipt_id WHERE v.id=$1`, a.Request.ID).Scan(&tokenHash, &requestHash, &key, &noCall, &noResult, &created, &expiry, &unknown, &receiptHash, &accepted, &age, &end, &retained, &bindings, &sessions)
	if err != nil || !noCall || !noResult || bindings != 0 || sessions != 0 || key != a.Key || !bytes.Equal(tokenHash, a.Request.Hash[:]) || !bytes.Equal(requestHash, a.Hash[:]) || !bytes.Equal(receiptHash, a.Hash[:]) || !created.Equal(a.Request.CreatedAt) || !expiry.Equal(a.Request.UnknownDeadline) || !unknown.Equal(a.Request.UnknownDeadline) || !accepted.Equal(a.AcceptedAt) || !age.Equal(a.AcceptedAt) || !end.Equal(a.Request.UnknownDeadline) || !retained.Equal(a.Request.UnknownDeadline.Add(24*time.Hour)) {
		t.Fatalf("expired completion published state or rewrote original receipt anchors: callAbsent=%t resultAbsent=%t bindings=%d sessions=%d err=%v", noCall, noResult, bindings, sessions, err)
	}
}

func assertDeadlineCompletionError(t *testing.T, err error, unknown bool, call verification.Call) {
	t.Helper()
	if unknown {
		if !errors.Is(err, verification.ErrMissing) || errors.Is(err, verification.ErrProviderDuplicate) {
			t.Fatal("unknown deadline did not win over candidate expiry", err)
		}
		return
	}
	var duplicate *verification.ProviderDuplicateError
	if !errors.As(err, &duplicate) || !duplicate.BusyUntil.Equal(call.BusyUntil()) {
		t.Fatal("public deadline did not preserve the independently known busy bound", err)
	}
}

func TestIntegrationCompletionRechecksDeadlinesAfterPostgresWaits(t *testing.T) {
	for _, stage := range []string{"own-row", "binding-unique"} {
		for _, deadline := range []string{"unknown", "public"} {
			t.Run(stage+"/"+deadline, func(t *testing.T) {
				db, _, _ := profileDB(t)
				base := time.Now().UTC().Truncate(time.Microsecond)
				var clock atomic.Int64
				clock.Store(base.UnixNano())
				repo := NewVerificationStarts(db).WithClock(func() time.Time { return time.Unix(0, clock.Load()).UTC() })
				a := dispatchEvidence(t, db, repo, claimEvidence(t, db, repo, startKeyA, "+79001234567", base), base)
				call := verification.Call{ID: "deadline-candidate", Phone: a.Request.Phone, Campaign: a.Request.Campaign, StartedAt: base, ReceivedAt: base, State: verification.Pending, Numbers: []string{"+74951234567"}, ClockSkewTolerance: time.Second}
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				blocker, err := db.Primary().Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = blocker.Rollback(context.Background()) }()
				queryFragment := "%phone_verification%"
				if stage == "own-row" {
					_, err = blocker.Exec(ctx, `SELECT id FROM phone_verification WHERE id=$1 FOR UPDATE`, a.Request.ID)
				} else {
					queryFragment = "%INSERT INTO phone_provider_binding%"
					_, err = blocker.Exec(ctx, `INSERT INTO phone_provider_binding(provider,campaign,call_id,attempt_id,provider_created_at,declared_expiry,public_deadline,busy_until) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, a.Request.Provider, call.Campaign, call.ID, a.Request.ID, call.StartedAt, call.StartedAt.Add(verification.RequestTTL), call.PublicExpiry(), call.BusyUntil())
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
						return repo.Complete(tx, a, call, token.Sum("provisional-deadline-token"))
					})
				}()
				var pid int32
				select {
				case pid = <-ready:
				case err := <-done:
					t.Fatal("completion exited before its database barrier", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				waitPhoneCondition(t, func() bool {
					var waiting bool
					if err := db.Primary().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock' AND query LIKE $2)`, pid, queryFragment).Scan(&waiting); err != nil {
						return false
					}
					return waiting
				})
				// The clock advances only after PostgreSQL proves this exact backend
				// is waiting. Releasing the blocker must not validate a stale instant.
				at := call.PublicExpiry()
				if deadline == "unknown" {
					at = a.Request.UnknownDeadline
				}
				clock.Store(at.UnixNano())
				if err := blocker.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					assertDeadlineCompletionError(t, err, deadline == "unknown", call)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				assertDeadlineAttemptUnpublished(t, db, a)
			})
		}
	}
}

func TestIntegrationFinalizeKeyRechecksDeadlineAfterSelectorDelay(t *testing.T) {
	for _, deadline := range []string{"unknown", "public"} {
		t.Run(deadline, func(t *testing.T) {
			db, _, _ := profileDB(t)
			base := time.Now().UTC().Truncate(time.Microsecond)
			var clock atomic.Int64
			clock.Store(base.UnixNano())
			repo := NewVerificationStarts(db).WithClock(func() time.Time { return time.Unix(0, clock.Load()).UTC() })
			a := dispatchEvidence(t, db, repo, claimEvidence(t, db, repo, startKeyA, "+79001234567", base), base)
			call := verification.Call{ID: "finalizer-candidate", Phone: a.Request.Phone, Campaign: a.Request.Campaign, StartedAt: base, ReceivedAt: base, State: verification.Pending, Numbers: []string{"+74951234567"}, ClockSkewTolerance: time.Second}
			err := db.Run(t.Context(), func(ctx context.Context) error {
				if err := repo.Complete(ctx, a, call, token.Sum("provisional-finalizer-token")); err != nil {
					return err
				}
				// Model a selector/cache write which waited after Complete succeeded.
				at := call.PublicExpiry()
				if deadline == "unknown" {
					at = a.Request.UnknownDeadline
				}
				clock.Store(at.UnixNano())
				return repo.FinalizeKey(ctx, a)
			})
			assertDeadlineCompletionError(t, err, deadline == "unknown", call)
			assertDeadlineAttemptUnpublished(t, db, a)
		})
	}
}
