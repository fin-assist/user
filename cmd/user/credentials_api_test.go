package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	goidentity "github.com/fin-assist/go-identity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/fin-assist/user/internal/testdb"
)

// stageCredential writes one file of the stand as systemd hands it to the service.
func stageCredential(t *testing.T, path string, content []byte, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

// stagePEM returns a PKCS#8 Ed25519 key and its public JWK with the given kid.
func stagePEM(t *testing.T, kid string) ([]byte, map[string]string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), map[string]string{"kty": "OKP", "crv": "Ed25519", "kid": kid, "x": base64.RawURLEncoding.EncodeToString(public)}
}

// FIN-349: the executable started as the stand unit starts it (ENV=stage, AUTH_MODE=verify,
// every private file a credential in $CREDENTIALS_DIRECTORY) serves with credentials 0440
// (systemd ACL mask on tmpfs) and answers a call signed by bank-bff with its stand key;
// 0440 outside the directory and a credential readable by others refuse to start.
func TestAPIServeStartsWithSystemdCredentials(t *testing.T) {
	dsn := testdb.DSN(t)
	for name, value := range map[string]string{
		"SERVICE_NAME": "user", "ENV": "dev", "AUTH_MODE": "insecure-dev", "DB_PRIMARY_DSN": dsn, "DB_PRIMARY_DSN_FILE": "",
	} {
		t.Setenv(name, value)
	}
	if err := run(t.Context(), []string{"migrate", "up"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		inside bool
		perm   os.FileMode
		want   string // "" serves
	}{
		{"credentials 0440", true, 0o440, ""},
		{"credentials 0444", true, 0o444, "invalid key file or permissions"},
		{"0440 outside the credentials directory", false, 0o440, "invalid key file or permissions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credentials, outside, public := t.TempDir(), t.TempDir(), t.TempDir()
			dir := outside
			if tc.inside {
				dir = credentials
			}
			userStage, userJWK := stagePEM(t, "user")
			bffStage, bffJWK := stagePEM(t, "bank-bff")
			jwtSigning, _ := stagePEM(t, "access")
			jwks, err := json.Marshal(map[string]any{"keys": []any{userJWK, bffJWK}})
			if err != nil {
				t.Fatal(err)
			}
			jwksPath := filepath.Join(public, "stage-public.jwks")
			stageCredential(t, jwksPath, jwks, 0o644)
			bffKey := filepath.Join(public, "bank-bff.pem")
			stageCredential(t, bffKey, bffStage, 0o600)
			// The DSN credential is always valid: the case is about the key files.
			stageCredential(t, filepath.Join(credentials, "db-primary-dsn"), []byte(dsn+"\n"), 0o440)
			for name, content := range map[string][]byte{"jwt-signing": jwtSigning, "auth-idempotency": bytes.Repeat([]byte{1}, 32), "phone-result": bytes.Repeat([]byte{2}, 32), "stage-signing": userStage} {
				stageCredential(t, filepath.Join(dir, name), content, tc.perm)
			}
			for name, value := range map[string]string{
				"CREDENTIALS_DIRECTORY": credentials, "ENV": "stage", "AUTH_MODE": "verify", "SERVICE_NAME": "user", "LOG_LEVEL": "error",
				"DB_PRIMARY_DSN": "", "DB_PRIMARY_DSN_FILE": filepath.Join(credentials, "db-primary-dsn"),
				"JWT_SIGNING_KEY_FILE": filepath.Join(dir, "jwt-signing"), "JWT_KEY_ID": "stage-1", "JWT_ISSUER": "https://user.example", "JWT_AUDIENCE": "finassist",
				"AUTH_IDEMPOTENCY_KEY_FILE": filepath.Join(dir, "auth-idempotency"), "PHONE_RESULT_KEY_FILE": filepath.Join(dir, "phone-result"),
				"AUTH_STAGE_ISSUER": "stage-services", "AUTH_STAGE_SIGNING_KEY_FILE": filepath.Join(dir, "stage-signing"), "AUTH_STAGE_PUBLIC_KEYS_FILE": jwksPath,
				"GRPC_ADDR": "127.0.0.1:0", "HTTP_ADDR": "127.0.0.1:0", "PHONE_PROVIDER": "disabled", "SHUTDOWN_DELAY": "0s", "SHUTDOWN_TIMEOUT": "3s",
				"PEER_BANK_STATEMENT_DATA_ADDR": "passthrough:///127.0.0.1:1", "PEER_ANALYTICS_ADDR": "passthrough:///127.0.0.1:1", "PEER_ASSISTANT_ADDR": "passthrough:///127.0.0.1:1",
			} {
				t.Setenv(name, value)
			}
			if tc.want != "" {
				startupCtx, stopStartup := context.WithTimeout(t.Context(), 5*time.Second)
				defer stopStartup()
				recorder := &listenRecorder{}
				served := false
				err := runWith(startupCtx, []string{"serve"}, serveHooks{listening: func(boundAddrs) { served = true }, listen: recorder.listen})
				if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), dir) {
					t.Fatalf("unsafe credential mode accepted or unclear: %v", err)
				}
				if served {
					t.Fatal("rejected deployment started serving")
				}
				recorder.requireAllClosed(t, 0)
				return
			}
			ctx, cancel := context.WithCancel(t.Context())
			bound, done := startServe(t, ctx)
			t.Cleanup(func() {
				cancel()
				select {
				case err := <-done:
					if err != nil {
						t.Error(err)
					}
				case <-time.After(10 * time.Second):
					t.Error("serve did not stop")
				}
			})
			conn, err := grpc.NewClient(bound.GRPC.String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			bff, err := goidentity.NewStageSource("stage", goidentity.StageOptions{KeyFile: bffKey, Caller: "bank-bff", Issuer: "stage-services", Clock: time.Now})
			if err != nil {
				t.Fatal(err)
			}
			token, err := bff.Token(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			callCtx, stop := context.WithTimeout(t.Context(), 10*time.Second)
			defer stop()
			callCtx = metadata.AppendToOutgoingContext(callCtx, "x-service-authorization", "Bearer "+token)
			keys, err := userv1.NewUserInternalServiceClient(conn).GetSigningKeys(callCtx, &userv1.GetSigningKeysRequest{}, grpc.WaitForReady(true))
			if err != nil || !strings.Contains(keys.GetJwksJson(), `"stage-1"`) {
				t.Fatalf("signed call to the stand assembly: %v %v", keys, err)
			}
		})
	}
}
