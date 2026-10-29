package apitest_test

import (
	"context"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	idempotency "github.com/fin-assist/go-idempotency"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/contracts/gen/go/finerr"

	"github.com/fin-assist/user/internal/adapter/fakephone"
	"github.com/fin-assist/user/internal/adapter/grpc/retryinfo"
	idempotencyadapter "github.com/fin-assist/user/internal/adapter/idempotency"
	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/verification"
)

func integrationStarter(t *testing.T, env *apitest.Env, p verification.Provider) *verification.Starter {
	t.Helper()
	key, err := config.LoadSessionKey(env.App.Cfg.Sessions)
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := idempotency.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := idempotency.New(env.App.DB, idempotency.Options{Clock: time.Now, Cipher: cipher, Registerer: prometheus.NewRegistry(), Methods: map[string]idempotency.Policy{userv1.AuthService_StartPhoneVerification_FullMethodName: {Encrypt: true}}})
	if err != nil {
		t.Fatal(err)
	}
	var selector verification.NumberSelector
	if p != nil {
		numbers, err := verification.NewNumbers(postgres.NewVerifications(env.App.DB), env.App.Log, prometheus.NewRegistry())
		if err != nil {
			t.Fatal(err)
		}
		selector = numbers
	}
	starter, err := verification.NewStarter(env.App.DB, postgres.NewVerificationStarts(env.App.DB), idempotencyadapter.StartEngine{StartEngine: engine}, env.App.Limits, postgres.NewAuth(env.App.DB), env.App.Tombstones, p, selector, time.Now, token.New, env.App.Log, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	return starter
}

func TestIntegrationStartCommitsReservationBeforeProviderAndRecoversLostReply(t *testing.T) {
	for _, mode := range []string{"ok", "lost", "unknown", "reject", "duplicate", "empty"} {
		t.Run(mode, func(t *testing.T) {
			env := apitest.Start(t)
			seedDocuments(t, env)
			p := &creationProvider{env: env, mode: mode}
			starter := integrationStarter(t, env, p)
			phone := "+79001234567"
			request := idempotency.Request{Endpoint: userv1.AuthService_StartPhoneVerification_FullMethodName, Key: authKey(), Hash: token.Sum(phone)}
			first, err := starter.StartWithConsent(t.Context(), request, phone, "192.0.2.1", verification.Consent{Accepted: true, Version: "v1", AgeConfirmed: true})
			switch mode {
			case "ok", "lost":
				if mode == "lost" {
					if err == nil {
						t.Fatal("lost response accepted")
					}
					first, err = starter.StartWithConsent(t.Context(), request, phone, "192.0.2.1", verification.Consent{Accepted: true, Version: "v1", AgeConfirmed: true})
				}
				if err != nil || first.Token == "" || first.CallbackNumber == "" {
					t.Fatal(first, err)
				}
				replay, err := starter.StartWithConsent(t.Context(), request, phone, "192.0.2.1", verification.Consent{Accepted: true, Version: "v1", AgeConfirmed: true})
				if err != nil || replay.Token != first.Token || replay.CallbackNumber != first.CallbackNumber {
					t.Fatalf("replay changed committed secret: %+v %v", replay, err)
				}
				request.Hash = token.Sum("changed body")
				if _, err := starter.StartWithConsent(t.Context(), request, phone, "192.0.2.1", verification.Consent{Accepted: true, Version: "v1", AgeConfirmed: true}); apitest.Reason(err) != "IDEMPOTENCY_CONFLICT" {
					t.Fatal("conflicting body accepted", err)
				}
				request.Key = authKey()
				request.Hash = token.Sum(phone)
				if _, err := starter.StartWithConsent(t.Context(), request, phone, "192.0.2.1", verification.Consent{Accepted: true, Version: "v1", AgeConfirmed: true}); apitest.Reason(err) != "RATE_LIMITED" {
					t.Fatal("parallel phone claim accepted", err)
				}
			case "duplicate":
				if apitest.Reason(err) != "RATE_LIMITED" {
					t.Fatal("duplicate not translated", err)
				}
				if _, err = starter.StartWithConsent(t.Context(), request, phone, "192.0.2.1", verification.Consent{Accepted: true, Version: "v1", AgeConfirmed: true}); apitest.Reason(err) != "RATE_LIMITED" {
					t.Fatal("duplicate result not retained", err)
				}
			case "reject":
				if err == nil || startHits(t, env) != 0 {
					t.Fatal("definite refusal consumed reserve", err)
				}
				p.setMode("ok")
				if recovered, err := starter.StartWithConsent(t.Context(), request, phone, "192.0.2.1", verification.Consent{Accepted: true, Version: "v1", AgeConfirmed: true}); err == nil || recovered.Token != "" {
					t.Fatal("terminal rejection retried external effect", recovered, err)
				}
			default:
				if err == nil {
					t.Fatal("uncertain creation accepted")
				}
				p.setMode("ok")
				if recovered, err := starter.StartWithConsent(t.Context(), request, phone, "192.0.2.1", verification.Consent{Accepted: true, Version: "v1", AgeConfirmed: true}); err != nil || recovered.Token == "" {
					t.Fatal("own attempt not recoverable", recovered, err)
				}
			}
			creates, _ := p.counts()
			if creates < 1 {
				t.Fatal("provider never ran")
			}
		})
	}
	env := apitest.Start(t)
	seedDocuments(t, env)
	disabled := integrationStarter(t, env, nil)
	request := idempotency.Request{Endpoint: userv1.AuthService_StartPhoneVerification_FullMethodName, Key: authKey(), Hash: token.Sum("disabled")}
	if _, err := disabled.StartWithConsent(t.Context(), request, "+79001234567", "192.0.2.1", verification.Consent{Accepted: true, Version: "v1", AgeConfirmed: true}); err == nil {
		t.Fatal("disabled provider accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := disabled.StartWithConsent(ctx, request, "bad phone", "192.0.2.1", verification.Consent{Accepted: true, Version: "v1", AgeConfirmed: true}); err == nil {
		t.Fatal("cancelled invalid input accepted")
	}
}

func TestIntegrationFakeBackendUsesDurableAttemptAndCommonConfirmation(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	p := fakephone.New(time.Now)
	starter := integrationStarter(t, env, p)
	starter.SetImmediateConfirmation(env.App.Verifications)
	phone := "+70000001111"
	request := idempotency.Request{Endpoint: userv1.AuthService_StartPhoneVerification_FullMethodName, Key: authKey(), Hash: token.Sum(phone)}
	first, err := starter.StartWithConsent(t.Context(), request, phone, "203.0.113.9", verification.Consent{Accepted: true, Version: "v1", AgeConfirmed: true})
	if err != nil || first.Token == "" {
		t.Fatal(first, err)
	}
	_, proof, err := env.App.Verifications.Snapshot(t.Context(), first.Token)
	if err != nil || proof.State != verification.Verified || !proof.IsNewUser || proof.RegistrationToken == "" {
		t.Fatal(proof, err)
	}
	// Reconstructing the provider and coordinator preserves the committed result.
	restarted := integrationStarter(t, env, fakephone.New(time.Now))
	restarted.SetImmediateConfirmation(env.App.Verifications)
	again, err := restarted.StartWithConsent(t.Context(), request, phone, "203.0.113.9", verification.Consent{Accepted: true, Version: "v1", AgeConfirmed: true})
	if err != nil || again.Token != first.Token || again.CallbackNumber != first.CallbackNumber {
		t.Fatal(again, err)
	}
	// Real phones are rejected by the development backend before a durable success
	// can be committed, while the common coordinator compensates the reservation.
	phone = "+79001234567"
	request.Key = authKey()
	request.Hash = token.Sum(phone)
	if out, err := restarted.StartWithConsent(t.Context(), request, phone, "203.0.113.9", verification.Consent{Accepted: true, Version: "v1", AgeConfirmed: true}); err == nil || out.Token != "" {
		t.Fatal(out, err)
	}
	var results int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM phone_verification WHERE phone=$1 AND verified_at IS NOT NULL`, phone).Scan(&results); err != nil || results != 0 {
		t.Fatal(results, err)
	}
}

func TestIntegrationPhoneAdviceSurvivesCachedValidationButDoesNotUpgradeBusyReplay(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	starter := integrationStarter(t, env, &creationProvider{})
	request := idempotency.Request{Endpoint: userv1.AuthService_StartPhoneVerification_FullMethodName, Key: authKey(), Hash: token.Sum("invalid")}
	for range 2 {
		_, err := starter.StartWithConsent(t.Context(), request, "invalid", "192.0.2.1", verification.Consent{Accepted: true, Version: "v1", AgeConfirmed: true})
		rest, ok := finerr.ToREST(retryinfo.Apply(err))
		if !ok || rest.Code != "VALIDATION_ERROR" || rest.RequestRetryable == nil || *rest.RequestRetryable {
			t.Fatal("cached validation lost false", rest, err)
		}
	}
	var rows int
	if err := env.App.DB.Primary().QueryRow(t.Context(), "SELECT count(*) FROM phone_verification").Scan(&rows); err != nil || rows != 0 {
		t.Fatal(rows, err)
	}
	p := &creationProvider{mode: "duplicate"}
	starter = integrationStarter(t, env, p)
	request.Key = authKey()
	request.Hash = token.Sum("duplicate")
	for range 2 {
		_, err := starter.StartWithConsent(t.Context(), request, "+79001234567", "192.0.2.1", verification.Consent{Accepted: true, Version: "v1", AgeConfirmed: true})
		rest, ok := finerr.ToREST(retryinfo.Apply(err))
		if !ok || rest.Code != "RATE_LIMITED" || (rest.RequestRetryable == nil || *rest.RequestRetryable) {
			t.Fatal("terminal dispatched busy outcome lost false", rest, err)
		}
	}
}
