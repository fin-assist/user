package apitest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	outboxlib "github.com/fin-assist/go-outbox"

	"github.com/fin-assist/user/internal/adapter/outbox"
	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/auth"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/deletion"
	"github.com/fin-assist/user/internal/profile"
	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/verification"
)

// These tests call usecases with real repositories and transactions, never gRPC handlers.
func TestIntegrationAccountLifecycleUsesOneTransactionForConsentSessionAndEvents(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	publisher, err := outboxlib.NewPublisher(env.App.DB, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	events := outbox.NewEvents(publisher)
	sessions := auth.New(env.App.DB, postgres.NewAuth(env.App.DB), env.App.Tombstones, env.App.Signing, events, time.Now, token.New, time.Hour)
	seedRegistration(t, env, "integration-registration", "+79001234567", time.Now().Add(-time.Minute))
	in := auth.RegisterInput{Token: "integration-registration", Accepted: true, ConsentVersion: "v1"}
	bad := in
	bad.Accepted = false
	if _, err := sessions.Register(t.Context(), bad); apitest.Reason(err) != "CONSENT_REQUIRED" {
		t.Fatalf("missing consent: %v", err)
	}
	bad = in
	bad.ConsentVersion = "old"
	if _, err := sessions.Register(t.Context(), bad); apitest.Reason(err) != "CONSENT_OUTDATED" {
		t.Fatalf("old consent: %v", err)
	}
	registered, err := sessions.Register(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Register(t.Context(), in); apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatalf("consumed token reused: %v", err)
	}
	checkAccessJWT(t, env, registered.Pair.Access, registered.UserID)
	profiles := profile.New(postgres.NewProfileTransactions(env.App.DB), postgres.NewProfiles(env.App.DB), env.App.Tombstones, events, time.Now)
	p, err := profiles.Get(t.Context(), registered.UserID)
	if err != nil || p.Timezone != "Europe/Moscow" {
		t.Fatalf("initial profile: %+v %v", p, err)
	}
	zone, theme := "Asia/Tokyo", "dark"
	p, err = profiles.Update(t.Context(), registered.UserID, profile.Patch{Timezone: &zone, Theme: &theme})
	if err != nil || p.Timezone != zone || p.Theme != theme {
		t.Fatalf("update: %+v %v", p, err)
	}
	if _, err = profiles.Grant(t.Context(), registered.UserID, "v2"); err != nil {
		t.Fatal(err)
	}
	if c, err := profiles.Status(t.Context(), registered.UserID); err != nil || !c.Granted {
		t.Fatalf("grant: %+v %v", c, err)
	}
	if err = profiles.Revoke(t.Context(), registered.UserID); err != nil {
		t.Fatal(err)
	}
	if c, err := profiles.Status(t.Context(), registered.UserID); err != nil || c.Granted {
		t.Fatalf("revoke: %+v %v", c, err)
	}
	rotated, err := sessions.Refresh(t.Context(), registered.Pair.Refresh)
	if err != nil || rotated.UserID != registered.UserID {
		t.Fatalf("refresh: %+v %v", rotated, err)
	}
	if reused, err := sessions.Refresh(t.Context(), registered.Pair.Refresh); apitest.Reason(err) != "TOKEN_REVOKED" || !reused.Reused {
		t.Fatalf("reuse: %+v %v", reused, err)
	}
	if err = sessions.CompleteReuse(t.Context(), registered.Pair.Refresh); err != nil {
		t.Fatal(err)
	}
	if _, err = sessions.Refresh(t.Context(), rotated.Pair.Refresh); apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatalf("chain not revoked: %v", err)
	}
	if err = sessions.Logout(t.Context(), registered.UserID, rotated.Pair.Refresh); err != nil {
		t.Fatal(err)
	}
	accounts := deletion.New(env.App.DB, env.App.Tombstones, postgres.NewAccounts(env.App.DB), events)
	if err = accounts.Delete(t.Context(), registered.UserID); err != nil {
		t.Fatal(err)
	}
	if err = accounts.Delete(t.Context(), registered.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err = profiles.Get(t.Context(), registered.UserID); apitest.Reason(err) != "NOT_FOUND" {
		t.Fatalf("deleted profile read: %v", err)
	}
	var users, sessionsCount, consents int
	err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM app_user),(SELECT count(*) FROM auth_session),(SELECT count(*) FROM user_consent)`).Scan(&users, &sessionsCount, &consents)
	if err != nil || users != 0 || sessionsCount != 0 || consents != 0 {
		t.Fatalf("personal data retained: %d %d %d %v", users, sessionsCount, consents, err)
	}
}

func TestIntegrationVerificationCommitsProofAndReplayAcrossSnapshots(t *testing.T) {
	env := apitest.Start(t)
	raw, phone := "integration-watch", "+79001234567"
	pendingPhoneAPI(t, env, raw, phone, time.Now().Add(time.Minute))
	_, out, err := env.App.Verifications.Snapshot(t.Context(), raw)
	if err != nil || out.State != verification.Pending {
		t.Fatalf("pending: %+v %v", out, err)
	}
	// An invalid proof must not change the stored pending request.
	if _, err = env.App.Verifications.Confirm(t.Context(), verification.Proof{Provider: "test", CallID: "call:" + raw, Phone: "+79001234568"}); err == nil {
		t.Fatal("foreign phone proof accepted")
	}
	changed, err := env.App.Verifications.Confirm(t.Context(), verification.Proof{Provider: "test", CallID: "call:" + raw, Phone: phone})
	if err != nil || !changed {
		t.Fatalf("confirmation: %v %v", changed, err)
	}
	if changed, err = env.App.Verifications.Confirm(t.Context(), verification.Proof{Provider: "test", CallID: "call:" + raw, Phone: phone}); err != nil || changed {
		t.Fatalf("replayed proof: %v %v", changed, err)
	}
	var received []verification.Outcome
	err = env.App.Verifications.Watch(t.Context(), raw, func(o verification.Outcome) error { received = append(received, o); return nil })
	if err != nil || len(received) != 1 || !received[0].IsNewUser || received[0].RegistrationToken == "" {
		t.Fatalf("replayed outcome: %+v %v", received, err)
	}
	_, again, err := env.App.Verifications.Snapshot(context.Background(), raw)
	if err != nil || again.RegistrationToken != received[0].RegistrationToken {
		t.Fatalf("immutable registration result: %+v %v", again, err)
	}
}

func TestIntegrationWatchDeliversCommittedExistingSessionThroughPostgresNotification(t *testing.T) {
	env := apitest.Start(t)
	seedProfiles(t, env)
	phoneJob(t, env)
	raw, phone := "integration-notified-existing", "+79001234567"
	pendingPhoneAPI(t, env, raw, phone, time.Now().Add(time.Minute))
	pending := make(chan struct{})
	var outcomes []verification.Outcome
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go func() {
		done <- env.App.Verifications.Watch(ctx, raw, func(o verification.Outcome) error {
			outcomes = append(outcomes, o)
			if o.State == verification.Pending {
				close(pending)
			}
			return nil
		})
	}()
	select {
	case <-pending:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	changed, err := env.App.Verifications.Confirm(ctx, verification.Proof{Provider: "test", CallID: "call:" + raw, Phone: phone})
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 2 || outcomes[1].IsNewUser || outcomes[1].UserID != profileUserA || outcomes[1].Pair.Refresh == "" {
		t.Fatal(outcomes)
	}
	checkAccessJWT(t, env, outcomes[1].Pair.Access, profileUserA)
	var count int
	if err := env.App.DB.Primary().QueryRow(ctx, `SELECT count(*) FROM auth_session WHERE user_id=$1`, profileUserA).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if err := env.App.Verifications.Watch(ctx, raw, func(verification.Outcome) error { return context.Canceled }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := env.App.DB.Primary().Exec(ctx, `UPDATE phone_verification SET closed_at=now() WHERE token_hash=$1`, tokenHash(raw)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := env.App.Verifications.Snapshot(ctx, raw); apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatal(err)
	}
}

func tokenHash(raw string) []byte { h := token.Sum(raw); return h[:] }

func TestIntegrationVerificationRejectsCorruptedStoredResultsAndExpiredProofs(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"missing-key", `UPDATE phone_verification SET result_key_id=NULL`},
		{"unknown-key", `UPDATE phone_verification SET result_key_id='retired'`},
		{"bad-ciphertext", `UPDATE phone_verification SET result_ciphertext='\\x01'`},
		{"bad-registration-hash", `UPDATE phone_verification SET registration_token_hash=decode(repeat('00',32),'hex')`},
		{"bad-registration-shape", `UPDATE phone_verification SET registration_token_hash=decode('00','hex')`},
		{"bad-token-hash", `UPDATE phone_verification SET token_hash=decode('00','hex')`},
		{"expired-result", `UPDATE phone_verification SET verified_at=now()-interval '16 minutes'`},
		{"wrong-result-kind", `UPDATE phone_verification SET is_new_user=false`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := apitest.Start(t)
			raw, phone := "integration-corruption", "+79001234567"
			pendingPhoneAPI(t, env, raw, phone, time.Now().Add(time.Minute))
			confirmPhoneAPI(t, env, raw, phone)
			if _, err := env.App.DB.Primary().Exec(t.Context(), tc.sql); err != nil {
				t.Fatal(err)
			}
			if _, _, err := env.App.Verifications.Snapshot(t.Context(), raw); err == nil {
				t.Fatal("corrupted result delivered")
			}
		})
	}
	env := apitest.Start(t)
	if _, _, err := env.App.Verifications.Snapshot(t.Context(), "missing"); apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatal(err)
	}
	for _, proof := range []verification.Proof{{Provider: "bad name", CallID: "call", Phone: "+79001234567"}, {Provider: "test", Phone: "+79001234567"}, {Provider: "test", CallID: "call", Phone: "+7900x234567"}} {
		if changed, err := env.App.Verifications.Confirm(t.Context(), proof); changed || !errors.Is(err, verification.ErrProof) {
			t.Fatal(changed, err)
		}
	}
	raw, phone := "integration-expired", "+79001234567"
	pendingPhoneAPI(t, env, raw, phone, time.Now().Add(-time.Minute))
	if changed, err := env.App.Verifications.Confirm(t.Context(), verification.Proof{Provider: "test", CallID: "call:" + raw, Phone: phone}); changed || err != nil {
		t.Fatal(changed, err)
	}
	if _, _, err := env.App.Verifications.Snapshot(t.Context(), raw); apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatal(err)
	}
}

func TestIntegrationPendingWatchCancellationExpiryAndDeliveryFailureReleaseSlots(t *testing.T) {
	env := apitest.Start(t, func(c *config.Config) { c.Phone.MaxStreams = 1; c.Phone.ResyncInterval = 20 * time.Millisecond })
	phoneJob(t, env)
	raw, phone := "integration-expiring-watch", "+79001234567"
	pendingPhoneAPI(t, env, raw, phone, time.Now().Add(3*time.Second))
	var states []verification.State
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := env.App.Verifications.Watch(ctx, raw, func(o verification.Outcome) error { states = append(states, o.State); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(states) != 2 || states[0] != verification.Pending || states[1] != verification.Expired {
		t.Fatal(states)
	}
	raw = "integration-cancel-watch"
	pendingPhoneAPI(t, env, raw, "+79001234568", time.Now().Add(time.Minute))
	cancelled, stop := context.WithCancel(ctx)
	if err := env.App.Verifications.Watch(cancelled, raw, func(verification.Outcome) error { stop(); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := env.App.Verifications.Watch(ctx, raw, func(verification.Outcome) error { return context.Canceled }); !errors.Is(err, context.Canceled) {
		t.Fatal("failed delivery did not release slot", err)
	}
	// A confirmed result is replayable independently of its creation TTL.
	if changed, err := env.App.Verifications.Confirm(ctx, verification.Proof{Provider: "test", CallID: "call:" + raw, Phone: "+79001234568"}); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if err := env.App.Verifications.Watch(ctx, raw, func(o verification.Outcome) error {
		if o.State != verification.Verified {
			t.Fatal(o)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationConcurrentRegistrationProofsPreserveExistingAccountSettings(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	publisher, err := outboxlib.NewPublisher(env.App.DB, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	sessions := auth.New(env.App.DB, postgres.NewAuth(env.App.DB), env.App.Tombstones, env.App.Signing, outbox.NewEvents(publisher), time.Now, token.New, time.Hour)
	for _, raw := range []string{"first-independent-proof", "second-independent-proof"} {
		seedRegistration(t, env, raw, "+79001234567", time.Now().Add(-time.Minute))
	}
	firstZone, secondZone := "Asia/Tokyo", "Europe/London"
	first, err := sessions.Register(t.Context(), auth.RegisterInput{Token: "first-independent-proof", Accepted: true, ConsentVersion: "v1", Timezone: &firstZone})
	if err != nil {
		t.Fatal(err)
	}
	second, err := sessions.Register(t.Context(), auth.RegisterInput{Token: "second-independent-proof", Accepted: true, ConsentVersion: "v1", Timezone: &secondZone})
	if err != nil || second.UserID != first.UserID {
		t.Fatal(second, err)
	}
	var zone string
	var consents, events int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT timezone,(SELECT count(*) FROM user_consent),(SELECT count(*) FROM outbox_events) FROM app_user`).Scan(&zone, &consents, &events); err != nil || zone != firstZone || consents != 1 || events != 1 {
		t.Fatal(zone, consents, events, err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE refresh_token SET generation=0 WHERE token_hash=$1`, tokenHash(second.Pair.Refresh)); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Refresh(t.Context(), second.Pair.Refresh); err == nil {
		t.Fatal("invalid persisted generation accepted")
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE refresh_token SET generation=1,issued_at=now()+interval '1 minute' WHERE token_hash=$1`, tokenHash(second.Pair.Refresh)); err != nil {
		t.Fatal(err)
	}
	rotated, err := sessions.Refresh(t.Context(), second.Pair.Refresh)
	if err != nil || rotated.Pair.Refresh == "" {
		t.Fatal(rotated, err)
	}
	if err := sessions.CompleteReuse(t.Context(), "missing-chain"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.CompleteReuse(t.Context(), rotated.Pair.Refresh); err != nil {
		t.Fatal(err)
	}
	// A storage error after creating the user must roll the account, consent, event
	// and one-time proof consumption back together.
	seedRegistration(t, env, "rollback-independent-proof", "+79001234568", time.Now().Add(-time.Minute))
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE auth_session RENAME COLUMN created_at TO unavailable`); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Register(t.Context(), auth.RegisterInput{Token: "rollback-independent-proof", Accepted: true, ConsentVersion: "v1"}); err == nil {
		t.Fatal("unavailable session storage accepted")
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE auth_session RENAME COLUMN unavailable TO created_at`); err != nil {
		t.Fatal(err)
	}
	var users int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM app_user`).Scan(&users); err != nil || users != 1 {
		t.Fatal(users, err)
	}
	if _, err := sessions.Register(t.Context(), auth.RegisterInput{Token: "rollback-independent-proof", Accepted: true, ConsentVersion: "v1"}); err != nil {
		t.Fatal("retry after rollback", err)
	}
}

func TestIntegrationRegistrationChecksStoredProofAndCurrentConsentBeforeWrites(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	publisher, err := outboxlib.NewPublisher(env.App.DB, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	sessions := auth.New(env.App.DB, postgres.NewAuth(env.App.DB), env.App.Tombstones, env.App.Signing, outbox.NewEvents(publisher), time.Now, token.New, time.Hour)
	seedRegistration(t, env, "guarded-registration", "+79001234567", time.Now().Add(-time.Minute))
	in := auth.RegisterInput{Token: "guarded-registration", Accepted: true, ConsentVersion: "v1"}
	bad := in
	zone := "Invalid/Timezone"
	bad.Timezone = &zone
	if _, err := sessions.Register(t.Context(), bad); apitest.Reason(err) != "VALIDATION_ERROR" {
		t.Fatal(err)
	}
	for _, tc := range []struct{ sql, reason string }{
		{`UPDATE phone_verification SET verified_at=now()+interval '1 minute'`, ""},
		{`UPDATE phone_verification SET verified_at=now()-interval '16 minutes'`, "TOKEN_EXPIRED"},
		{`UPDATE phone_verification SET verified_at=NULL`, "INVALID_TOKEN"},
	} {
		if _, err := env.App.DB.Primary().Exec(t.Context(), tc.sql); err != nil {
			t.Fatal(err)
		}
		if _, err := sessions.Register(t.Context(), in); err == nil || apitest.Reason(err) != tc.reason {
			t.Fatal(tc, err)
		}
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE phone_verification SET verified_at=now();DELETE FROM consent_document WHERE type='personal_data'`); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Register(t.Context(), in); err == nil {
		t.Fatal("unpublished personal-data consent accepted")
	}
	var users int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM app_user`).Scan(&users); err != nil || users != 0 {
		t.Fatal(users, err)
	}
}
