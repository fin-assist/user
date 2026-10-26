package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/fin-assist/user/internal/testdb"
)

// During SHUTDOWN_DELAY the process is still listening but must already look not ready
// on both probes: gRPC health NOT_SERVING and HTTP /readyz 503, while /healthz stays up.
func TestAPIShutdownDelayReportsNotReadyOnBothProbes(t *testing.T) {
	setSigningKey(t)
	setPhoneKey(t)
	dsn := testdb.DSN(t)
	dsnFile := filepath.Join(t.TempDir(), "primary-dsn")
	if err := os.WriteFile(dsnFile, []byte(dsn), 0o600); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"SERVICE_NAME": "user", "ENV": "dev", "AUTH_MODE": "insecure-dev", "LOG_LEVEL": "error",
		"DB_PRIMARY_DSN": "", "DB_PRIMARY_DSN_FILE": dsnFile, "GRPC_ADDR": "127.0.0.1:0", "HTTP_ADDR": "127.0.0.1:0",
		"SHUTDOWN_DELAY": "3s", "SHUTDOWN_TIMEOUT": "2s",
		"PEER_BANK_STATEMENT_DATA_ADDR": "passthrough:///127.0.0.1:1",
		"PEER_ANALYTICS_ADDR":           "passthrough:///127.0.0.1:1", "PEER_ASSISTANT_ADDR": "passthrough:///127.0.0.1:1",
	} {
		t.Setenv(k, v)
	}
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	if err := run(t.Context(), []string{"migrate", "up"}); err != nil {
		t.Fatal(err)
	}
	serveCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	bound, done := startServe(t, serveCtx)
	grpcAddr, httpAddr := bound.GRPC.String(), bound.HTTP.String()

	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	health := healthv1.NewHealthClient(conn)
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	ctx, stop := context.WithTimeout(t.Context(), 30*time.Second)
	defer stop()
	get := func(path string) int {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+httpAddr+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	await := func(what string, cond func() bool) {
		t.Helper()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for !cond() {
			select {
			case <-ctx.Done():
				t.Fatalf("%s: timed out", what)
			case err := <-done:
				t.Fatalf("%s: serve stopped: %v", what, err)
			case <-tick.C:
			}
		}
	}
	serving := func(want healthv1.HealthCheckResponse_ServingStatus) func() bool {
		return func() bool {
			r, err := health.Check(ctx, &healthv1.HealthCheckRequest{})
			return err == nil && r.Status == want
		}
	}
	await("ready before shutdown", func() bool { return get("/readyz") == http.StatusOK && serving(healthv1.HealthCheckResponse_SERVING)() })

	cancel()
	started := time.Now()
	await("gRPC health NOT_SERVING during the delay", serving(healthv1.HealthCheckResponse_NOT_SERVING))
	if code := get("/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz during SHUTDOWN_DELAY: %d, want 503", code)
	}
	if code := get("/healthz"); code != http.StatusOK {
		t.Fatalf("/healthz during SHUTDOWN_DELAY: %d, want 200", code)
	}
	if time.Since(started) >= 3*time.Second {
		t.Fatal("probes were checked after SHUTDOWN_DELAY, not during it")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("serve did not stop")
	}
}
