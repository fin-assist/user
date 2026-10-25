package app

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/signing"
)

// FIN-325: NewAssembly selects the service identity by ENV and reads only configured
// files; no network is touched at construction, so this stays a unit test.
func TestUnitIdentityAssemblyUsesStageFilesOrDefersKubernetesFetch(t *testing.T) {
	cfg := config.Config{Env: config.EnvStage, Service: "user", Signing: signingConfig(t), Auth: config.AuthConfig{Mode: config.AuthVerify, Stage: config.StageAuthConfig{Issuer: "stage-issuer"}}}
	keys, err := signing.Load(signing.Options{PrivateKeyFile: cfg.Signing.PrivateKeyFile, KeyID: cfg.Signing.KeyID, Issuer: cfg.Signing.Issuer, Audience: cfg.Signing.Audience, Clock: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	// The stage JWKS binds kid = service name; reuse the user key under kid "user".
	stageKeys, err := signing.Load(signing.Options{PrivateKeyFile: cfg.Signing.PrivateKeyFile, KeyID: "user", Issuer: "x", Audience: "y", Clock: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	public, err := stageKeys.JWKS(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Auth.Stage.PublicKeysFile = filepath.Join(t.TempDir(), "stage.json")
	if err = os.WriteFile(cfg.Auth.Stage.PublicKeysFile, public, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Auth.Stage.SigningKeyFile = cfg.Signing.PrivateKeyFile
	runtime, err := buildIdentity(cfg, keys, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.close()
	raw, err := runtime.tokens.Token(t.Context())
	if err != nil || raw == "" {
		t.Fatal("stage signer missing", raw, err)
	}
	// The outgoing stage token is accepted by the same stage verifier (kid = sub = user).
	if caller, err := runtime.services.VerifyService(t.Context(), raw); err != nil || caller != "user" {
		t.Fatalf("stage round trip: %q %v", caller, err)
	}
	access, err := keys.Sign(t.Context(), "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if sub, err := runtime.users.VerifyUser(t.Context(), access); err != nil || sub != "user-1" {
		t.Fatalf("local user keys: %q %v", sub, err)
	}
	// Same trusted key and kid, but a foreign issuer or audience: refused.
	for name, o := range map[string]signing.Options{
		"foreign issuer":   {Issuer: "foreign-issuer", Audience: cfg.Signing.Audience},
		"foreign audience": {Issuer: cfg.Signing.Issuer, Audience: "foreign-audience"},
	} {
		o.PrivateKeyFile, o.KeyID, o.Clock = cfg.Signing.PrivateKeyFile, cfg.Signing.KeyID, time.Now
		other, err := signing.Load(o)
		if err != nil {
			t.Fatal(err)
		}
		token, err := other.Sign(t.Context(), "user-1")
		if err != nil {
			t.Fatal(err)
		}
		if sub, err := runtime.users.VerifyUser(t.Context(), token); err == nil || sub != "" {
			t.Fatalf("%s accepted: %q", name, sub)
		}
	}
	if len(runtime.jobs) != 1 || runtime.jobs[0].Name != "identity-keys" {
		t.Fatalf("key refresh job: %+v", runtime.jobs)
	}
	for name, mutate := range map[string]func(*config.Config){
		"missing stage keys":   func(c *config.Config) { c.Auth.Stage.PublicKeysFile = filepath.Join(t.TempDir(), "missing") },
		"missing stage issuer": func(c *config.Config) { c.Auth.Stage.Issuer = "" },
		"missing signing file": func(c *config.Config) { c.Auth.Stage.SigningKeyFile = "" },
		"dev environment":      func(c *config.Config) { c.Env = config.EnvDev },
		"unknown environment":  func(c *config.Config) { c.Env = "qa" },
	} {
		c := cfg
		mutate(&c)
		if r, err := buildIdentity(c, keys, prometheus.NewRegistry()); err == nil {
			r.close()
			t.Fatalf("%s accepted", name)
		}
	}

	prod := config.Config{Env: config.EnvProd, Service: "user", Signing: cfg.Signing, Auth: config.AuthConfig{Mode: config.AuthVerify}}
	dir := t.TempDir()
	serviceToken, apiToken := filepath.Join(dir, "service-token"), filepath.Join(dir, "api-token")
	for path, body := range map[string]string{serviceToken: "test-projected-token", apiToken: "test-api-token"} {
		if err = os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prod.Auth.ServiceTokenFile = serviceToken
	prod.Auth.Kubernetes = config.KubernetesAuthConfig{APITokenFile: apiToken, CAFile: filepath.Join(dir, "ca.pem"), JWKSURL: "https://kubernetes.example.test/openid/v1/jwks"}
	if _, err = buildIdentity(prod, keys, prometheus.NewRegistry()); err == nil {
		t.Fatal("missing CA accepted")
	}
	if err = os.WriteFile(prod.Auth.Kubernetes.CAFile, []byte("invalid certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = buildIdentity(prod, keys, prometheus.NewRegistry()); err == nil {
		t.Fatal("invalid CA accepted")
	}
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, pub, private)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(prod.Auth.Kubernetes.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err = buildIdentity(prod, keys, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.close()
	if raw, err := runtime.tokens.Token(t.Context()); err != nil || raw != "test-projected-token" {
		t.Fatal("projected token missing", raw, err)
	}
	if len(runtime.jobs) != 1 {
		t.Fatal("one assembly job refreshes both key caches")
	}
	for name, mutate := range map[string]func(*config.Config){
		"insecure JWKS endpoint": func(c *config.Config) { c.Auth.Kubernetes.JWKSURL = "http://insecure.example.test/jwks" },
		"shared token file":      func(c *config.Config) { c.Auth.ServiceTokenFile = apiToken },
		"stage keys in prod":     func(c *config.Config) { c.Auth.Stage.Issuer = "stage" },
	} {
		c := prod
		mutate(&c)
		if r, err := buildIdentity(c, keys, prometheus.NewRegistry()); err == nil {
			r.close()
			t.Fatalf("%s accepted", name)
		}
	}
	reg := prometheus.NewRegistry()
	first, err := buildIdentity(prod, keys, reg)
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	if _, err = buildIdentity(prod, keys, reg); err == nil {
		t.Fatal("duplicate identity metrics accepted")
	}
}
