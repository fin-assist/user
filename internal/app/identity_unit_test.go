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

func TestUnitIdentityConstructionUsesLocalStageFilesAndDefersKubernetesFetch(t *testing.T) {
	cfg := config.Config{Env: config.EnvStage, Service: "user", Signing: signingConfig(t), Auth: config.AuthConfig{Mode: config.AuthVerify, ServiceMode: "stage", ServiceIssuer: "stage-issuer"}}
	keys, err := signing.Load(signing.Options{PrivateKeyFile: cfg.Signing.PrivateKeyFile, KeyID: cfg.Signing.KeyID, Issuer: cfg.Signing.Issuer, Audience: cfg.Signing.Audience, Clock: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	public, err := keys.JWKS(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Auth.StagePublicKeysFile = filepath.Join(t.TempDir(), "stage.json")
	if err = os.WriteFile(cfg.Auth.StagePublicKeysFile, public, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Auth.StageKeyFile = cfg.Signing.PrivateKeyFile
	runtime, err := buildIdentity(cfg, keys, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.close()
	if raw, err := runtime.tokens.Token(t.Context()); err != nil || raw == "" {
		t.Fatal("stage signer missing", raw, err)
	}
	if len(runtime.jobs) != 1 {
		t.Fatal("local key refresh worker missing")
	}
	cfg.Auth.StagePublicKeysFile = filepath.Join(t.TempDir(), "missing")
	if _, err = buildIdentity(cfg, keys, prometheus.NewRegistry()); err == nil {
		t.Fatal("missing stage keys accepted")
	}
	cfg.Auth.ServiceMode = "unknown"
	if _, err = buildIdentity(cfg, keys, prometheus.NewRegistry()); err == nil {
		t.Fatal("unknown service identity mode accepted")
	}
	cfg.Auth.ServiceMode = "kubernetes"
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, "token")
	if err = os.WriteFile(tokenPath, []byte("test-projected-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Auth.ServiceTokenFile = tokenPath
	cfg.Auth.KubernetesTokenFile = tokenPath
	cfg.Auth.KubernetesCAFile = filepath.Join(dir, "ca.pem")
	cfg.Auth.KubernetesJWKSURL = "https://kubernetes.example.test/openid/v1/jwks"
	if _, err = buildIdentity(cfg, keys, prometheus.NewRegistry()); err == nil {
		t.Fatal("missing CA accepted")
	}
	if err = os.WriteFile(cfg.Auth.KubernetesCAFile, []byte("invalid certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = buildIdentity(cfg, keys, prometheus.NewRegistry()); err == nil {
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
	if err = os.WriteFile(cfg.Auth.KubernetesCAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err = buildIdentity(cfg, keys, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.close()
	if raw, err := runtime.tokens.Token(t.Context()); err != nil || raw != "test-projected-token" {
		t.Fatal("projected token missing", raw, err)
	}
	if len(runtime.jobs) != 2 {
		t.Fatal("Kubernetes key refresh worker missing")
	}
	cfg.Auth.KubernetesJWKSURL = "http://insecure.example.test/jwks"
	if _, err = buildIdentity(cfg, keys, prometheus.NewRegistry()); err == nil {
		t.Fatal("insecure JWKS endpoint accepted")
	}
	reg := prometheus.NewRegistry()
	cfg.Auth.ServiceMode = "unknown"
	_, _ = buildIdentity(cfg, keys, reg)
	if _, err = buildIdentity(cfg, keys, reg); err == nil {
		t.Fatal("duplicate identity metrics accepted")
	}
}
