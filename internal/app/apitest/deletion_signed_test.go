package apitest_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	identity "github.com/fin-assist/go-identity"
	"google.golang.org/grpc/metadata"

	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/signing"
)

func TestDeleteAccountSignedJWTStillVerifiesRetries(t *testing.T) {
	userKey, userPublic := stageKey(t, "user")
	bffKey, bffPublic := stageKey(t, "bank-bff")
	encoded, err := json.Marshal(map[string]any{"keys": []any{userPublic, bffPublic}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "stage.jwks")
	if err = os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	env := apitest.Start(t, func(c *config.Config) {
		c.Env = config.EnvStage
		c.Auth = config.AuthConfig{Mode: config.AuthVerify, ServiceMode: "stage", ServiceIssuer: "stage-services", StageKeyFile: userKey, StagePublicKeysFile: path}
	})
	seedDocuments(t, env)
	seedRegistration(t, env, "signed-register", "+79001234567", time.Now().UTC())
	source, err := identity.NewStageSource("stage", identity.StageOptions{KeyFile: bffKey, Caller: "bank-bff", Issuer: "stage-services", Clock: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	service, err := source.Token(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx := metadata.AppendToOutgoingContext(t.Context(), "x-service-authorization", "Bearer "+service)
	registered, err := userv1.NewAuthServiceClient(env.Conn).Register(metadata.AppendToOutgoingContext(ctx, "idempotency-key", authKey(), "x-client-ip", "203.0.113.1"), registrationRequest("signed-register"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := env.App.Cfg.Signing
	expiredSigner, err := signing.Load(signing.Options{PrivateKeyFile: cfg.PrivateKeyFile, KeyID: cfg.KeyID, Issuer: cfg.Issuer, Audience: cfg.Audience, Clock: func() time.Time { return time.Now().Add(-2 * time.Hour) }})
	if err != nil {
		t.Fatal(err)
	}
	expired, err := expiredSigner.Sign(t.Context(), registered.GetUserId())
	if err != nil {
		t.Fatal(err)
	}
	forged := strings.Split(registered.GetTokens().GetAccessToken(), ".")
	forged[2] = strings.Repeat("A", len(forged[2]))
	profiles := userv1.NewProfileServiceClient(env.Conn)
	for _, jwt := range []string{expired, strings.Join(forged, ".")} {
		if _, err = profiles.DeleteAccount(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+jwt), &userv1.DeleteAccountRequest{}); apitest.Reason(err) != "INVALID_TOKEN" {
			t.Fatal("invalid JWT authorized deletion", err)
		}
	}
	access := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+registered.GetTokens().GetAccessToken())
	if _, err = profiles.GetProfile(access, &userv1.GetProfileRequest{}); err != nil {
		t.Fatal("rejected JWT changed account", err)
	}
	for range 2 {
		if _, err = profiles.DeleteAccount(access, &userv1.DeleteAccountRequest{}); err != nil {
			t.Fatal("signed repeat refused", err)
		}
	}
	if _, err = profiles.GetProfile(access, &userv1.GetProfileRequest{}); apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatal("deleted JWT reads data", err)
	}
	if _, err = profiles.DeleteAccount(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+expired), &userv1.DeleteAccountRequest{}); apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatal("tombstone bypassed expiry", err)
	}
}
