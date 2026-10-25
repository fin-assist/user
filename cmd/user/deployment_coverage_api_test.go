package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/fin-assist/user/internal/testdb"
)

// Exercise the deployed entrypoint and its public endpoint across failed restarts.
// Pure configuration and key parsing tests remain in the isolated unit suite.
func TestAPIDeploymentRejectsUnsafeRestartsBeforeExposingSigningKeys(t *testing.T) {
	setSigningKey(t)
	setPhoneKey(t)
	dsn := testdb.DSN(t)
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
	tlsFixture := httptest.NewTLSServer(http.NotFoundHandler())
	defer tlsFixture.Close()
	caFile := filepath.Join(dir, "kubernetes-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsFixture.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	wrongType, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	wrongDER, err := x509.MarshalPKCS8PrivateKey(wrongType)
	if err != nil {
		t.Fatal(err)
	}
	wrongTypeFile, oversizedFile, unsafeFile, trailingPublicFile, emptyPublicFile, public64File := filepath.Join(dir, "wrong-type.pem"), filepath.Join(dir, "oversized.pem"), filepath.Join(dir, "unsafe.pem"), filepath.Join(dir, "trailing.jwks"), filepath.Join(dir, "empty.jwks"), filepath.Join(dir, "64-public.jwks")
	actualPrivate, err := os.ReadFile(os.Getenv("JWT_SIGNING_KEY_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	retained := []any{}
	for i := range 64 {
		retained = append(retained, map[string]string{"kty": "OKP", "crv": "Ed25519", "kid": fmt.Sprintf("retained-%d", i), "x": base64.RawURLEncoding.EncodeToString(public)})
	}
	public64, err := json.Marshal(map[string]any{"keys": retained})
	if err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string][]byte{wrongTypeFile: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: wrongDER}), oversizedFile: []byte(strings.Repeat("x", 8193)), unsafeFile: actualPrivate, trailingPublicFile: []byte(`{"keys":[]} {}`), emptyPublicFile: []byte(`{"keys":[]}`), public64File: public64} {
		if err := os.WriteFile(path, value, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(unsafeFile, 0644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, want string
		env        map[string]string
	}{
		{"missing-service-name", "SERVICE_NAME", map[string]string{"SERVICE_NAME": ""}},
		{"unknown-environment", "ENV=", map[string]string{"ENV": "unknown"}},
		{"unknown-log-format", "LOG_FORMAT", map[string]string{"LOG_FORMAT": "yaml"}},
		{"unknown-log-level", "LOG_LEVEL", map[string]string{"LOG_LEVEL": "verbose"}},
		{"missing-database", "DB_PRIMARY_DSN", map[string]string{"DB_PRIMARY_DSN": ""}},
		{"zero-pool", "DB_MAX_CONNS", map[string]string{"DB_MAX_CONNS": "0"}},
		{"nonnumeric-pool", "not an integer", map[string]string{"DB_MAX_CONNS": "NaN"}},
		{"zero-receive-size", "GRPC_MAX_RECV_MSG_SIZE", map[string]string{"GRPC_MAX_RECV_MSG_SIZE": "0"}},
		{"unknown-auth-mode", "AUTH_MODE", map[string]string{"AUTH_MODE": "unknown"}},
		{"verified-identity-in-dev", "AUTH_MODE=verify requires ENV=stage or prod", map[string]string{"AUTH_MODE": "verify"}},
		{"production-stage-identity", "AUTH_STAGE_* variables are not allowed with ENV=prod", map[string]string{"ENV": "prod", "AUTH_MODE": "verify", "AUTH_STAGE_ISSUER": "stage-services"}},
		{"renamed-service-auth-mode", "SERVICE_AUTH_MODE is no longer read", map[string]string{"SERVICE_AUTH_MODE": "stage"}},
		{"renamed-stage-key", "use AUTH_STAGE_PUBLIC_KEYS_FILE", map[string]string{"STAGE_PUBLIC_KEYS_FILE": filepath.Join(dir, "stage.jwks")}},
		{"renamed-kubernetes-token", "use AUTH_KUBERNETES_API_TOKEN_FILE", map[string]string{"KUBERNETES_TOKEN_FILE": shortAES}},
		{"negative-shutdown-delay", "SHUTDOWN_DELAY", map[string]string{"SHUTDOWN_DELAY": "-1s"}},
		{"non-duration-shutdown", "SHUTDOWN_TIMEOUT", map[string]string{"SHUTDOWN_TIMEOUT": "soon"}},
		{"zero-watch-resync", "PHONE_WATCH_RESYNC_INTERVAL", map[string]string{"PHONE_WATCH_RESYNC_INTERVAL": "0s"}},
		{"invalid-watch-capacity", "phone:", map[string]string{"PHONE_WATCH_MAX_STREAMS": "0"}},
		{"invalid-phone-key-id", "phone:", map[string]string{"PHONE_RESULT_KEY_ID": "bad key id"}},
		{"zero-auth-pool", "AUTH_DB_MAX_CONNS", map[string]string{"AUTH_DB_MAX_CONNS": "0"}},
		{"zero-outbox-pool", "OUTBOX_DB_MAX_CONNS", map[string]string{"OUTBOX_DB_MAX_CONNS": "0"}},
		{"insufficient-outbox-pool", "insufficient pool", map[string]string{"OUTBOX_DB_MAX_CONNS": "1"}},
		{"duplicate-outbox-target", "duplicate target", map[string]string{"OUTBOX_TIMEZONE_TARGETS": "assistant,assistant"}},
		{"missing-required-deletion-target", "required deletion target", map[string]string{"OUTBOX_DELETION_TARGETS": "bank-statement-data,analytics,other"}},
		{"short-deletion-route", "invalid routes", map[string]string{"OUTBOX_DELETION_TARGETS": "assistant"}},
		{"zero-register-limit", "rate limits", map[string]string{"LIMIT_REGISTER_IP": "0"}},
		{"conflicting-secret-file", "both DB_PRIMARY_DSN", map[string]string{"DB_PRIMARY_DSN_FILE": filepath.Join(dir, "missing")}},
		{"unreadable-secret-file", "DB_PRIMARY_DSN_FILE", map[string]string{"DB_PRIMARY_DSN": "", "DB_PRIMARY_DSN_FILE": filepath.Join(dir, "missing")}},
		{"invalid-replica-dsn", "invalid DSN", map[string]string{"DB_REPLICA_DSN": "://bad"}},
		{"missing-result-key", "phone result", map[string]string{"PHONE_RESULT_KEY_FILE": filepath.Join(dir, "missing")}},
		{"shared-encryption-key", "must differ", map[string]string{"PHONE_RESULT_KEY_FILE": os.Getenv("AUTH_IDEMPOTENCY_KEY_FILE")}},
		{"unknown-phone-provider", "unknown provider", map[string]string{"PHONE_PROVIDER": "unknown"}},
		{"disabled-provider-credentials", "disabled provider", map[string]string{"ZVONOK_PUBLIC_KEY": "configured-private-key"}},
		{"fake-provider-credentials", "real credentials", map[string]string{"PHONE_PROVIDER": "fake", "ZVONOK_PUBLIC_KEY": "configured-private-key"}},
		{"invalid-proxy-prefix", "trusted proxy CIDR", map[string]string{"ZVONOK_TRUSTED_PROXY_CIDRS": "not-a-prefix"}},
		{"default-route-proxy", "trusted proxy CIDR", map[string]string{"ZVONOK_TRUSTED_PROXY_CIDRS": "0.0.0.0/0"}},
		{"incomplete-zvonok", "invalid credentials", map[string]string{"PHONE_PROVIDER": "zvonok"}},
		{"missing-stage-jwks", "stage public keys unavailable", map[string]string{"ENV": "stage", "AUTH_MODE": "verify", "AUTH_STAGE_ISSUER": "stage-services", "AUTH_STAGE_SIGNING_KEY_FILE": os.Getenv("JWT_SIGNING_KEY_FILE"), "AUTH_STAGE_PUBLIC_KEYS_FILE": filepath.Join(dir, "missing")}},
		{"incomplete-stage-identity", "stage issuer and key files are required", map[string]string{"ENV": "stage", "AUTH_MODE": "verify", "AUTH_STAGE_ISSUER": "stage-services"}},
		{"missing-user-key-id", "key ID", map[string]string{"JWT_KEY_ID": ""}},
		{"invalid-grpc-listener", "address", map[string]string{"GRPC_ADDR": "invalid:address:format"}},
		{"invalid-ops-listener", "http:", map[string]string{"HTTP_ADDR": "invalid:address:format"}},
		{"invalid-callback-listener", "address", map[string]string{"PHONE_PROVIDER": "zvonok", "ZVONOK_PUBLIC_KEY": "test-private-key", "ZVONOK_CAMPAIGN_ID": "123", "ZVONOK_CALLBACK_ADDR": "invalid:address:format"}},
		{"wrong-signing-algorithm", "Ed25519 key required", map[string]string{"JWT_SIGNING_KEY_FILE": wrongTypeFile}},
		{"oversized-signing-key", "invalid key file", map[string]string{"JWT_SIGNING_KEY_FILE": oversizedFile}},
		{"unsafe-signing-key-permissions", "permissions", map[string]string{"JWT_SIGNING_KEY_FILE": unsafeFile}},
		{"directory-signing-key", "invalid key file", map[string]string{"JWT_SIGNING_KEY_FILE": dir}},
		{"trailing-public-keys", "trailing JWKS", map[string]string{"JWT_PUBLIC_KEYS_FILE": trailingPublicFile}},
		{"empty-public-keys", "public key count", map[string]string{"JWT_PUBLIC_KEYS_FILE": emptyPublicFile}},
		{"retained-key-limit", "too many public keys", map[string]string{"JWT_PUBLIC_KEYS_FILE": public64File}},
		{"missing-outbox-peer", "analytics", map[string]string{"PEER_ANALYTICS_ADDR": ""}},
		{"missing-kubernetes-ca", "Kubernetes CA file unavailable", map[string]string{"ENV": "prod", "AUTH_MODE": "verify", "AUTH_KUBERNETES_ISSUER": "https://kubernetes.test", "SERVICE_TOKEN_FILE": shortAES, "AUTH_KUBERNETES_API_TOKEN_FILE": filepath.Join(dir, "api-token"), "AUTH_KUBERNETES_CA_FILE": filepath.Join(dir, "missing")}},
		{"invalid-kubernetes-ca", "invalid Kubernetes CA", map[string]string{"ENV": "prod", "AUTH_MODE": "verify", "AUTH_KUBERNETES_ISSUER": "https://kubernetes.test", "SERVICE_TOKEN_FILE": shortAES, "AUTH_KUBERNETES_API_TOKEN_FILE": filepath.Join(dir, "api-token"), "AUTH_KUBERNETES_CA_FILE": shortAES}},
		{"insecure-jwks-endpoint", "HTTPS", map[string]string{"ENV": "prod", "AUTH_MODE": "verify", "AUTH_KUBERNETES_ISSUER": "https://kubernetes.test", "SERVICE_TOKEN_FILE": shortAES, "AUTH_KUBERNETES_API_TOKEN_FILE": filepath.Join(dir, "api-token"), "AUTH_KUBERNETES_CA_FILE": caFile, "AUTH_KUBERNETES_JWKS_URL": "http://untrusted.test/jwks"}},
		{"shared-kubernetes-token", "separate service and API token files", map[string]string{"ENV": "prod", "AUTH_MODE": "verify", "SERVICE_TOKEN_FILE": shortAES, "AUTH_KUBERNETES_API_TOKEN_FILE": shortAES}},
		{"missing-auth-key-file", "auth: cannot open key file", map[string]string{"AUTH_IDEMPOTENCY_KEY_FILE": filepath.Join(dir, "missing")}},
		{"empty-auth-key-file", "auth: key file", map[string]string{"AUTH_IDEMPOTENCY_KEY_FILE": ""}},
		{"unsafe-auth-key-permissions", "regular owner-only", map[string]string{"AUTH_IDEMPOTENCY_KEY_FILE": unsafeFile}},
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
