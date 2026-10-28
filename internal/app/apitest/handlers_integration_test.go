package apitest_test

import (
	"context"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	idempotency "github.com/fin-assist/go-idempotency"
	outboxlib "github.com/fin-assist/go-outbox"
	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/go-grpc/reqctx"

	authgrpc "github.com/fin-assist/user/internal/adapter/grpc/auth"
	consentgrpc "github.com/fin-assist/user/internal/adapter/grpc/consent"
	profilegrpc "github.com/fin-assist/user/internal/adapter/grpc/profile"
	signinggrpc "github.com/fin-assist/user/internal/adapter/grpc/signing"
	"github.com/fin-assist/user/internal/adapter/outbox"
	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/auth"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/consent"
	"github.com/fin-assist/user/internal/deletion"
	"github.com/fin-assist/user/internal/profile"
	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/verification"
)

// A registrar exposes direct adapter calls: this intentionally omits the public
// transport and its authorization chain. The collaborators below are real SQL
// repositories, transactions, signing, tombstones and the idempotency engine.
type integrationRegistrar map[string]any

func (r integrationRegistrar) RegisterService(d *grpc.ServiceDesc, s any) { r[d.ServiceName] = s }

func TestIntegrationHandlersCommitCachedSessionsAndSettingsAtomically(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	publisher, err := outboxlib.NewPublisher(env.App.DB, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	events := outbox.NewEvents(publisher)
	profiles := profile.New(postgres.NewProfileTransactions(env.App.DB), postgres.NewProfiles(env.App.DB), env.App.Tombstones, events, time.Now)
	accounts := deletion.New(env.App.DB, env.App.Tombstones, postgres.NewAccounts(env.App.DB), events)
	key, err := config.LoadSessionKey(env.App.Cfg.Sessions)
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := idempotency.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	// Separate pools are essential: attempt accounting commits independently of the
	// transaction that may roll back for a rejected registration or refresh token.
	state, err := postgres.Open(t.Context(), postgres.Options{PrimaryDSN: env.DSN, MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	stateGuard, err := tombstone.New(state, state.Primary(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	statePublisher, err := outboxlib.NewPublisher(state, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	sessions := auth.New(state, postgres.NewAuth(state), stateGuard, env.App.Signing, outbox.NewEvents(statePublisher), time.Now, token.New, time.Hour)
	engine, err := idempotency.New(state, idempotency.Options{Clock: time.Now, Cipher: cipher, Registerer: prometheus.NewRegistry(), Methods: map[string]idempotency.Policy{
		userv1.AuthService_StartPhoneVerification_FullMethodName: {Encrypt: true},
		userv1.AuthService_Register_FullMethodName:               {Encrypt: true},
		userv1.AuthService_RefreshTokens_FullMethodName:          {Encrypt: true, TTL: time.Minute},
	}})
	if err != nil {
		t.Fatal(err)
	}
	r := integrationRegistrar{}
	starter := integrationStarter(t, env, &creationProvider{env: env})
	authgrpc.Register(r, sessions, engine, postgres.NewSessionTransactions(state), env.App.DB, env.App.Limits, stateGuard, env.App.Verifications, time.Now, starter)
	profilegrpc.Register(r, profiles, accounts)
	consentgrpc.Register(r, consent.New(postgres.NewConsentDocuments(env.App.DB)), env.App.Users, env.App.Tombstones)
	signinggrpc.Register(r, env.App.Signing, profiles)
	a := r["finassist.user.v1.AuthService"].(userv1.AuthServiceServer)
	p := r["finassist.user.v1.ProfileService"].(userv1.ProfileServiceServer)
	d := r["finassist.user.v1.ConsentDocumentService"].(userv1.ConsentDocumentServiceServer)
	s := r["finassist.user.v1.UserInternalService"].(userv1.UserInternalServiceServer)
	ctx := reqctx.WithClientIP(t.Context(), "203.0.113.19")
	keysByLabel := map[string]string{}
	incoming := func(key string) context.Context {
		if keysByLabel[key] == "" {
			keysByLabel[key] = authKey()
		}
		return metadata.NewIncomingContext(ctx, metadata.Pairs("idempotency-key", keysByLabel[key]))
	}
	started, err := a.StartPhoneVerification(incoming("integration-start"), &userv1.StartPhoneVerificationRequest{Phone: "+79001234569", PdConsentAccepted: true, PdConsentVersion: "v1", AgeConfirmed: true})
	if err != nil || started.GetVerificationToken() == "" {
		t.Fatal(started, err)
	}
	var call string
	if err := env.App.DB.Primary().QueryRow(ctx, `SELECT provider_call_id FROM phone_verification WHERE phone='+79001234569'`).Scan(&call); err != nil {
		t.Fatal(err)
	}
	if changed, err := env.App.Verifications.Confirm(ctx, verification.Proof{Provider: "test", CallID: call, Phone: "+79001234569"}); err != nil || !changed {
		t.Fatal(changed, err)
	}
	stream := &integrationVerificationStream{ctx: ctx}
	if err := a.WatchPhoneVerification(&userv1.WatchPhoneVerificationRequest{VerificationToken: started.GetVerificationToken()}, stream); err != nil {
		t.Fatal(err)
	}
	if len(stream.outcomes) != 1 || stream.outcomes[0].GetStatus() != userv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED || stream.outcomes[0].GetRegistrationToken() == "" {
		t.Fatal(stream.outcomes)
	}
	seedRegistration(t, env, "handler-registration", "+79001234567", time.Now().Add(-time.Minute))
	seedRegistration(t, env, "handler-invalid-zone", "+79001234568", time.Now().UTC())
	invalidReq := registrationRequest("handler-invalid-zone")
	invalidReq.Timezone = stringPtr("Local")
	invalidContext := incoming("integration-register-invalid-zone")
	assertCachedRegistrationRefusal(t, env, keysByLabel["integration-register-invalid-zone"], func() error { _, err := a.Register(invalidContext, invalidReq); return err })
	in := &userv1.RegisterRequest{RegistrationToken: "handler-registration", PdConsentAccepted: true, PdConsentVersion: "v1"}
	registered, err := a.Register(incoming("integration-register"), in)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := a.Register(incoming("integration-register"), in)
	if err != nil || !proto.Equal(registered, replay) {
		t.Fatalf("encrypted replay changed: %v %v", replay, err)
	}
	if _, err := a.Register(incoming("integration-register-conflict"), in); apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatal(err)
	}
	user := reqctx.WithUserID(ctx, registered.GetUserId())
	got, err := p.GetProfile(user, &userv1.GetProfileRequest{})
	if err != nil || got.GetProfile().GetPhone() != "+79001234567" {
		t.Fatal(got, err)
	}
	zone := "Asia/Tokyo"
	for _, theme := range []userv1.Theme{userv1.Theme_THEME_LIGHT, userv1.Theme_THEME_DARK, userv1.Theme_THEME_SYSTEM} {
		changed, err := p.UpdateProfile(user, &userv1.UpdateProfileRequest{Timezone: &zone, Theme: &theme})
		if err != nil || changed.GetProfile().GetTheme() != theme || changed.GetProfile().GetTimezone() != zone {
			t.Fatal(changed, err)
		}
	}
	if _, err := p.UpdateProfile(user, &userv1.UpdateProfileRequest{Theme: userv1.Theme(99).Enum()}); apitest.Reason(err) != "VALIDATION_ERROR" {
		t.Fatal(err)
	}
	if _, err := p.GrantAssistantConsent(user, &userv1.GrantAssistantConsentRequest{Version: "v2"}); err != nil {
		t.Fatal(err)
	}
	status, err := s.GetAssistantConsentStatus(ctx, &userv1.GetAssistantConsentStatusRequest{UserId: stringPtr(registered.GetUserId())})
	if err != nil || !status.GetConsent().GetGranted() {
		t.Fatal(status, err)
	}
	if _, err := p.RevokeAssistantConsent(user, &userv1.RevokeAssistantConsentRequest{}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []userv1.ConsentType{userv1.ConsentType_CONSENT_TYPE_PERSONAL_DATA, userv1.ConsentType_CONSENT_TYPE_ASSISTANT} {
		docctx := metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer dev:"+registered.GetUserId()))
		doc, err := d.GetConsentDocument(docctx, &userv1.GetConsentDocumentRequest{Type: kind})
		if err != nil || doc.GetDocument().GetVersion() == "" {
			t.Fatal(doc, err)
		}
	}
	keys, err := s.GetSigningKeys(ctx, &userv1.GetSigningKeysRequest{})
	if err != nil || len(keys.GetJwksJson()) == 0 {
		t.Fatal(keys, err)
	}
	refresh := &userv1.RefreshTokensRequest{RefreshToken: registered.GetTokens().GetRefreshToken()}
	rotated, err := a.RefreshTokens(incoming("integration-refresh"), refresh)
	if err != nil || rotated.GetTokens().GetRefreshToken() == "" {
		t.Fatal(rotated, err)
	}
	if _, err = a.RefreshTokens(incoming("integration-refresh-reuse"), refresh); apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatal(err)
	}
	if _, err = a.RefreshTokens(incoming("integration-refresh-child"), &userv1.RefreshTokensRequest{RefreshToken: rotated.GetTokens().GetRefreshToken()}); apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatal(err)
	}
	if _, err = a.Logout(user, &userv1.LogoutRequest{RefreshToken: rotated.GetTokens().GetRefreshToken()}); err != nil {
		t.Fatal(err)
	}
	if _, err = p.DeleteAccount(user, &userv1.DeleteAccountRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Register(incoming("integration-register"), in); apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatal("deleted cached owner replayed", err)
	}
	assertRequestPermission(t, err, false)
	_, err = p.UpdateProfile(user, &userv1.UpdateProfileRequest{Theme: userv1.Theme_THEME_DARK.Enum()})
	if apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatal(err)
	}
	assertRequestPermission(t, err, false)
	if _, err = p.GetProfile(user, &userv1.GetProfileRequest{}); apitest.Reason(err) != "NOT_FOUND" {
		t.Fatal(err)
	}
	if _, err = d.GetConsentDocument(metadata.NewIncomingContext(ctx, metadata.Pairs("authorization", "Bearer dev:"+registered.GetUserId())), &userv1.GetConsentDocumentRequest{Type: userv1.ConsentType_CONSENT_TYPE_ASSISTANT}); apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatal(err)
	}
	// Direct handler identity guards are separate from the omitted server chain.
	for _, call := range []func() error{
		func() error { _, e := p.GetProfile(ctx, &userv1.GetProfileRequest{}); return e },
		func() error { _, e := p.UpdateProfile(ctx, &userv1.UpdateProfileRequest{}); return e },
		func() error { _, e := p.GrantAssistantConsent(ctx, &userv1.GrantAssistantConsentRequest{}); return e },
		func() error { _, e := p.RevokeAssistantConsent(ctx, &userv1.RevokeAssistantConsentRequest{}); return e },
		func() error { _, e := p.DeleteAccount(ctx, &userv1.DeleteAccountRequest{}); return e },
	} {
		if err := call(); apitest.Reason(err) != "INVALID_TOKEN" {
			t.Fatal(err)
		}
	}
}

type integrationVerificationStream struct {
	grpc.ServerStream
	ctx      context.Context
	outcomes []*userv1.WatchPhoneVerificationResponse
}

func (s *integrationVerificationStream) Context() context.Context { return s.ctx }
func (s *integrationVerificationStream) Send(out *userv1.WatchPhoneVerificationResponse) error {
	s.outcomes = append(s.outcomes, out)
	return nil
}
