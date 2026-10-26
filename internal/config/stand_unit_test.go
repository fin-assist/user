package config

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/user/internal/signing"
)

// standUnit is the settings the example systemd unit of the stand gives the process:
// active Environment= lines with %d resolved to the credentials directory, and the
// LoadCredential= names that directory holds.
type standUnit struct {
	env         map[string]string
	credentials map[string]bool
}

// readStandUnit parses deploy/systemd/user.service, resolving %d to credentials.
func readStandUnit(t *testing.T, credentials string) standUnit {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "deploy", "systemd", "user.service"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	unit := standUnit{env: map[string]string{}, credentials: map[string]bool{}}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if value, ok := strings.CutPrefix(line, "Environment="); ok {
			name, setting, found := strings.Cut(value, "=")
			if !found {
				t.Fatalf("malformed unit line %q", line)
			}
			if _, dup := unit.env[name]; dup {
				t.Fatalf("unit sets %s twice", name)
			}
			unit.env[name] = strings.ReplaceAll(setting, "%d", credentials)
		}
		if value, ok := strings.CutPrefix(line, "LoadCredential="); ok {
			name, source, found := strings.Cut(value, ":")
			if !found || name == "" || source == "" {
				t.Fatalf("malformed unit line %q", line)
			}
			unit.credentials[name] = true
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return unit
}

// writeCredential places one credential as systemd does: an owner-only regular file in
// the credentials directory, readable by the DynamicUser of the unit.
func writeCredential(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o400); err != nil {
		t.Fatal(err)
	}
}

// FIN-349: with ENV=stage and DynamicUser=yes the process reads only what the unit hands it.
// Every private file the configuration requires must come from a LoadCredential= of the
// unit (a root-owned 0600 file is unreadable to the dynamic user, and loadAESKey refuses a
// wider mode), the env file must not override the unit, and the stand configuration loads.
func TestUnitStandUnitMatchesStageConfiguration(t *testing.T) {
	credentials := t.TempDir()
	unit := readStandUnit(t, credentials)
	if unit.env["ENV"] != "stage" {
		t.Fatalf("unit ENV=%q, want stage", unit.env["ENV"])
	}
	// SERVICE_NAME is the kid/sub of the stand signing key, fixed by the unit itself.
	if unit.env["SERVICE_NAME"] != "user" {
		t.Fatalf("unit SERVICE_NAME=%q, want user", unit.env["SERVICE_NAME"])
	}
	// Private files the stage configuration requires: each must be a loaded credential.
	private := []string{"DB_PRIMARY_DSN_FILE", "JWT_SIGNING_KEY_FILE", "AUTH_IDEMPOTENCY_KEY_FILE", "PHONE_RESULT_KEY_FILE", "AUTH_STAGE_SIGNING_KEY_FILE"}
	for _, name := range private {
		if !strings.HasPrefix(unit.env[name], credentials+string(filepath.Separator)) {
			t.Fatalf("unit gives %s=%q, want a path in the credentials directory", name, unit.env[name])
		}
	}
	// The shared stand JWKS is public (0644) and read directly from one path.
	if unit.env["AUTH_STAGE_PUBLIC_KEYS_FILE"] != "/etc/finassist/stage-public.jwks" {
		t.Fatalf("unit AUTH_STAGE_PUBLIC_KEYS_FILE=%q", unit.env["AUTH_STAGE_PUBLIC_KEYS_FILE"])
	}
	for name, value := range unit.env {
		if credential, ok := strings.CutPrefix(value, credentials+string(filepath.Separator)); ok && !unit.credentials[credential] {
			t.Fatalf("%s names credential %q that the unit does not load", name, credential)
		}
	}

	// The env file of the stand: only the non-secret settings named in the unit comments.
	envFile := map[string]string{
		"JWT_KEY_ID": "stage-1", "JWT_ISSUER": "https://user.example", "JWT_AUDIENCE": "finassist",
		"AUTH_STAGE_ISSUER":             "stage-services",
		"PEER_BANK_STATEMENT_DATA_ADDR": "dns:///127.0.0.1:9092", "PEER_ANALYTICS_ADDR": "dns:///127.0.0.1:9093", "PEER_ASSISTANT_ADDR": "dns:///127.0.0.1:9094",
	}
	// Merge in systemd order: Environment= of the unit first, EnvironmentFile= over it.
	// An env file that repeated a unit setting would silently replace it, so refuse that.
	vars := map[string]string{}
	for name, value := range unit.env {
		vars[name] = value
	}
	for name, value := range envFile {
		if _, ok := unit.env[name]; ok {
			t.Fatalf("the env file would override the unit setting %s", name)
		}
		vars[name] = value
	}

	writeCredential(t, unit.env["DB_PRIMARY_DSN_FILE"], []byte("postgres://primary/user\n"))
	writeCredential(t, unit.env["AUTH_IDEMPOTENCY_KEY_FILE"], bytes.Repeat([]byte{1}, 32))
	writeCredential(t, unit.env["PHONE_RESULT_KEY_FILE"], bytes.Repeat([]byte{2}, 32))
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writeCredential(t, unit.env["JWT_SIGNING_KEY_FILE"], pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))

	get, list := env(vars)
	c, err := load(get, os.ReadFile, list)
	if err != nil {
		t.Fatalf("stand unit configuration rejected: %v", err)
	}
	if c.Env != EnvStage || c.Auth.Mode != AuthVerify || c.Service != "user" || c.DB.PrimaryDSN != "postgres://primary/user" {
		t.Fatalf("stand runs ENV=%s AUTH_MODE=%s SERVICE_NAME=%s", c.Env, c.Auth.Mode, c.Service)
	}
	if c.Auth.Stage.SigningKeyFile != filepath.Join(credentials, "stage-signing") || c.Auth.Stage.PublicKeysFile == "" {
		t.Fatalf("stage key files: %+v", c.Auth.Stage)
	}
	// The private keys the service opens after load pass their owner-only checks.
	if _, err := LoadSessionKey(c.Sessions); err != nil {
		t.Fatalf("AUTH_IDEMPOTENCY_KEY_FILE from the unit: %v", err)
	}
	if _, err := LoadPhoneKey(c.Phone); err != nil {
		t.Fatalf("PHONE_RESULT_KEY_FILE from the unit: %v", err)
	}
	if _, err := signing.Load(signing.Options{PrivateKeyFile: c.Signing.PrivateKeyFile, KeyID: c.Signing.KeyID, Issuer: c.Signing.Issuer, Audience: c.Signing.Audience, Clock: time.Now}); err != nil {
		t.Fatalf("JWT_SIGNING_KEY_FILE from the unit: %v", err)
	}
}
