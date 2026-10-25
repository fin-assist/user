package ops

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIntegrationReadinessNetworkFailureRecoveryAndShutdown(t *testing.T) {
	var unavailable atomic.Bool
	dependency := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if unavailable.Load() {
			http.Error(w, "dependency unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer dependency.Close()
	client := &http.Client{Timeout: time.Second}
	readiness := &Readiness{}
	changes := make(chan bool, 8)
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		defer close(done)
		readiness.Watch(ctx, 100*time.Millisecond, slog.New(slog.DiscardHandler), func(ctx context.Context) error {
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, dependency.URL, nil)
			if err != nil {
				return err
			}
			response, err := client.Do(request)
			if err != nil {
				return err
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != http.StatusNoContent {
				return errors.New("dependency unavailable")
			}
			return nil
		}, func(ready bool) { changes <- ready })
	}()
	await := func(want bool) {
		t.Helper()
		select {
		case got := <-changes:
			if got != want {
				t.Fatalf("readiness=%v, want %v", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("readiness transition timed out")
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := Server(listener.Addr().String(), Handler(NewRegistry(), readiness))
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	base := "http://" + listener.Addr().String()
	get := func(path string, status int, body string) {
		t.Helper()
		response, err := client.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != status || !strings.Contains(string(data), body) {
			t.Fatalf("%s status=%d missing body=%q", path, response.StatusCode, body)
		}
	}
	await(true)
	get("/readyz", 200, "ok")
	get("/healthz", 200, "ok")
	get("/metrics", 200, "go_goroutines")
	get("/missing", 404, "404")
	unavailable.Store(true)
	await(false)
	get("/readyz", 503, "not ready")
	get("/healthz", 200, "ok")
	unavailable.Store(false)
	await(true)
	get("/readyz", 200, "ok")
	// Shutdown drain over the real listener: not ready while the dependency is healthy.
	readiness.Drain()
	get("/readyz", 503, "not ready")
	get("/healthz", 200, "ok")
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("readiness watcher did not stop")
	}
	shutdownCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("HTTP server did not stop")
	}
	if response, err := client.Get(base + "/healthz"); err == nil {
		_ = response.Body.Close()
		t.Fatal("health endpoint survived shutdown")
	}
}
