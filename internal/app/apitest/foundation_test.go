package apitest_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	goidentity "github.com/fin-assist/go-identity"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/config"
)

func seedDocuments(t *testing.T, env *apitest.Env) {
	t.Helper()
	_, err := env.App.DB.Primary().Exec(t.Context(), `INSERT INTO consent_document(type,version,title,url,is_current) VALUES ('personal_data','v1','PD consent','https://example.test/pd',true),('assistant','v2','Assistant consent','https://example.test/assistant',true)`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestDocumentAPI(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	c := userv1.NewConsentDocumentServiceClient(env.Conn)
	ctx := apitest.Caller(t.Context(), "bank-bff")
	r, err := c.GetConsentDocument(ctx, &userv1.GetConsentDocumentRequest{Type: userv1.ConsentType_CONSENT_TYPE_PERSONAL_DATA})
	if err != nil || r.GetDocument().GetVersion() != "v1" || r.GetDocument().GetTitle() != "PD consent" || r.GetDocument().GetUrl() != "https://example.test/pd" {
		t.Fatalf("%v %v", r, err)
	}
	for _, kind := range []userv1.ConsentType{userv1.ConsentType_CONSENT_TYPE_UNSPECIFIED, userv1.ConsentType(100)} {
		_, err := c.GetConsentDocument(ctx, &userv1.GetConsentDocumentRequest{Type: kind})
		if apitest.Reason(err) != "NOT_FOUND" {
			t.Fatalf("unknown %v: %v", kind, err)
		}
	}
	req := &userv1.GetConsentDocumentRequest{Type: userv1.ConsentType_CONSENT_TYPE_ASSISTANT}
	if _, err := c.GetConsentDocument(ctx, req); apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatalf("missing access: %v", err)
	}
	id := "019c7714-3b77-74d1-9866-e1f484aae2ab"
	if _, err := c.GetConsentDocument(apitest.User(t.Context(), id), req); err != nil {
		t.Fatal(err)
	}
	if err := env.App.DB.Run(t.Context(), func(ctx context.Context) error {
		return env.App.Tombstones.Purge(ctx, id, func(context.Context, string) error { return nil })
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetConsentDocument(apitest.User(t.Context(), id), req); apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatalf("deleted: %v", err)
	}
	if _, err := c.GetConsentDocument(apitest.Caller(t.Context(), "assistant"), req); status.Code(err) != codes.PermissionDenied || apitest.Reason(err) != "" {
		t.Fatalf("foreign caller: %v", err)
	}
	if _, err := c.GetConsentDocument(t.Context(), req); status.Code(err) != codes.Unauthenticated || apitest.Reason(err) != "" {
		t.Fatalf("no service token: %v", err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE consent_document SET is_current=false WHERE type='personal_data'`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetConsentDocument(ctx, &userv1.GetConsentDocumentRequest{Type: userv1.ConsentType_CONSENT_TYPE_PERSONAL_DATA}); apitest.Reason(err) != "NOT_FOUND" {
		t.Fatalf("unpublished: %v", err)
	}
	if _,err:=env.App.DB.Primary().Exec(t.Context(),`DROP TABLE consent_document CASCADE`);err!=nil{t.Fatal(err)}
	_,err=c.GetConsentDocument(ctx,&userv1.GetConsentDocumentRequest{Type:userv1.ConsentType_CONSENT_TYPE_PERSONAL_DATA})
	if status.Code(err)!=codes.Internal || apitest.Reason(err)!="" || strings.Contains(status.Convert(err).Message(),"consent_document") {t.Fatalf("database error leaked: %v",err)}
}

func TestSigningKeysAPI(t *testing.T) {
	env := apitest.Start(t)
	c := userv1.NewUserInternalServiceClient(env.Conn)
	for _, caller := range []string{"bank-bff", "user", "assistant", "analytics", "bank-statement-data"} {
		r, err := c.GetSigningKeys(apitest.Caller(t.Context(), caller), &userv1.GetSigningKeysRequest{})
		if err != nil {
			t.Fatal(err)
		}
		var public struct {
			Keys []map[string]any `json:"keys"`
		}
		if err := json.Unmarshal([]byte(r.GetJwksJson()), &public); err != nil {
			t.Fatal(err)
		}
		if len(public.Keys) != 1 || public.Keys[0]["d"] != nil || public.Keys[0]["kid"] != "apitest" {
			t.Fatalf("keys: %s", r.GetJwksJson())
		}
	}
	if _, err := c.GetSigningKeys(t.Context(), &userv1.GetSigningKeysRequest{}); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unauthenticated JWKS: %v", err)
	}
}

func stageKey(t *testing.T, name string) (string, map[string]string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), name+".pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p, map[string]string{"kty": "OKP", "crv": "Ed25519", "kid": name, "x": base64.RawURLEncoding.EncodeToString(public)}
}

func TestStageSignedIdentityAPI(t *testing.T) {
	userKey, userPublic := stageKey(t, "user")
	bffKey, bffPublic := stageKey(t, "bank-bff")
	b, err := json.Marshal(map[string]any{"keys": []any{userPublic, bffPublic}})
	if err != nil {
		t.Fatal(err)
	}
	pubFile := filepath.Join(t.TempDir(), "stage.jwks")
	if err := os.WriteFile(pubFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	env := apitest.Start(t, func(c *config.Config) {
		c.Env = config.EnvStage
		c.Auth = config.AuthConfig{Mode: config.AuthVerify, ServiceMode: "stage", ServiceIssuer: "stage-services", StageKeyFile: userKey, StagePublicKeysFile: pubFile}
	})
	seedDocuments(t, env)
	bff, err := goidentity.NewStageSource("stage", goidentity.StageOptions{KeyFile: bffKey, Caller: "bank-bff", Issuer: "stage-services", Clock: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	serviceToken, err := bff.Token(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx := metadata.AppendToOutgoingContext(t.Context(), "x-service-authorization", "Bearer "+serviceToken)
	c := userv1.NewConsentDocumentServiceClient(env.Conn)
	if _, err := c.GetConsentDocument(ctx, &userv1.GetConsentDocumentRequest{Type: userv1.ConsentType_CONSENT_TYPE_PERSONAL_DATA}); err != nil {
		t.Fatal(err)
	}
	access, err := env.App.Signing.Sign(t.Context(), "019c7714-3b77-74d1-9866-e1f484aae2ab")
	if err != nil {
		t.Fatal(err)
	}
	req := &userv1.GetConsentDocumentRequest{Type: userv1.ConsentType_CONSENT_TYPE_ASSISTANT}
	if _, err := c.GetConsentDocument(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+access), req); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(access, ".")
	parts[2] = strings.Repeat("A", len(parts[2]))
	if _, err := c.GetConsentDocument(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+strings.Join(parts, ".")), req); apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatalf("forged access: %v", err)
	}
	if _, err := c.GetConsentDocument(apitest.Caller(t.Context(), "bank-bff"), req); status.Code(err) != codes.Unauthenticated || apitest.Reason(err) != "" {
		t.Fatalf("dev credential accepted in stage: %v", err)
	}
	// The service token produced for outgoing calls is signed and names user.
	token, err := env.App.Tokens.Token(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = userv1.NewUserInternalServiceClient(env.Conn).GetSigningKeys(metadata.AppendToOutgoingContext(t.Context(), "x-service-authorization", "Bearer "+token), &userv1.GetSigningKeysRequest{})
	if err != nil {
		t.Fatal(err)
	}
}
