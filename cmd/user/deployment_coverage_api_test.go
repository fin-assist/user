package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/fin-assist/user/internal/adapter/postgres/migrate/migratetest"
)

// Exercise the deployed entrypoint and its public endpoint across failed restarts.
// Pure configuration and key parsing tests remain in the isolated unit suite.
func TestAPIDeploymentRejectsUnsafeRestartsBeforeExposingSigningKeys(t *testing.T) {
	setSigningKey(t)
	setPhoneKey(t)
	dsn := migratetest.DSN(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	for name, value := range map[string]string{
		"SERVICE_NAME": "user", "ENV": "dev", "AUTH_MODE": "insecure-dev", "LOG_LEVEL": "error",
		"DB_PRIMARY_DSN": dsn, "GRPC_ADDR": address, "HTTP_ADDR": "127.0.0.1:0", "PHONE_PROVIDER": "disabled",
		"SHUTDOWN_DELAY": "0s", "SHUTDOWN_TIMEOUT": "3s",
		"PEER_BANK_STATEMENT_DATA_ADDR": "passthrough:///127.0.0.1:1", "PEER_ANALYTICS_ADDR": "passthrough:///127.0.0.1:1", "PEER_ASSISTANT_ADDR": "passthrough:///127.0.0.1:1",
	} {
		t.Setenv(name, value)
	}
	if err := run(t.Context(), []string{"migrate", "up"}); err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	client := userv1.NewUserInternalServiceClient(conn)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"serve"}) }()
	requestCtx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	requestCtx = metadata.AppendToOutgoingContext(requestCtx, "x-service-authorization", "Bearer dev:assistant")
	response, err := client.GetSigningKeys(requestCtx, &userv1.GetSigningKeysRequest{}, grpc.WaitForReady(true))
	stop()
	cancel()
	select {
	case serveErr := <-done:
		if serveErr != nil {
			t.Fatal(serveErr)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("healthy deployment did not stop")
	}
	if err != nil || !strings.Contains(response.GetJwksJson(), "serve-test") {
		t.Fatalf("baseline deployment did not expose its actual public API: %v %v", response, err)
	}
	dir := t.TempDir()
	invalid, publicMismatch, privatePublic, shortAES := filepath.Join(dir, "invalid.pem"), filepath.Join(dir, "mismatched.jwks"), filepath.Join(dir, "private.jwks"), filepath.Join(dir, "short-aes")
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	mismatch, err := json.Marshal(map[string]any{"keys": []any{map[string]string{"kty": "OKP", "crv": "Ed25519", "kid": "serve-test", "x": base64.RawURLEncoding.EncodeToString(public)}}})
	if err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string][]byte{invalid: []byte("invalid-key"), publicMismatch: mismatch, privatePublic: []byte(`{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"retained","x":"invalid","d":"must-never-be-published"}]}`), shortAES: []byte("short")} {
		if err := os.WriteFile(path, value, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, want string
		env        map[string]string
	}{
		{"missing-signing-key", "key file unavailable", map[string]string{"JWT_SIGNING_KEY_FILE": filepath.Join(dir, "missing")}},
		{"invalid-signing-key", "invalid PKCS#8", map[string]string{"JWT_SIGNING_KEY_FILE": invalid}},
		{"mismatched-public-key", "active public key mismatch", map[string]string{"JWT_PUBLIC_KEYS_FILE": publicMismatch}},
		{"private-jwks-field", "invalid public JWKS", map[string]string{"JWT_PUBLIC_KEYS_FILE": privatePublic}},
		{"result-encryption-key", "phone result", map[string]string{"PHONE_RESULT_KEY_FILE": shortAES}},
		{"idempotency-encryption-key", "auth: key must contain exactly 32 raw bytes", map[string]string{"AUTH_IDEMPOTENCY_KEY_FILE": shortAES}},
		{"production-dev-identities", "insecure-dev", map[string]string{"ENV": "prod"}},
		{"production-fake-phone", "fake", map[string]string{"ENV": "prod", "PHONE_PROVIDER": "fake"}},
		{"database-unavailable", "postgres", map[string]string{"DB_PRIMARY_DSN": "postgres://postgres@127.0.0.1:1/missing?sslmode=disable&connect_timeout=1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			startupCtx, stopStartup := context.WithTimeout(t.Context(), 3*time.Second)
			err := run(startupCtx, []string{"serve"})
			stopStartup()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("unsafe restart accepted or failed for the wrong reason: %v", err)
			}
			requestCtx, stop := context.WithTimeout(t.Context(), 250*time.Millisecond)
			defer stop()
			requestCtx = metadata.AppendToOutgoingContext(requestCtx, "x-service-authorization", "Bearer dev:assistant")
			if response, err := client.GetSigningKeys(requestCtx, &userv1.GetSigningKeysRequest{}); err == nil || response != nil {
				t.Fatalf("rejected deployment still exposed signing keys: %v %v", response, err)
			}
		})
	}
}
