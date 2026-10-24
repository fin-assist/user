package apitest_test

import (
	"context"
	"testing"
	"time"

	outboxlib "github.com/fin-assist/go-outbox"

	"github.com/fin-assist/user/internal/adapter/outbox"
	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/auth"
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
	profiles := profile.New(env.App.DB, postgres.NewProfiles(env.App.DB), env.App.Tombstones, events, time.Now)
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
	if err = sessions.Logout(t.Context(), rotated.Pair.Refresh); err != nil {
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
