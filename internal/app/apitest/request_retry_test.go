package apitest_test

import (
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"

	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/ratelimit"
)

func assertRequestPermission(t *testing.T, err error, want bool) {
	t.Helper()
	r, ok := finerr.ToREST(err)
	if !ok || r.RequestRetryable == nil || *r.RequestRetryable != want {
		t.Fatal("wrong request advice", r, err)
	}
}
func TestAPIOriginalRequestPermissionIsPhaseOwned(t *testing.T) {
	env := apitest.Start(t, func(c *config.Config) {
		c.Limits[ratelimit.RegisterIP] = ratelimit.Rule{Limit: 1, Window: time.Hour}
		c.Limits[ratelimit.RefreshIP] = ratelimit.Rule{Limit: 1, Window: time.Hour}
		c.Limits[ratelimit.WatchIP] = ratelimit.Rule{Limit: 1, Window: time.Hour}
	})
	seedProfiles(t, env)
	auth := userv1.NewAuthServiceClient(env.Conn)
	_, err := auth.Register(authContext(t.Context(), authKey()), registrationRequest("invalid"))
	assertRequestPermission(t, err, false)
	_, err = auth.Register(authContext(t.Context(), authKey()), registrationRequest("invalid"))
	assertRequestPermission(t, err, true)
	_, err = auth.RefreshTokens(authContext(t.Context(), authKey()), &userv1.RefreshTokensRequest{RefreshToken: "invalid"})
	assertRequestPermission(t, err, false)
	_, err = auth.RefreshTokens(authContext(t.Context(), authKey()), &userv1.RefreshTokensRequest{RefreshToken: "invalid"})
	assertRequestPermission(t, err, true)
	for i := 0; i < 2; i++ {
		stream, err := auth.WatchPhoneVerification(authContext(t.Context(), authKey()), &userv1.WatchPhoneVerificationRequest{VerificationToken: "invalid"})
		if err == nil {
			_, err = stream.Recv()
		}
		assertRequestPermission(t, err, i == 1)
	}
	_, err = userv1.NewProfileServiceClient(env.Conn).GrantAssistantConsent(apitest.User(t.Context(), profileUserA), &userv1.GrantAssistantConsentRequest{Version: "old"})
	assertRequestPermission(t, err, false)
}
func TestAPIPhoneQuotaAdviceDoesNotAuthorizeDuplicateReplay(t *testing.T) {
	p := &creationProvider{}
	env := startEnv(t, p, func(c *config.Config) {
		c.Limits[ratelimit.VerificationPhoneShort] = ratelimit.Rule{Limit: 1, Window: time.Hour}
	})
	if _, err := startCall(t, env, authKey(), "+79001234567"); err != nil {
		t.Fatal(err)
	}
	_, err := startCall(t, env, authKey(), "+79001234567")
	assertRequestPermission(t, err, true)
	if creates, _ := p.counts(); creates != 1 {
		t.Fatal("limited request reached provider", creates)
	}
	p = &creationProvider{mode: "duplicate"}
	env = startEnv(t, p)
	key := authKey()
	for range 2 {
		_, err = startCall(t, env, key, "+79001234568")
		r, ok := finerr.ToREST(err)
		if !ok || r.Code != "RATE_LIMITED" || (r.RequestRetryable == nil || *r.RequestRetryable) {
			t.Fatal("terminal dispatched busy reply lost false", r, err)
		}
	}
}

// A cacheable Register 400 uses Engine.Do, not Start's DoReserved adapter.
// A confirmed secret must remain unconsumed and the second RPC must decode the
// stored false Any without another attempt counter or registration side effect.
func TestAPIRegisterValidationFalseSurvivesIdempotentReplay(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	seedRegistration(t, env, "cache-invalid-zone", "+79001234567", time.Now().UTC())
	req := registrationRequest("cache-invalid-zone")
	req.Timezone = stringPtr("Local")
	key := authKey()
	client := userv1.NewAuthServiceClient(env.Conn)
	call := func() error { _, err := client.Register(authContext(t.Context(), key), req); return err }
	assertCachedRegistrationRefusal(t, env, key, call)
}
func assertCachedRegistrationRefusal(t *testing.T, env *apitest.Env, key string, call func() error) {
	t.Helper()
	var hits int
	for i := 0; i < 2; i++ {
		err := call()
		if apitest.Reason(err) != "VALIDATION_ERROR" {
			t.Fatal("invalid timezone not refused", err)
		}
		assertRequestPermission(t, err, false)
		var nextHits, users, sessions, done, encrypted int
		if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT
 (SELECT coalesce(sum(hits),0) FROM rate_limit_window),
 (SELECT count(*) FROM app_user),
 (SELECT count(*) FROM auth_session),
 (SELECT count(*) FROM idempotency_keys WHERE key=$1 AND done AND payload IS NOT NULL),
 (SELECT count(*) FROM idempotency_keys WHERE key=$1 AND encrypted)`, key).Scan(&nextHits, &users, &sessions, &done, &encrypted); err != nil {
			t.Fatal(err)
		}
		if users != 0 || sessions != 0 || done != 1 || encrypted != 1 || nextHits <= 0 {
			t.Fatal("wrong durable refusal", nextHits, users, sessions, done, encrypted)
		}
		if i == 1 && nextHits != hits {
			t.Fatal("cached refusal repeated the operation", hits, nextHits)
		}
		hits = nextHits
	}
}
