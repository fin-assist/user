package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/fin-assist/user/internal/testdb"
)

func TestAPIDeploymentExposesGuardedCallbackListenerAndStopsIt(t *testing.T) {
	setSigningKey(t)
	setPhoneKey(t)
	for key, value := range map[string]string{
		"SERVICE_NAME": "user", "ENV": "dev", "AUTH_MODE": "insecure-dev", "LOG_LEVEL": "info", "LOG_FORMAT": "json",
		"DB_PRIMARY_DSN": testdb.DSN(t), "DB_PRIMARY_DSN_FILE": "", "GRPC_ADDR": "127.0.0.1:0", "HTTP_ADDR": "127.0.0.1:0",
		"SHUTDOWN_DELAY": "0s", "SHUTDOWN_TIMEOUT": "3s", "PHONE_PROVIDER": "zvonok", "ZVONOK_PUBLIC_KEY": "test-private-key", "ZVONOK_CAMPAIGN_ID": "123", "ZVONOK_CALLBACK_ADDR": "127.0.0.1:0", "ZVONOK_TRUSTED_PROXY_CIDRS": "",
		"PEER_BANK_STATEMENT_DATA_ADDR": "passthrough:///127.0.0.1:1", "PEER_ANALYTICS_ADDR": "passthrough:///127.0.0.1:1", "PEER_ASSISTANT_ADDR": "passthrough:///127.0.0.1:1",
	} {
		t.Setenv(key, value)
	}
	if err := run(t.Context(), []string{"migrate", "up"}); err != nil {
		t.Fatal(err)
	}
	logs, err := os.CreateTemp(t.TempDir(), "serve-callback-*.log")
	if err != nil {
		t.Fatal(err)
	}
	previousStderr := os.Stderr
	os.Stderr = logs
	t.Cleanup(func() { os.Stderr = previousStderr; _ = logs.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	bound, done := startServe(t, ctx)
	grpcAddr, callbackAddr := bound.GRPC.String(), bound.Callback.String()
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
	assertServingLog(t, logs.Name(), callbackAddr, "test-private-key")
	if response, err := client.Get("http://" + callbackAddr + "/callbacks/zvonok?ct_call_id=41"); err == nil {
		_ = response.Body.Close()
		t.Fatal("callback ingress remained available after deployment stopped")
	}
}

// assertServingLog checks that the serving record names the bound postback address, or
// an empty one without Zvonok (FIN-349), and that no record of the run carries secret.
func assertServingLog(t *testing.T, path, callbackAddr, secret string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	found := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if strings.Contains(string(line), secret) {
			t.Fatal("log leaked the provider key")
		}
		var record map[string]any
		if json.Unmarshal(line, &record) != nil || record["msg"] != "serving" {
			continue
		}
		if got, ok := record["callback"].(string); !ok || got != callbackAddr {
			t.Fatalf("serving record callback=%v, want %s", record["callback"], callbackAddr)
		}
		found = true
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("no serving record")
	}
}
