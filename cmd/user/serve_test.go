package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/user/internal/adapter/postgres/migrate/migratetest"
)

func TestServeFailsBeforeConnectingWithoutSigningKey(t *testing.T) {
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
func TestServeRunsAndStops(t *testing.T) {
	setSigningKey(t)
	dsn := migratetest.DSN(t)
	for k, v := range map[string]string{
		"SERVICE_NAME": "user", "ENV": "dev", "AUTH_MODE": "insecure-dev", "LOG_LEVEL": "error",
		"DB_PRIMARY_DSN": dsn, "GRPC_ADDR": "127.0.0.1:0", "HTTP_ADDR": "127.0.0.1:0",
		"SHUTDOWN_DELAY": "0s", "SHUTDOWN_TIMEOUT": "5s",
	} {
		t.Setenv(k, v)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { errc <- runServe(ctx) }()
	// No port to probe (":0"), so give the readiness check its first round, then stop.
	time.AfterFunc(500*time.Millisecond, cancel)
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not stop after cancel")
	}
}

func TestServeFailsOnBadInput(t *testing.T) {
	setSigningKey(t)
	base := map[string]string{"SERVICE_NAME": "user", "ENV": "dev", "AUTH_MODE": "insecure-dev", "LOG_LEVEL": "error"}
	cases := map[string]map[string]string{
		"config":   {"DB_PRIMARY_DSN": ""},
		"database": {"DB_PRIMARY_DSN": "postgres://nobody@127.0.0.1:1/db?connect_timeout=1"},
		"listen":   {"DB_PRIMARY_DSN": migratetest.DSN(t), "GRPC_ADDR": "256.0.0.1:1"},
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
			}
		})
	}
}

func setSigningKey(t *testing.T) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil { t.Fatal(err) }
	der,err:=x509.MarshalPKCS8PrivateKey(key)
	if err!=nil {t.Fatal(err)}
	path:=filepath.Join(t.TempDir(),"key.pem")
	if err:=os.WriteFile(path,pem.EncodeToMemory(&pem.Block{Type:"PRIVATE KEY",Bytes:der}),0o600);err!=nil{t.Fatal(err)}
	t.Setenv("JWT_SIGNING_KEY_FILE",path)
	t.Setenv("JWT_KEY_ID","serve-test")
	t.Setenv("JWT_ISSUER","test-user")
	t.Setenv("JWT_AUDIENCE","test-app")
}
