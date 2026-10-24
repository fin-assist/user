package main

import (
	"context"
	"net"
	"net/http"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/fin-assist/user/internal/adapter/postgres/migrate/migratetest"
)

func TestAPIDeploymentExposesGuardedCallbackListenerAndStopsIt(t *testing.T) {
	setSigningKey(t)
	setPhoneKey(t)
	address := func() string {
		t.Helper()
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		a := l.Addr().String()
		_ = l.Close()
		return a
	}
	grpcAddr, callbackAddr := address(), address()
	for key, value := range map[string]string{
		"SERVICE_NAME": "user", "ENV": "dev", "AUTH_MODE": "insecure-dev", "LOG_LEVEL": "error",
		"DB_PRIMARY_DSN": migratetest.DSN(t), "DB_PRIMARY_DSN_FILE": "", "GRPC_ADDR": grpcAddr, "HTTP_ADDR": "127.0.0.1:0",
		"SHUTDOWN_DELAY": "0s", "SHUTDOWN_TIMEOUT": "3s", "PHONE_PROVIDER": "zvonok", "ZVONOK_PUBLIC_KEY": "test-private-key", "ZVONOK_CAMPAIGN_ID": "123", "ZVONOK_CALLBACK_ADDR": callbackAddr, "ZVONOK_TRUSTED_PROXY_CIDRS": "",
		"PEER_BANK_STATEMENT_DATA_ADDR": "passthrough:///127.0.0.1:1", "PEER_ANALYTICS_ADDR": "passthrough:///127.0.0.1:1", "PEER_ASSISTANT_ADDR": "passthrough:///127.0.0.1:1",
	} {
		t.Setenv(key, value)
	}
	if err := run(t.Context(), []string{"migrate", "up"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"serve"}); close(done) }()
	stop := func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(8 * time.Second):
			t.Error("callback deployment did not stop")
		}
	}
	t.Cleanup(stop)
	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	callCtx, stopCall := context.WithTimeout(t.Context(), 10*time.Second)
	defer stopCall()
	callCtx = metadata.AppendToOutgoingContext(callCtx, "x-service-authorization", "Bearer dev:assistant")
	keys, err := userv1.NewUserInternalServiceClient(conn).GetSigningKeys(callCtx, &userv1.GetSigningKeysRequest{}, grpc.WaitForReady(true))
	if err != nil || keys.GetJwksJson() == "" {
		t.Fatal(keys, err)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/callbacks/zvonok?ct_call_id=41", http.StatusForbidden},
		{http.MethodPost, "/callbacks/zvonok?ct_call_id=41", http.StatusNotFound},
		{http.MethodGet, "/unknown", http.StatusNotFound},
	} {
		req, err := http.NewRequestWithContext(t.Context(), tc.method, "http://"+callbackAddr+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		// An untrusted caller cannot turn itself into the provider by spoofing XFF.
		req.Header.Set("X-Forwarded-For", "193.232.64.150")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != tc.want || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatal(tc, response.StatusCode)
		}
	}
	stop()
	if response, err := client.Get("http://" + callbackAddr + "/callbacks/zvonok?ct_call_id=41"); err == nil {
		_ = response.Body.Close()
		t.Fatal("callback ingress remained available after deployment stopped")
	}
}
