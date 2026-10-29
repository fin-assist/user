package postgres

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fin-assist/user/internal/adapter/verificationcrypto"
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

func TestIntegrationEvidenceCutoffPurgesResourcesWithoutAuthCleanup(t *testing.T) {
	db, _, _ := profileDB(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	repo := NewVerificationStarts(db)
	a := claimEvidence(t, db, repo, startKeyA, "+79001234567", now)
	live := claimEvidence(t, db, repo, startKeyB, "+79001234569", now)
	old := "019c7714-3b77-74d1-9866-000000000011"
	busy := "019c7714-3b77-74d1-9866-000000000012"
	reused := "019c7714-3b77-74d1-9866-000000000013"
	for _, key := range []string{old, busy, reused} {
		resource := a.Request.ID
		if key == reused {
			resource = live.Request.ID
		}
		if _, err := db.Primary().Exec(t.Context(), `INSERT INTO idempotency_keys(scope,endpoint,key,request_hash,expires_at,resource_type,resource_id) VALUES('00000000-0000-0000-0000-000000000000',$1,$2,$3,$4,'phone_verification',$5::integer::text)`, startPhoneVerificationEndpoint, key, a.Hash[:], now.Add(-time.Second), resource); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Primary().Exec(t.Context(), `UPDATE phone_verification SET expires_at=$2 WHERE id=$1`, a.Request.ID, now.Add(-25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Primary().Exec(t.Context(), `UPDATE phone_consent_receipt SET attempt_end=$2,retain_until=$3 WHERE attempt_id=$1`, a.Request.ID, now.Add(-25*time.Hour), now); err != nil {
		t.Fatal(err)
	}
	blocker, err := db.Primary().Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	if _, err = blocker.Exec(t.Context(), `SELECT 1 FROM idempotency_keys WHERE key=$1 FOR UPDATE`, old); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err = db.Run(ctx, func(ctx context.Context) error {
		_, e := NewVerifications(db).DeleteOldRequests(ctx, now, 1000)
		return e
	})
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || pg.Code != "55P03" {
		t.Fatal("cleanup waited for a key while holding its parent", err)
	}
	var proof, working int
	if err = db.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM phone_consent_receipt WHERE attempt_id=$1),(SELECT count(*) FROM phone_verification WHERE id=$1)`, a.Request.ID).Scan(&proof, &working); err != nil || proof != 1 || working != 1 {
		t.Fatal("partial cleanup committed before key purge", proof, working, err)
	}
	if err = blocker.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = db.Run(t.Context(), func(ctx context.Context) error {
		_, e := NewVerifications(db).DeleteOldRequests(ctx, now, 1000)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	var oldKeys, newKeys int
	if err = db.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM idempotency_keys WHERE key IN($1,$2)),(SELECT count(*) FROM idempotency_keys WHERE key=$3),(SELECT count(*) FROM phone_consent_receipt WHERE attempt_id=$4),(SELECT count(*) FROM phone_verification WHERE id=$4)`, old, busy, reused, a.Request.ID).Scan(&oldKeys, &newKeys, &proof, &working); err != nil || oldKeys != 0 || newKeys != 1 || proof != 0 || working != 0 {
		t.Fatal("cutoff left derived keys or erased a new resource", oldKeys, newKeys, proof, working, err)
	}
}

func TestIntegrationKnownBusyResourceSurvivesWorkingDeletionWithoutOwningForeignCall(t *testing.T) {
	db, _, _ := profileDB(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	repo := NewVerificationStarts(db)
	a := dispatchEvidence(t, db, repo, claimEvidence(t, db, repo, startKeyA, "+79001234567", now), now)
	a.Request.KnownBusyUntil = now.Add(time.Minute)
	if err := db.Run(t.Context(), func(ctx context.Context) error { return repo.Duplicate(ctx, a, now) }); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Primary().Exec(t.Context(), `DELETE FROM phone_verification WHERE id=$1`, a.Request.ID); err != nil {
		t.Fatal(err)
	}
	archived, err := repo.Find(t.Context(), a.Key)
	if err != nil || !archived.Request.Closed || !archived.Request.StartTerminal || archived.Hash != a.Hash {
		t.Fatal("working deletion lost original terminal-key protection", err)
	}

	if err := db.Run(t.Context(), func(ctx context.Context) error {
		id, until, e := repo.Busy(ctx, a.Request.Phone, now)
		if e == nil && (id != a.Request.ID || !until.Equal(a.Request.KnownBusyUntil)) {
			t.Fatal("busy resource lost its independent origin", id, until)
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	var bindings int
	if err := db.Primary().QueryRow(t.Context(), `SELECT count(*) FROM phone_provider_binding WHERE attempt_id=$1`, a.Request.ID).Scan(&bindings); err != nil || bindings != 0 {
		t.Fatal("known busy fact fabricated foreign-call ownership", bindings, err)
	}
	after := now.Add(time.Minute)
	if err := db.Run(t.Context(), func(ctx context.Context) error {
		_, until, e := repo.Busy(ctx, a.Request.Phone, after)
		if e == nil && !until.IsZero() {
			t.Fatal("finite busy fact extended")
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Run(t.Context(), func(ctx context.Context) error {
		_, e := NewVerifications(db).CleanupResults(ctx, after, 1000)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	var facts, proof int
	if err := db.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM phone_known_busy),(SELECT count(*) FROM phone_consent_receipt WHERE attempt_id=$1)`, a.Request.ID).Scan(&facts, &proof); err != nil || facts != 0 || proof != 1 {
		t.Fatal("busy expiry changed evidence retention", facts, proof, err)
	}
}

func TestIntegrationLinkedEvidenceKeepsLiveKeysThenCleansOnlyWorkingState(t *testing.T) {
	db, _, _ := profileDB(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	clock := func() time.Time { return now }
	starts := NewVerificationStarts(db).WithClock(clock)
	requests := NewVerifications(db)
	guard, err := tombstone.New(db, db.Primary(), clock)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := verificationcrypto.New(bytes.Repeat([]byte{42}, 32), "test")
	if err != nil {
		t.Fatal(err)
	}
	service := verification.New(db, requests, NewAuth(db), guard, phoneSigner{}, codec, noPhoneNotifications{}, clock, token.New, 60*24*time.Hour, time.Second)
	a := dispatchEvidence(t, db, starts, claimEvidence(t, db, starts, startKeyA, "+79001234567", now), now)
	call := verification.Call{ID: "linked-call", Phone: a.Request.Phone, StartedAt: now, State: verification.Pending, Numbers: []string{"+74951234567"}}
	if err := db.Run(t.Context(), func(ctx context.Context) error { return starts.Complete(ctx, a, call, token.Sum("linked-secret")) }); err != nil {
		t.Fatal(err)
	}
	if changed, err := service.Confirm(t.Context(), verification.Proof{Provider: "test", CallID: call.ID, Phone: a.Request.Phone}); err != nil || !changed {
		t.Fatal("real linked proof failed", changed, err)
	}
	now = now.Add(25 * time.Hour)
	const key = "019c7714-3b77-74d1-9866-000000000021"
	if _, err := db.Primary().Exec(t.Context(), `INSERT INTO idempotency_keys(scope,endpoint,key,request_hash,expires_at,resource_type,resource_id) VALUES('00000000-0000-0000-0000-000000000000','/finassist.user.v1.AuthService/Register',$1,$2,$3,'phone_verification',$4::integer::text)`, key, a.Hash[:], now.Add(time.Minute), a.Request.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Cleanup(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	var working int
	if err := db.Primary().QueryRow(t.Context(), `SELECT count(*) FROM phone_verification WHERE id=$1`, a.Request.ID).Scan(&working); err != nil || working != 1 {
		t.Fatal("live linked key lost working parent before its own lifetime", working, err)
	}
	if _, err := db.Primary().Exec(t.Context(), `UPDATE idempotency_keys SET expires_at=$2 WHERE key=$1`, key, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Cleanup(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	var proof, keys, sessions, unlinked int
	if err := db.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM phone_verification WHERE id=$1),(SELECT count(*) FROM phone_consent_receipt WHERE attempt_id=$1 AND linked_user_id=$2::uuid AND retain_until IS NULL),(SELECT count(*) FROM idempotency_keys WHERE key=$3),(SELECT count(*) FROM auth_session WHERE user_id=$2::uuid),(SELECT count(*) FROM auth_session WHERE user_id=$2::uuid AND phone_verification_id IS NULL)`, a.Request.ID, profileID, key).Scan(&working, &proof, &keys, &sessions, &unlinked); err != nil || working != 0 || proof != 1 || keys != 0 || sessions != 1 || unlinked != 1 {
		t.Fatal("working cleanup changed account proof/session", working, proof, keys, sessions, unlinked, err)
	}
}
