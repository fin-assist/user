package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/user/internal/adapter/postgres/migrate"
	"github.com/fin-assist/user/internal/adapter/postgres/migrate/migratetest"
	"github.com/fin-assist/user/internal/app"
)

func TestUnitServeFailsBeforeConnectingWithoutSigningKey(t *testing.T) {
	t.Setenv("SERVICE_NAME", "user")
	t.Setenv("DB_PRIMARY_DSN", "postgres://nowhere.invalid/db")
	t.Setenv("ENV", "prod")
	err := runServe(t.Context())
	if err == nil || !strings.Contains(err.Error(), "signing:") {
		t.Fatalf("error %v", err)
	}
}

// The whole serve path against a real database: config, pools, server, ops listener,
// readiness and an orderly stop on cancel.
func TestIntegrationServeRunsAndStops(t *testing.T) {
	for _, provider := range []string{"disabled", "zvonok"} {
		t.Run(provider, func(t *testing.T) { serveAndStop(t, provider) })
	}
}
func serveAndStop(t *testing.T, provider string) {
	t.Helper()
	t.Setenv("PEER_BANK_STATEMENT_DATA_ADDR", "passthrough:///127.0.0.1:1")
	t.Setenv("PEER_ANALYTICS_ADDR", "passthrough:///127.0.0.1:1")
	t.Setenv("PEER_ASSISTANT_ADDR", "passthrough:///127.0.0.1:1")
	setSigningKey(t)
	setPhoneKey(t)
	dsn := migratetest.DSN(t)
	if err := migrate.Up(t.Context(), migrate.DefaultConfig(dsn), app.MigrationSources()); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"SERVICE_NAME": "user", "ENV": "dev", "AUTH_MODE": "insecure-dev", "LOG_LEVEL": "error", "DB_PRIMARY_DSN": dsn, "GRPC_ADDR": "127.0.0.1:0", "HTTP_ADDR": "127.0.0.1:0", "SHUTDOWN_DELAY": "0s", "SHUTDOWN_TIMEOUT": "5s", "PHONE_PROVIDER": provider} {
		t.Setenv(k, v)
	}
	addr := ""
	if provider == "zvonok" {
		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr = lis.Addr().String()
		_ = lis.Close()
		t.Setenv("ZVONOK_CALLBACK_ADDR", addr)
		t.Setenv("ZVONOK_PUBLIC_KEY", "fake-test-key")
		t.Setenv("ZVONOK_CAMPAIGN_ID", "123")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- runServe(ctx) }()
	if provider == "zvonok" {
		deadline := time.Now().Add(5 * time.Second)
		ready := false
		for time.Now().Before(deadline) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/callbacks/zvonok?ct_call_id=123", nil)
			if err != nil {
				t.Fatal(err)
			}
			r, err := http.DefaultClient.Do(req)
			if err == nil {
				_ = r.Body.Close()
				if r.StatusCode != 403 {
					t.Fatal(r.StatusCode)
				}
				ready = true
				break
			}
			select {
			case err := <-errc:
				t.Fatal("serve stopped", err)
			case <-time.After(10 * time.Millisecond):
			}
		}
		if !ready {
			t.Fatal("callback listener did not start")
		}
		cancel()
	} else {
		time.AfterFunc(500*time.Millisecond, cancel)
	}
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal("serve", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
}

func TestIntegrationServeFailsOnBadInput(t *testing.T) {
	setSigningKey(t)
	setPhoneKey(t)
	base := map[string]string{"SERVICE_NAME": "user", "ENV": "dev", "AUTH_MODE": "insecure-dev", "LOG_LEVEL": "error",
		"PEER_BANK_STATEMENT_DATA_ADDR": "passthrough:///127.0.0.1:1", "PEER_ANALYTICS_ADDR": "passthrough:///127.0.0.1:1", "PEER_ASSISTANT_ADDR": "passthrough:///127.0.0.1:1"}
	cases := map[string]map[string]string{
		"config":   {"DB_PRIMARY_DSN": ""},
		"database": {"DB_PRIMARY_DSN": "postgres://nobody@127.0.0.1:1/db?connect_timeout=1"},
		"listen":   {"DB_PRIMARY_DSN": migratetest.DSN(t), "GRPC_ADDR": "256.0.0.1:1"},
		"outbox":   {"DB_PRIMARY_DSN": migratetest.DSN(t), "PEER_ASSISTANT_ADDR": ""},
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			for k, v := range base {
				t.Setenv(k, v)
			}
			for k, v := range extra {
				t.Setenv(k, v)
			}
			if err := runServe(t.Context()); err == nil {
				t.Fatal("want an error")
			} else if name == "outbox" && !strings.Contains(err.Error(), "peer address required") {
				t.Fatal(err)
			}
		})
	}
}

func setSigningKey(t *testing.T) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JWT_SIGNING_KEY_FILE", path)
	t.Setenv("JWT_KEY_ID", "serve-test")
	t.Setenv("JWT_ISSUER", "test-user")
	t.Setenv("JWT_AUDIENCE", "test-app")
	secretPath := filepath.Join(t.TempDir(), "auth.key")
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretPath, secret[:], 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTH_IDEMPOTENCY_KEY_FILE", secretPath)
}

func setPhoneKey(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "phone.key")
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PHONE_RESULT_KEY_FILE", path)
}
