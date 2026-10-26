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
		if !ok || r.Code != "RATE_LIMITED" || r.RequestRetryable != nil {
			t.Fatal("cached busy reply invented advice", r, err)
		}
	}
}
