package apitest_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/config"
)

func TestAPIKubernetesIdentityUsesTLSKeysAndSeparatesCallersAndUsers(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var keyFetches atomic.Int32
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-api-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		keyFetches.Add(1)
		if err := json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "OKP", "crv": "Ed25519", "kid": "cluster-key", "x": base64.RawURLEncoding.EncodeToString(public)}}}); err != nil {
			t.Error(err)
		}
	}))
	defer endpoint.Close()
	dir := t.TempDir()
	ca, apiToken, serviceToken := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "api-token"), filepath.Join(dir, "service-token")
	_, retained := stageKey(t, "retained-user-key")
	keys, err := json.Marshal(map[string]any{"keys": []any{retained}})
	if err != nil {
		t.Fatal(err)
	}
	retainedPath := filepath.Join(dir, "retained.jwks")
	for path, value := range map[string][]byte{
		ca:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: endpoint.Certificate().Raw}),
		apiToken: []byte("local-api-token"), serviceToken: []byte("local-outgoing-token"), retainedPath: keys,
	} {
		if err := os.WriteFile(path, value, 0600); err != nil {
			t.Fatal(err)
		}
	}
	env := apitest.Start(t, func(c *config.Config) {
		c.Env = config.EnvProd
		c.Auth = config.AuthConfig{Mode: config.AuthVerify, ServiceTokenFile: serviceToken, Kubernetes: config.KubernetesAuthConfig{Issuer: "local-cluster", APITokenFile: apiToken, CAFile: ca, JWKSURL: endpoint.URL}}
		c.Signing.PublicKeysFile = retainedPath
	})
	seedDocuments(t, env)
	signed := func(caller, namespace string, ttl time.Duration) string {
		t.Helper()
		claims := jwt.MapClaims{"iss": "local-cluster", "aud": "finassist-internal", "sub": "system:serviceaccount:" + namespace + ":" + caller, "exp": time.Now().Add(ttl).Unix(), "iat": time.Now().Add(-time.Hour).Unix(), "kubernetes.io": map[string]any{"namespace": namespace, "serviceaccount": map[string]string{"name": caller}}}
		token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
		token.Header["kid"] = "cluster-key"
		raw, err := token.SignedString(private)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	service := signed("bank-bff", "finassist", time.Minute)
	ctx := metadata.AppendToOutgoingContext(t.Context(), "x-service-authorization", "Bearer "+service)
	client := userv1.NewConsentDocumentServiceClient(env.Conn)
	personal := &userv1.GetConsentDocumentRequest{Type: userv1.ConsentType_CONSENT_TYPE_PERSONAL_DATA}
	if response, err := client.GetConsentDocument(ctx, personal); err != nil || response.GetDocument().GetVersion() != "v1" {
		t.Fatalf("verified bank-bff could not read public consent: %v %v", response, err)
	}
	response, err := userv1.NewUserInternalServiceClient(env.Conn).GetSigningKeys(ctx, &userv1.GetSigningKeysRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var published struct {
		Keys []map[string]any `json:"keys"`
	}
	if json.Unmarshal([]byte(response.GetJwksJson()), &published) != nil || len(published.Keys) != 2 {
		t.Fatalf("retained public keys lost: %v", response)
	}
	seen := map[string]bool{}
	for _, key := range published.Keys {
		seen[key["kid"].(string)] = true
		if key["d"] != nil {
			t.Fatal("private signing material published")
		}
	}
	if !seen["apitest"] || !seen["retained-user-key"] || keyFetches.Load() == 0 {
		t.Fatalf("active/retained keys or authenticated TLS lookup missing: %v", seen)
	}
	for _, tc := range []struct {
		name, token string
		code        codes.Code
	}{
		{"foreign-caller", signed("analytics", "finassist", time.Minute), codes.PermissionDenied},
		{"foreign-namespace", signed("bank-bff", "another", time.Minute), codes.Unauthenticated},
		{"expired", signed("bank-bff", "finassist", -time.Hour), codes.Unauthenticated},
		{"dev-token", "dev:bank-bff", codes.Unauthenticated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := metadata.AppendToOutgoingContext(t.Context(), "x-service-authorization", "Bearer "+tc.token)
			if response, err := client.GetConsentDocument(ctx, personal); response != nil || status.Code(err) != tc.code || apitest.Reason(err) != "" {
				t.Fatalf("untrusted caller read a document: %v %v", response, err)
			}
		})
	}
	assistant := &userv1.GetConsentDocumentRequest{Type: userv1.ConsentType_CONSENT_TYPE_ASSISTANT}
	if _, err := client.GetConsentDocument(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+service), assistant); apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatalf("service JWT accepted as user JWT: %v", err)
	}
	access, err := env.App.Signing.Sign(t.Context(), "019c7714-3b77-74d1-9866-e1f484aae2ab")
	if err != nil {
		t.Fatal(err)
	}
	if response, err := client.GetConsentDocument(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+access), assistant); err != nil || response.GetDocument().GetVersion() != "v2" {
		t.Fatalf("valid user JWT refused after rejected credentials: %v %v", response, err)
	}
}
