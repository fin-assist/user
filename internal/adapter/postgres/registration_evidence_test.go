package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/verification"
	"github.com/fin-assist/user/migrations"
)

func claimEvidence(t *testing.T, db *DB, repo *VerificationStarts, key, phone string, now time.Time) verification.StartRecord {
	t.Helper()
	v := startRecord(key, phone, now)
	v.Request.Hash = token.Sum(key + phone + now.String())
	if err := db.Run(t.Context(), func(ctx context.Context) error { var err error; v, _, err = repo.Claim(ctx, v); return err }); err != nil {
		t.Fatal(err)
	}
	return v
}
func dispatchEvidence(t *testing.T, db *DB, repo *VerificationStarts, v verification.StartRecord, now time.Time) verification.StartRecord {
	t.Helper()
	if err := db.Run(t.Context(), func(ctx context.Context) error {
		var err error
		var first bool
		v, first, err = repo.Dispatch(ctx, v, now)
		if err == nil && !first {
			t.Fatal("first marker not acquired")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestIntegrationEvidenceDispatchDeadlineReleaseAndKeyWindow(t *testing.T) {
	db, _, _ := profileDB(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	repo := NewVerificationStarts(db).WithClock(func() time.Time { return now })
	a := claimEvidence(t, db, repo, startKeyA, "+79001234567", now)
	a = dispatchEvidence(t, db, repo, a, now.Add(time.Second))
	var again verification.StartRecord
	if err := db.Run(t.Context(), func(ctx context.Context) error {
		var first bool
		var e error
		again, first, e = repo.Dispatch(ctx, a, now.Add(2*time.Second))
		if first {
			t.Fatal("dispatch marker acquired twice")
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if !again.Request.FirstDispatchAt.Equal(a.Request.FirstDispatchAt) || !again.Request.DispatchNotAfter.Equal(a.Request.UnknownDeadline) {
		t.Fatal("dispatch times changed")
	}
	now = now.Add(verification.RequestTTL)
	if err := db.Run(t.Context(), func(ctx context.Context) error {
		_, busy, e := repo.Busy(ctx, a.Request.Phone, now)
		if !busy.IsZero() {
			t.Fatal("unknown phone stayed held")
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	old, e := repo.Find(t.Context(), a.Key)
	if e != nil || !old.Request.Closed || !old.Request.StartTerminal {
		t.Fatal("terminal marker lost", e)
	}
	if err := db.Run(t.Context(), func(ctx context.Context) error {
		return repo.Complete(ctx, a, verification.Call{ID: "late", Phone: a.Request.Phone, StartedAt: now, State: verification.Pending}, token.Sum("late"))
	}); !errors.Is(err, verification.ErrMissing) {
		t.Fatal("late completion reopened", err)
	}
	b := claimEvidence(t, db, repo, startKeyB, a.Request.Phone, now)
	if b.Request.ID == a.Request.ID {
		t.Fatal("old attempt revived")
	}
	var receipts int
	if err := db.Primary().QueryRow(t.Context(), `SELECT count(*) FROM phone_consent_receipt WHERE phone=$1`, a.Request.Phone).Scan(&receipts); err != nil || receipts != 2 {
		t.Fatal("receipt lost on phone release", receipts, err)
	}
	// The original UUID becomes fresh exactly after its ordinary 24h window,
	// even though evidence physically survives until deadline + 24h.
	now = a.KeyExpiresAt
	if _, e := repo.Find(t.Context(), a.Key); !errors.Is(e, verification.ErrMissing) {
		t.Fatal("archive extended key window", e)
	}
	c := claimEvidence(t, db, repo, a.Key, "+79001234569", now)
	if c.Request.ID == a.Request.ID {
		t.Fatal("old key resurrected original request")
	}
}

func TestIntegrationEvidenceBindingUniqueAcrossLiveAndRetainedArchive(t *testing.T) {
	for _, archive := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "archive"}[archive], func(t *testing.T) {
			db, _, _ := profileDB(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			repo := NewVerificationStarts(db).WithClock(func() time.Time { return now })
			a := dispatchEvidence(t, db, repo, claimEvidence(t, db, repo, startKeyA, "+79001234567", now), now)
			call := verification.Call{ID: "same-provider-id", Phone: a.Request.Phone, StartedAt: now, State: verification.Pending, Numbers: []string{"+74951234567"}, ClockSkewTolerance: time.Second}
			if err := db.Run(t.Context(), func(ctx context.Context) error { return repo.Complete(ctx, a, call, token.Sum("first")) }); err != nil {
				t.Fatal(err)
			}
			if archive {
				if _, err := db.Primary().Exec(t.Context(), `DELETE FROM phone_verification WHERE id=$1`, a.Request.ID); err != nil {
					t.Fatal(err)
				}
			}
			b := dispatchEvidence(t, db, repo, claimEvidence(t, db, repo, startKeyB, "+79001234569", now), now)
			call.Phone = b.Request.Phone
			err := db.Run(t.Context(), func(ctx context.Context) error { return repo.Complete(ctx, b, call, token.Sum("other")) })
			var duplicate *verification.ProviderDuplicateError
			if !errors.As(err, &duplicate) || !duplicate.BusyUntil.Equal(now.Add(5*time.Minute+time.Second)) {
				t.Fatal("collision not authoritative duplicate", err)
			}
			var id int32
			var calls int
			if err := db.Primary().QueryRow(t.Context(), `SELECT (SELECT attempt_id FROM phone_provider_binding WHERE call_id=$1),(SELECT count(*) FROM phone_verification WHERE provider_call_id=$1)`, call.ID).Scan(&id, &calls); err != nil || id != a.Request.ID || calls != map[bool]int{false: 1, true: 0}[archive] {
				t.Fatal("binding stolen or rollback leaked", id, calls, err)
			}
		})
	}
}

func TestIntegrationEvidenceRetentionSurvivesAccountAndWorkingDeletion(t *testing.T) {
	db, service, _, now := phoneState(t)
	repo := NewVerificationStarts(db).WithClock(func() time.Time { return now })
	a := claimEvidence(t, db, repo, startKeyA, "+79001234567", now)
	b := claimEvidence(t, db, repo, startKeyB, "+79001234569", now)
	if err := db.Run(t.Context(), func(ctx context.Context) error { return linkPhoneReceipt(ctx, db, a.Request.ID, profileID, now) }); err != nil {
		t.Fatal(err)
	}
	account := NewAccounts(db)
	account.now = func() time.Time { return now }
	if err := db.Run(t.Context(), func(ctx context.Context) error { return account.Remove(ctx, profileID) }); err != nil {
		t.Fatal(err)
	}
	var until time.Time
	var accepted time.Time
	var owner string
	if err := db.Primary().QueryRow(t.Context(), `SELECT retain_until,consent_accepted_at,linked_user_id::text FROM phone_consent_receipt WHERE attempt_id=$1`, a.Request.ID).Scan(&until, &accepted, &owner); err != nil || !until.Equal(now.AddDate(3, 0, 0)) || !accepted.Equal(now) || owner != profileID {
		t.Fatal("linked proof lost/extended", until, accepted, owner, err)
	}
	// Cleanup's anchored clock is advanced by constructing the same service with a new clock.
	_ = service
	cleanup := NewVerifications(db)
	cut := b.Request.ExpiresAt.Add(24 * time.Hour)
	for _, at := range []time.Time{cut.Add(-time.Microsecond), cut} {
		if err := db.Run(t.Context(), func(ctx context.Context) error { _, err := cleanup.DeleteOldRequests(ctx, at, 1000); return err }); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := db.Primary().QueryRow(t.Context(), `SELECT count(*) FROM phone_consent_receipt WHERE attempt_id=$1`, b.Request.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		want := 1
		if at.Equal(cut) {
			want = 0
		}
		if count != want {
			t.Fatal("abandoned retention boundary", count, want)
		}
	}
	var count int
	if err := db.Primary().QueryRow(t.Context(), `SELECT count(*) FROM phone_consent_receipt WHERE attempt_id=$1`, a.Request.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("another live retention was erased", count, err)
	}
}

func TestIntegrationLateDuplicateCannotFenceNewClaimOrRefundOriginal(t *testing.T) {
	db, _, _ := profileDB(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	repo := NewVerificationStarts(db).WithClock(func() time.Time { return now })
	a := dispatchEvidence(t, db, repo, claimEvidence(t, db, repo, startKeyA, "+79001234567", now), now)
	now = now.Add(verification.RequestTTL)
	if err := db.Run(t.Context(), func(ctx context.Context) error { _, _, e := repo.Busy(ctx, a.Request.Phone, now); return e }); err != nil {
		t.Fatal(err)
	}
	b := claimEvidence(t, db, repo, startKeyB, a.Request.Phone, now)
	a.Request.KnownBusyUntil = now.Add(verification.RequestTTL)
	for _, late := range []func(context.Context) error{func(ctx context.Context) error { return repo.Duplicate(ctx, a, now) }, func(ctx context.Context) error { return repo.Reject(ctx, a, now) }} {
		if err := db.Run(t.Context(), late); !errors.Is(err, verification.ErrMissing) {
			t.Fatal("late executor changed terminal state", err)
		}
	}
	var holder int32
	var facts int
	if err := db.Primary().QueryRow(t.Context(), `SELECT (SELECT id FROM phone_verification WHERE phone=$1 AND released_at IS NULL),(SELECT count(*) FROM phone_known_busy WHERE phone=$1)`, a.Request.Phone).Scan(&holder, &facts); err != nil || holder != b.Request.ID || facts != 0 {
		t.Fatal("late response disturbed new attempt", holder, facts, err)
	}
}

func TestIntegrationEvidenceMigrationRollbackRefusesRetainedReceipts(t *testing.T) {
	db, _, _ := profileDB(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	repo := NewVerificationStarts(db)
	a := claimEvidence(t, db, repo, startKeyA, "+79001234567", now)
	sql, err := migrations.FS.ReadFile("000005_registration_evidence.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Primary().Exec(t.Context(), string(sql)); err == nil {
		t.Fatal("down erased retained evidence")
	}
	var count int
	if err := db.Primary().QueryRow(t.Context(), `SELECT count(*) FROM phone_consent_receipt WHERE attempt_id=$1`, a.Request.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("failed rollback lost proof", count, err)
	}
	if _, err = repo.Find(t.Context(), a.Key); err != nil {
		t.Fatal("failed rollback changed working schema", err)
	}
}
