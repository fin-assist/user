package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/fin-assist/user/internal/adapter/postgres/migrate/migratetest"
)

// This API test exercises the executable's actual environment loader and serve assembly.
func TestAPIServeFromEnvironmentPublishesSigningKeysAndStops(t *testing.T) {
	setSigningKey(t)
	setPhoneKey(t)
	dsn := migratetest.DSN(t)
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	_ = reserved.Close()
	for name, value := range map[string]string{
		"SERVICE_NAME": "user", "ENV": "dev", "AUTH_MODE": "insecure-dev", "LOG_LEVEL": "debug",
		"DB_PRIMARY_DSN": dsn, "GRPC_ADDR": address, "HTTP_ADDR": "127.0.0.1:0",
		"SHUTDOWN_DELAY": "0s", "SHUTDOWN_TIMEOUT": "5s", "PHONE_PROVIDER": "fake",
		"PEER_BANK_STATEMENT_DATA_ADDR": "passthrough:///127.0.0.1:1",
		"PEER_ANALYTICS_ADDR":           "passthrough:///127.0.0.1:1", "PEER_ASSISTANT_ADDR": "passthrough:///127.0.0.1:1",
	} {
		t.Setenv(name, value)
	}
	// File-injected database credentials are consumed by both deployment commands.
	dsnFile := t.TempDir() + "/database-dsn"
	if err := os.WriteFile(dsnFile, []byte(dsn+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DB_PRIMARY_DSN", "")
	t.Setenv("DB_PRIMARY_DSN_FILE", dsnFile)
	if err := run(t.Context(), []string{"migrate", "up"}); err != nil {
		t.Fatal(err)
	}
	// Invalid operator commands and dirty migration state must stop the rollout
	// before its final serve step. Repairing the checked version must restore the
	// actual public API below, without modifying another source's schema.
	for _, args := range [][]string{nil, {"unknown"}, {"serve", "extra"}, {"migrate"}, {"migrate", "unknown"}, {"migrate", "up", "extra"}, {"migrate", "version", "extra"}, {"migrate", "down"}, {"migrate", "force"}, {"migrate", "down", "unknown", "1"}, {"migrate", "force", "service", "bad"}, {"migrate", "down", "service", "0"}, {"migrate", "force", "service", "-2"}} {
		if err := run(t.Context(), args); err == nil {
			t.Fatalf("invalid rollout command accepted: %v", args)
		} else if err.Error() == "" {
			t.Fatal("operator received no error diagnostic")
		}
	}
	for _, tc := range []struct {
		name string
		env  map[string]string
		args []string
	}{
		{"migration-invalid-timeout", map[string]string{"MIGRATE_LOCK_TIMEOUT": "0s"}, []string{"migrate", "up"}},
		{"migration-missing-dsn", map[string]string{"DB_PRIMARY_DSN": "", "DB_PRIMARY_DSN_FILE": ""}, []string{"migrate", "up"}},
		{"migration-invalid-dsn", map[string]string{"DB_PRIMARY_DSN": "://bad", "DB_PRIMARY_DSN_FILE": ""}, []string{"migrate", "up"}},
		{"migration-unavailable-database", map[string]string{"DB_PRIMARY_DSN": "postgres://postgres@127.0.0.1:1/missing?sslmode=disable&connect_timeout=1", "DB_PRIMARY_DSN_FILE": ""}, []string{"migrate", "version"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range tc.env {
				t.Setenv(key, value)
			}
			if err := run(t.Context(), tc.args); err == nil {
				t.Fatal("invalid rollout configuration accepted")
			}
		})
	}
	operatorDB, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := operatorDB.Exec(t.Context(), `UPDATE schema_migrations SET dirty=true`); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"migrate", "up"}); err == nil {
		t.Fatal("dirty schema accepted for rollout")
	}
	capture, err := os.CreateTemp(t.TempDir(), "migration-versions-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	stdoutBefore := os.Stdout
	os.Stdout = capture
	versionErr := run(t.Context(), []string{"migrate", "version"})
	os.Stdout = stdoutBefore
	if err := capture.Close(); err != nil {
		t.Fatal(err)
	}
	if versionErr != nil {
		t.Fatal(versionErr)
	}
	versionBytes, err := os.ReadFile(capture.Name())
	if err != nil || !bytes.Contains(versionBytes, []byte("service\t4 (dirty)")) {
		t.Fatal(string(versionBytes), err)
	}
	if err := run(t.Context(), []string{"migrate", "force", "service", "4"}); err != nil {
		t.Fatal(err)
	}
	if err := operatorDB.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A deploy may roll back only the service source and then reapply it before
	// serving. The executable's commands must preserve library migration versions.
	var versions bytes.Buffer
	if err := runMigrate(t.Context(), []string{"version"}, &versions); err != nil || !strings.Contains(versions.String(), "service\t4") {
		t.Fatal(versions.String(), err)
	}
	for _, args := range [][]string{{"down", "service", "1"}, {"force", "service", "3"}, {"up"}} {
		if err := runMigrate(t.Context(), args, &versions); err != nil {
			t.Fatal(args, err)
		}
	}
	db, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(context.Background()) }()
	if _, err = db.Exec(t.Context(), `INSERT INTO consent_document(type,version,title,url,is_current) VALUES ('personal_data','v1','Test consent','https://example.test/consent',true),('assistant','v2','Assistant consent','https://example.test/assistant',true)`); err != nil {
		t.Fatal(err)
	}
	logs, err := os.CreateTemp(t.TempDir(), "serve-debug-*.log")
	if err != nil {
		t.Fatal(err)
	}
	previousStderr := os.Stderr
	os.Stderr = logs
	t.Cleanup(func() { os.Stderr = previousStderr; _ = logs.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"serve"}) }()
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
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	requestCtx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	requestCtx = metadata.NewOutgoingContext(requestCtx, metadata.Pairs("x-service-authorization", "Bearer dev:assistant"))
	response, err := userv1.NewUserInternalServiceClient(conn).GetSigningKeys(requestCtx, &userv1.GetSigningKeysRequest{}, grpc.WaitForReady(true))
	var jwks struct {
		Keys []struct {
			Kid string `json:"kid"`
		} `json:"keys"`
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal([]byte(response.GetJwksJson()), &jwks); err != nil || len(jwks.Keys) != 1 || jwks.Keys[0].Kid != "serve-test" {
		t.Fatalf("signing keys over public API: %v %v", response, err)
	}
	// Exercise sensitive fields through the executable's real transport and verify
	// their requests are masked in the actual debug logger.
	client := userv1.NewAuthServiceClient(conn)
	bank := metadata.NewOutgoingContext(requestCtx, metadata.Pairs("x-service-authorization", "Bearer dev:bank-bff", "x-client-ip", "203.0.113.1", "idempotency-key", "019c7714-3b77-74d1-9866-e1f484aae2ab", "x-private-token", "metadata-secret", "trace-bin", "binary-secret"))
	started, err := client.StartPhoneVerification(bank, &userv1.StartPhoneVerificationRequest{Phone: "+70000001111"})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.WatchPhoneVerification(bank, &userv1.WatchPhoneVerificationRequest{VerificationToken: started.GetVerificationToken()})
	if err != nil {
		t.Fatal(err)
	}
	verified, err := stream.Recv()
	if err != nil || verified.GetRegistrationToken() == "" {
		t.Fatal(verified, err)
	}
	registerCtx := metadata.AppendToOutgoingContext(metadata.NewOutgoingContext(requestCtx, metadata.Pairs("x-service-authorization", "Bearer dev:bank-bff", "x-client-ip", "203.0.113.1")), "idempotency-key", "019c7714-3b77-74d1-9866-e1f484aae2ac")
	registered, err := client.Register(registerCtx, &userv1.RegisterRequest{RegistrationToken: verified.GetRegistrationToken(), PdConsentAccepted: true, PdConsentVersion: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	refreshCtx := metadata.NewOutgoingContext(requestCtx, metadata.Pairs("x-service-authorization", "Bearer dev:bank-bff", "x-client-ip", "203.0.113.1", "idempotency-key", "019c7714-3b77-74d1-9866-e1f484aae2ad"))
	refreshed, err := client.RefreshTokens(refreshCtx, &userv1.RefreshTokensRequest{RefreshToken: registered.GetTokens().GetRefreshToken()})
	if err != nil || refreshed.GetTokens().GetRefreshToken() == "" {
		t.Fatal(refreshed, err)
	}
	userCtx := metadata.NewOutgoingContext(requestCtx, metadata.Pairs("x-service-authorization", "Bearer dev:bank-bff", "authorization", "Bearer dev:"+registered.GetUserId()))
	got, err := userv1.NewProfileServiceClient(conn).GetProfile(userCtx, &userv1.GetProfileRequest{})
	if err != nil || got.GetProfile().GetUserId() != registered.GetUserId() {
		t.Fatal(got, err)
	}
	if err := logs.Sync(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(logs.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("grpc")) || !bytes.Contains(data, []byte("***")) {
		t.Fatalf("no masked RPC diagnostic: %s", data)
	}
	for _, secret := range []string{started.GetVerificationToken(), verified.GetRegistrationToken(), registered.GetTokens().GetRefreshToken(), registered.GetTokens().GetAccessToken(), "metadata-secret", "binary-secret"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatal("debug log leaked secret")
		}
	}
}
