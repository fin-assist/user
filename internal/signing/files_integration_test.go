package signing_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	identity "github.com/fin-assist/go-identity"

	"github.com/fin-assist/user/internal/signing"
)

func writeSigningKey(t *testing.T, path string) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func publicSnapshot(t *testing.T, keys *signing.Keys) []byte {
	t.Helper()
	data, err := keys.JWKS(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestIntegrationSigningKeyDeploymentRetainsImmutableSnapshotUntilRestart(t *testing.T) {
	dir := t.TempDir()
	activePath, retainedPath := filepath.Join(dir, "active.pem"), filepath.Join(dir, "retained.json")
	writeSigningKey(t, activePath)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	opts := signing.Options{PrivateKeyFile: activePath, KeyID: "previous", Issuer: "user", Audience: "app", Clock: clock}
	previous, err := signing.Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	previousToken, err := previous.Sign(t.Context(), "account-1")
	if err != nil {
		t.Fatal(err)
	}
	previousPublic := publicSnapshot(t, previous)
	if err := os.WriteFile(retainedPath, previousPublic, 0o600); err != nil {
		t.Fatal(err)
	}

	// An operator replaces the file atomically. The running process retains its
	// immutable signer/JWKS until the coordinated restart loads the new snapshot.
	staged := filepath.Join(dir, "next.pem")
	writeSigningKey(t, staged)
	if err := os.Rename(staged, activePath); err != nil {
		t.Fatal(err)
	}
	if got := publicSnapshot(t, previous); !bytes.Equal(got, previousPublic) {
		t.Fatal("file replacement mutated the running snapshot")
	}
	opts.KeyID, opts.PublicKeysFile = "current", retainedPath
	current, err := signing.Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	currentToken, err := current.Sign(t.Context(), "account-1")
	if err != nil {
		t.Fatal(err)
	}
	cache, err := identity.NewKeyCache(current, clock, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := identity.NewUserVerifier(cache, identity.VerifyOptions{Issuer: "user", Audience: "app", Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{previousToken, currentToken} {
		if user, err := verifier.VerifyUser(t.Context(), token); err != nil || user != "account-1" {
			t.Fatalf("rotation rejected a valid session: %q %v", user, err)
		}
	}
	public := publicSnapshot(t, current)
	if strings.Contains(string(public), `"d"`) {
		t.Fatal("public snapshot exposed private material")
	}
	if err := os.WriteFile(retainedPath, public, 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := signing.Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	var set struct {
		Keys []struct {
			ID string `json:"kid"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(publicSnapshot(t, reloaded), &set); err != nil {
		t.Fatal(err)
	}
	if len(set.Keys) != 2 || set.Keys[0].ID != "current" || set.Keys[1].ID != "previous" {
		t.Fatalf("restart duplicated or lost a retained key: %+v", set)
	}
	opts.KeyID = "previous"
	if _, err := signing.Load(opts); err == nil {
		t.Fatal("new private key reused another key's ID")
	}
	// A failed deployment must not invalidate sessions served by the old process.
	if user, err := verifier.VerifyUser(t.Context(), previousToken); err != nil || user != "account-1" {
		t.Fatal("failed reload damaged the running verifier", err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := current.Sign(cancelled, "account-1"); err == nil {
		t.Fatal("cancelled request issued an access token")
	}
}

func TestIntegrationSigningKeyFilesRejectUnsafeDeploymentInputs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "active.pem")
	writeSigningKey(t, path)
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	opts := signing.Options{PrivateKeyFile: path, KeyID: "current", Issuer: "user", Audience: "app", Clock: time.Now}
	for name, arrange := range map[string]func(t *testing.T){
		"world-readable": func(t *testing.T) {
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"malformed-PEM": func(t *testing.T) {
			if err := os.WriteFile(path, []byte("PRIVATE DEPLOYMENT SECRET"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"trailing-PEM": func(t *testing.T) {
			if err := os.WriteFile(path, append(append([]byte(nil), good...), []byte("PRIVATE DEPLOYMENT SECRET")...), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"oversized": func(t *testing.T) {
			if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 8193), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"missing": func(t *testing.T) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, good, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			arrange(t)
			keys, err := signing.Load(opts)
			if err == nil || keys != nil {
				t.Fatal("unsafe deployment input accepted")
			}
			if strings.Contains(err.Error(), "PRIVATE DEPLOYMENT SECRET") || strings.Contains(err.Error(), path) {
				t.Fatal("deployment diagnostic leaked file data or path")
			}
		})
	}
}

func TestIntegrationSigningRetainedFileRejectsPrivateOrAmbiguousJWKS(t *testing.T) {
	dir := t.TempDir()
	active, public := filepath.Join(dir, "active.pem"), filepath.Join(dir, "retained.json")
	writeSigningKey(t, active)
	opts := signing.Options{PrivateKeyFile: active, KeyID: "current", Issuer: "user", Audience: "app", Clock: time.Now}
	keys, err := signing.Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	valid := publicSnapshot(t, keys)
	var set struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(valid, &set); err != nil {
		t.Fatal(err)
	}
	key := set.Keys[0]
	key["d"] = "PRIVATE DEPLOYMENT SECRET"
	private, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	delete(key, "d")
	set.Keys = append(set.Keys, key)
	duplicate, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	opts.PublicKeysFile = public
	for name, data := range map[string][]byte{"private-material": private, "duplicate-ID": duplicate, "trailing-document": append(append([]byte(nil), valid...), []byte(" {}")...), "empty-set": []byte(`{"keys":[]}`)} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(public, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if got, err := signing.Load(opts); err == nil || got != nil {
				t.Fatal("untrusted retained key file accepted")
			} else if strings.Contains(err.Error(), "PRIVATE DEPLOYMENT SECRET") {
				t.Fatal("private material leaked in error")
			}
		})
	}
}
