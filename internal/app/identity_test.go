package app

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"

	tombstone "github.com/fin-assist/go-tombstone"

	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/adapter/postgres/migrate/migratetest"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/signing"
)

func signingConfig(t *testing.T) config.SigningConfig {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "user.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.SigningConfig{PrivateKeyFile: path, KeyID: "user-key", Issuer: "test-user", Audience: "test-app"}
}

func TestIntegrationKubernetesIdentityTLS(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]any{"keys": []any{map[string]string{"kty": "OKP", "crv": "Ed25519", "kid": "cluster-key", "x": base64.RawURLEncoding.EncodeToString(public)}}}
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer api-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := json.NewEncoder(w).Encode(set); err != nil {
			t.Error(err)
		}
	}))
	defer endpoint.Close()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.crt")
	apiToken := filepath.Join(dir, "api-token")
	serviceToken := filepath.Join(dir, "service-token")
	for path, b := range map[string][]byte{ca: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: endpoint.Certificate().Raw}), apiToken: []byte("api-token"), serviceToken: []byte("outgoing-token")} {
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Config{Env: config.EnvProd, Signing: signingConfig(t), Auth: config.AuthConfig{Mode: config.AuthVerify, ServiceMode: "kubernetes", ServiceIssuer: "cluster", ServiceTokenFile: serviceToken, KubernetesTokenFile: apiToken, KubernetesCAFile: ca, KubernetesJWKSURL: endpoint.URL}}
	keys, err := signing.Load(signing.Options{PrivateKeyFile: cfg.Signing.PrivateKeyFile, KeyID: cfg.Signing.KeyID, Issuer: cfg.Signing.Issuer, Audience: cfg.Signing.Audience, Clock: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := buildIdentity(cfg, keys, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.close()
	claims := jwt.MapClaims{"iss": "cluster", "aud": "finassist-internal", "sub": "system:serviceaccount:finassist:bank-bff", "exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix(), "kubernetes.io": map[string]any{"namespace": "finassist", "serviceaccount": map[string]string{"name": "bank-bff"}}}
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	token.Header["kid"] = "cluster-key"
	raw, err := token.SignedString(private)
	if err != nil {
		t.Fatal(err)
	}
	if caller, err := runtime.services.VerifyService(t.Context(), raw); err != nil || caller != "bank-bff" {
		t.Fatalf("caller=%q err=%v", caller, err)
	}
	if token, err := runtime.tokens.Token(t.Context()); err != nil || token != "outgoing-token" {
		t.Fatalf("outgoing: %q %v", token, err)
	}
	if _, err := runtime.services.VerifyService(t.Context(), "dev:bank-bff"); err == nil {
		t.Fatal("dev token accepted in prod")
	}
	for _, mutate := range []func(*config.Config){
		func(c *config.Config) { c.Auth.KubernetesCAFile = "/missing" },
		func(c *config.Config) { c.Auth.KubernetesCAFile = apiToken },
		func(c *config.Config) { c.Auth.KubernetesJWKSURL = "http://untrusted.test" },
		func(c *config.Config) { c.Auth.ServiceTokenFile = "" },
		func(c *config.Config) { c.Auth.KubernetesTokenFile = "" },
		func(c *config.Config) { c.Auth.ServiceIssuer = "" },
		func(c *config.Config) { c.Auth.ServiceMode = "unsupported" },
		func(c *config.Config) { c.Auth.Mode = config.AuthInsecureDev },
		func(c *config.Config) { c.Auth.Mode = config.AuthMode("unknown") },
		func(c *config.Config) { c.Signing.Issuer = "" },
		func(c *config.Config) { c.Auth.ServiceMode = "stage"; c.Auth.StagePublicKeysFile = "/missing" },
	} {
		c := cfg
		mutate(&c)
		if r, err := buildIdentity(c, keys, prometheus.NewRegistry()); err == nil {
			r.close()
			t.Fatalf("invalid config accepted: %+v", c.Auth)
		}
	}
	// A duplicate metrics registration is an assembly error, never silently ignored.
	registry := prometheus.NewRegistry()
	r, err := buildIdentity(cfg, keys, registry)
	if err != nil {
		t.Fatal(err)
	}
	defer r.close()
	if _, err := buildIdentity(cfg, keys, registry); err == nil {
		t.Fatal("duplicate registration ignored")
	}
}

func TestUnitNewRequiresSigningKey(t *testing.T) {
	cfg := config.Config{Env: config.EnvDev, Auth: config.AuthConfig{Mode: config.AuthInsecureDev}}
	if _, err := New(t.Context(), cfg, nil, prometheus.NewRegistry()); err == nil || !strings.Contains(err.Error(), "signing:") {
		t.Fatalf("startup: %v", err)
	}
}

func TestIntegrationLibraryMigrationSources(t *testing.T) {
	for _, source := range MigrationSources() {
		t.Run(source.Name, func(t *testing.T) { migratetest.CheckUpDownUp(t, source) })
	}
}

func TestUnitStageVerifierCannotStartInProd(t *testing.T) {
	cfg := config.Config{Env: config.EnvProd, Service: "user", Signing: signingConfig(t), Auth: config.AuthConfig{Mode: config.AuthVerify, ServiceMode: "stage", ServiceIssuer: "stage"}}
	keys, err := signing.Load(signing.Options{PrivateKeyFile: cfg.Signing.PrivateKeyFile, KeyID: cfg.Signing.KeyID, Issuer: cfg.Signing.Issuer, Audience: cfg.Signing.Audience, Clock: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	b, err := keys.JWKS(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "stage.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Auth.StagePublicKeysFile = path
	if _, err := buildIdentity(cfg, keys, prometheus.NewRegistry()); err == nil {
		t.Fatal("stage verifier enabled in prod")
	}
}

func TestIntegrationTombstoneServerAdapter(t *testing.T) {
	db, err := postgres.Open(t.Context(), postgres.Options{PrimaryDSN: migratetest.DSN(t), MaxConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store, err := tombstone.New(db, db.Primary(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	// An invalid subject must fail closed rather than query or silently accept it.
	if _, err := (deletedUsers{store}).IsDeleted(t.Context(), "invalid-subject"); err == nil {
		t.Fatal("invalid subject accepted")
	}
}
