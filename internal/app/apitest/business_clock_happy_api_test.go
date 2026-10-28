package apitest_test

import (
	"sync/atomic"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"

	"github.com/fin-assist/user/internal/app"
	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/verification"
)

func TestAPIBusinessClockControlsSuccessfulStartAndFakeConfirmation(t *testing.T) {
	for _, configuredFake := range []bool{false, true} {
		t.Run(map[bool]string{false: "trusted-adapter", true: "configured-fake"}[configuredFake], func(t *testing.T) {
			base := time.Date(2032, time.February, 29, 23, 59, 0, 0, time.UTC)
			var instant atomic.Int64
			instant.Store(base.UnixNano())
			now := func() time.Time { return time.Unix(0, instant.Load()).UTC() }
			options := []app.Option{app.WithClock(now)}
			var provider *evidenceProvider
			phone := "+79001234567"
			if !configuredFake {
				provider = &evidenceProvider{outcome: func(a verification.Attempt, c verification.Call, err error) (verification.Call, error) {
					c.ReceivedAt = a.FirstDispatchAt
					return c, err
				}}
				options = append(options, app.WithPhoneBackend(provider, creationSelector{}))
			} else {
				phone = "+70000001234"
			}
			env := apitest.StartWithOptions(t, options, func(c *config.Config) {
				if configuredFake {
					c.Zvonok.Provider = "fake"
				}
			})
			if provider != nil {
				provider.env = env
			}
			seedDocuments(t, env)
			client := userv1.NewAuthServiceClient(env.Conn)
			key, request := authKey(), consentStartRequest(phone)
			started, err := client.StartPhoneVerification(authContext(t.Context(), key), request)
			if err != nil {
				t.Fatal("successful Start used a different clock", err)
			}
			if started.GetExpiresIn() != 300 || !started.GetExpiresAt().AsTime().Equal(base.Add(5*time.Minute)) {
				t.Fatal("Start lifetime was not five minutes", started.GetExpiresIn(), started.GetExpiresAt())
			}
			instant.Store(base.Add(time.Second).UnixNano())
			replay, err := client.StartPhoneVerification(authContext(t.Context(), key), request)
			if err != nil || replay.GetExpiresIn() != 299 || replay.GetVerificationToken() != started.GetVerificationToken() {
				t.Fatal("replay lifetime did not follow application clock", replay, err)
			}
			if configuredFake {
				verified, err := openPhoneWatch(t, env, phoneContext(t.Context()), started.GetVerificationToken()).Recv()
				if err != nil || !verified.GetIsNewUser() || verified.GetRegistrationToken() == "" {
					t.Fatal("common fake confirmation failed", verified, err)
				}
				registered, err := client.Register(authContext(t.Context(), authKey()), registrationRequest(verified.GetRegistrationToken()))
				if err != nil || registered.GetUserId() == "" || registered.GetTokens().GetRefreshToken() == "" {
					t.Fatal("registration with application clock failed", registered, err)
				}
				var owner string
				var accepted, confirmed time.Time
				if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT e.linked_user_id::text,e.consent_accepted_at,v.verified_at FROM phone_consent_receipt e JOIN phone_verification v ON v.id=e.attempt_id WHERE e.start_key=$1`, key).Scan(&owner, &accepted, &confirmed); err != nil || owner != registered.GetUserId() || !accepted.Equal(base) || !confirmed.Equal(base) {
					t.Fatal("receipt/confirmation clocks diverged", owner, accepted, confirmed, err)
				}
			}
		})
	}
}
