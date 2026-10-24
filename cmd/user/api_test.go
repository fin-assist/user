package main

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
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
		"SERVICE_NAME": "user", "ENV": "dev", "AUTH_MODE": "insecure-dev", "LOG_LEVEL": "error",
		"DB_PRIMARY_DSN": dsn, "GRPC_ADDR": address, "HTTP_ADDR": "127.0.0.1:0",
		"SHUTDOWN_DELAY": "0s", "SHUTDOWN_TIMEOUT": "5s", "PHONE_PROVIDER": "disabled",
		"PEER_BANK_STATEMENT_DATA_ADDR": "passthrough:///127.0.0.1:1",
		"PEER_ANALYTICS_ADDR":           "passthrough:///127.0.0.1:1", "PEER_ASSISTANT_ADDR": "passthrough:///127.0.0.1:1",
	} {
		t.Setenv(name, value)
	}
	if err := run(t.Context(), []string{"migrate", "up"}); err != nil {
		t.Fatal(err)
	}
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
}
