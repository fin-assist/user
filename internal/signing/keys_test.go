package signing

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	identity "github.com/fin-assist/go-identity"
)

func keyFile(t *testing.T) string {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUnitRotationAndPublicKeyValidation(t *testing.T) {
	now := time.Now
	old, err := Load(Options{PrivateKeyFile: keyFile(t), KeyID: "old", Issuer: "user", Audience: "app", Clock: now})
	if err != nil {
		t.Fatal(err)
	}
	oldToken, err := old.Sign(t.Context(), "u")
	if err != nil {
		t.Fatal(err)
	}
	b, err := old.JWKS(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "public.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	o := Options{PrivateKeyFile: keyFile(t), PublicKeysFile: path, KeyID: "new", Issuer: "user", Audience: "app", Clock: now}
	current, err := Load(o)
	if err != nil {
		t.Fatal(err)
	}
	cache, err := identity.NewKeyCache(current, now, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	v, err := identity.NewUserVerifier(cache, identity.VerifyOptions{Issuer: "user", Audience: "app", Clock: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{oldToken, mustSign(t, current)} {
		if id, err := v.VerifyUser(t.Context(), token); err != nil || id != "u" {
			t.Fatalf("rotation: %q %v", id, err)
		}
	}
	public, err := current.JWKS(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	public[0] = '!'
	if b, err := current.JWKS(t.Context()); err != nil || b[0] != '{' {
		t.Fatal("caller mutated keys")
	}
	stage, err := StagePublicKeys(path)
	if err != nil || len(stage) != 1 || len(stage["old"]) != 32 {
		t.Fatalf("stage map: %v", err)
	}
	o.KeyID = "old"
	if _, err := Load(o); err == nil {
		t.Fatal("active key mismatch accepted")
	}
	// Republish the same active public key only once.
	o.PrivateKeyFile = old.options.PrivateKeyFile
	if k, err := Load(o); err != nil {
		t.Fatal(err)
	} else if b, _ := k.JWKS(t.Context()); strings.Count(string(b), `"kid"`) != 1 {
		t.Fatalf("duplicate active: %s", b)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := current.Sign(ctx, "u"); err == nil {
		t.Fatal("cancelled sign accepted")
	}
	if _, err := current.JWKS(ctx); err == nil {
		t.Fatal("cancelled keys accepted")
	}
	if _, err := current.Sign(t.Context(), ""); err == nil {
		t.Fatal("empty subject accepted")
	}
}

func mustSign(t *testing.T, k *Keys) string {
	t.Helper()
	s, err := k.Sign(t.Context(), "u")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestUnitMalformedKeyFiles(t *testing.T) {
	o := Options{PrivateKeyFile: keyFile(t), KeyID: "key", Issuer: "user", Audience: "app", Clock: time.Now}
	for _, body := range []string{"not a key", string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: []byte("x")})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("x")})), strings.Repeat("x", 8193)} {
		if err := os.WriteFile(o.PrivateKeyFile, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(o); err == nil {
			t.Fatal("invalid private key accepted")
		}
	}
	pub := filepath.Join(t.TempDir(), "jwks")
	x := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	valid := `{"kty":"OKP","crv":"Ed25519","x":"` + x + `","kid":"k"}`
	for _, body := range []string{
		`garbage`, `{"keys":[]}`, `{"keys":[` + valid + `]} {}`,
		`{"keys":[` + valid + `,` + valid + `]}`,
		`{"keys":[` + strings.Replace(valid, `"kid":"k"`, `"kid":""`, 1) + `]}`,
		`{"keys":[` + strings.Replace(valid, `"crv":"Ed25519"`, `"crv":"P-256"`, 1) + `]}`,
		`{"keys":[` + strings.Replace(valid, `"kid":"k"`, `"kid":"k","d":"SECRET"`, 1) + `]}`,
		`{"keys":[` + strings.Replace(valid, `"kid":"k"`, `"kid":"k","key_ops":["sign"]`, 1) + `]}`,
		strings.Repeat("x", (1<<20)+1),
	} {
		if err := os.WriteFile(pub, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := StagePublicKeys(pub); err == nil {
			t.Fatal("invalid public key accepted")
		}
	}
	if _, err := StagePublicKeys("/missing"); err == nil {
		t.Fatal("missing public keys accepted")
	}
}

func TestUnitPublicKeyCountIncludesActiveKey(t *testing.T) {
	var extra keySet
	for i := 0; i < 64; i++ {
		extra.Keys = append(extra.Keys, publicKey{Type: "OKP", Curve: "Ed25519", ID: fmt.Sprintf("k-%d", i), X: base64.RawURLEncoding.EncodeToString(make([]byte, 32))})
	}
	b, err := json.Marshal(extra)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(Options{PrivateKeyFile: keyFile(t), PublicKeysFile: path, KeyID: "active", Issuer: "user", Audience: "app", Clock: time.Now}); err == nil {
		t.Fatal("65 published keys accepted")
	}
}

func TestUnitSignAndVerify(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	k, err := Load(Options{PrivateKeyFile: keyFile(t), KeyID: "current", Issuer: "test-user", Audience: "test-app", Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	cache, err := identity.NewKeyCache(k, clock, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	v, err := identity.NewUserVerifier(cache, identity.VerifyOptions{Issuer: "test-user", Audience: "test-app", Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	token, err := k.Sign(context.Background(), "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if sub, err := v.VerifyUser(context.Background(), token); err != nil || sub != "user-1" {
		t.Fatalf("verify: %q %v", sub, err)
	}
	now = now.Add(15 * time.Minute)
	if _, err := v.VerifyUser(context.Background(), token); err == nil {
		t.Fatal("expired token accepted")
	}
	b, err := k.JWKS(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Keys) != 1 || body.Keys[0]["kid"] != "current" || body.Keys[0]["d"] != nil {
		t.Fatalf("public keys: %s", b)
	}
}

func TestUnitInvalidOptions(t *testing.T) {
	good := Options{PrivateKeyFile: keyFile(t), KeyID: "key", Issuer: "user", Audience: "app", Clock: time.Now}
	for _, mutate := range []func(*Options){
		func(o *Options) { o.PrivateKeyFile = "" }, func(o *Options) { o.KeyID = "" },
		func(o *Options) { o.Issuer = "" }, func(o *Options) { o.Audience = "" }, func(o *Options) { o.Clock = nil },
		func(o *Options) { o.PublicKeysFile = "/missing" },
	} {
		o := good
		mutate(&o)
		if _, err := Load(o); err == nil {
			t.Fatal("invalid options accepted")
		}
	}
	if err := os.Chmod(good.PrivateKeyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(good); err == nil {
		t.Fatal("publicly readable key accepted")
	}
}
