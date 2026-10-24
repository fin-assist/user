package main

import (
	"context"
	"io"
	"net"
	"net/http"
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

func TestAPIDeploymentRebuildsBoundedCallbackMetricsFromStoredOutcomes(t *testing.T) {
	setSigningKey(t)
	setPhoneKey(t)
	dsn := migratetest.DSN(t)
	reserve := func() string {
		t.Helper()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().String()
		_ = listener.Close()
		return address
	}
	grpcAddress, httpAddress := reserve(), reserve()
	for name, value := range map[string]string{
		"SERVICE_NAME": "user", "ENV": "dev", "AUTH_MODE": "insecure-dev", "LOG_LEVEL": "error",
		"DB_PRIMARY_DSN": dsn, "GRPC_ADDR": grpcAddress, "HTTP_ADDR": httpAddress, "PHONE_PROVIDER": "fake",
		"SHUTDOWN_DELAY": "0s", "SHUTDOWN_TIMEOUT": "3s",
		"PEER_BANK_STATEMENT_DATA_ADDR": "passthrough:///127.0.0.1:1", "PEER_ANALYTICS_ADDR": "passthrough:///127.0.0.1:1", "PEER_ASSISTANT_ADDR": "passthrough:///127.0.0.1:1",
	} {
		t.Setenv(name, value)
	}
	if err := run(t.Context(), []string{"migrate", "up"}); err != nil {
		t.Fatal(err)
	}
	db, err := pgx.Connect(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close(context.WithoutCancel(t.Context())) }()
	// Persisted requests from the preceding deployment, including an old row.
	// Every starts the real report immediately; no job or Report method is called here.
	_, err = db.Exec(t.Context(), `INSERT INTO phone_verification
		(phone,token_hash,provider,provider_call_id,created_at,expires_at,verified_at,callback_number)
		SELECT '+7000000'||lpad(n::text,4,'0'), convert_to('stored-'||n,'UTF8'), 'zvonok','stored-'||n,
		CURRENT_TIMESTAMP - CASE WHEN n=258 THEN interval '25 hours' ELSE interval '1 hour' END,
		CURRENT_TIMESTAMP + CASE WHEN n%3=0 THEN -interval '1 minute' ELSE interval '30 minutes' END,
		CASE WHEN n%3=1 THEN CURRENT_TIMESTAMP-interval '5 minutes' ELSE NULL END,
		'+7495'||lpad(n::text,7,'0') FROM generate_series(1,258) n`)
	if err != nil {
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
		case <-time.After(8 * time.Second):
			t.Error("deployment did not stop its workers")
		}
	})
	conn, err := grpc.NewClient(grpcAddress, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	requestCtx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	requestCtx = metadata.AppendToOutgoingContext(requestCtx, "x-service-authorization", "Bearer dev:assistant")
	if response, err := userv1.NewUserInternalServiceClient(conn).GetSigningKeys(requestCtx, &userv1.GetSigningKeysRequest{}, grpc.WaitForReady(true)); err != nil || response.GetJwksJson() == "" {
		t.Fatalf("public API did not start alongside the reporting worker: %v %v", response, err)
	}
	client := &http.Client{Timeout: time.Second}
	var body string
	for {
		request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "http://"+httpAddress+"/metrics", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err == nil {
			data, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if readErr != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("metrics response: status=%d err=%v", response.StatusCode, readErr)
			}
			body = string(data)
			if strings.Contains(body, "finassist_phone_callback_overflow_requests_24h 1\n") {
				break
			}
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-requestCtx.Done():
			timer.Stop()
			t.Fatalf("scheduled worker did not publish persisted outcomes: %v", requestCtx.Err())
		case <-timer.C:
		}
	}
	for _, line := range []string{
		`finassist_phone_callback_outcomes_24h{callback_number="+74950000001",outcome="confirmed"} 1`,
		`finassist_phone_callback_outcomes_24h{callback_number="+74950000002",outcome="pending"} 1`,
		`finassist_phone_callback_outcomes_24h{callback_number="+74950000003",outcome="expired"} 1`,
		`finassist_phone_callback_outcomes_24h{callback_number="other",outcome="pending"} 1`,
	} {
		if !strings.Contains(body, line) {
			t.Fatalf("rolling outcome missing from actual HTTP metrics: %s", line)
		}
	}
	if strings.Count(body, `finassist_phone_callback_outcomes_24h{callback_number="+`) != 256 || strings.Contains(body, "+74950000257") || strings.Contains(body, "+74950000258") || strings.Contains(body, "+7000000") || strings.Contains(body, "stored-") {
		t.Fatal("metrics exceeded label bounds, included expired history or leaked caller identifiers")
	}
}
