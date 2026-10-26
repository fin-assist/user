package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fin-assist/go-migrate"

	"github.com/fin-assist/user/internal/app"
	"github.com/fin-assist/user/internal/testdb"
)

// startServe runs the serve command in the background and waits until every listener is
// bound, returning the actual addresses. Listeners use port 0, so no port is reserved,
// released and bound again by another process in between. The returned channel delivers
// the serve result once and is then closed.
func startServe(t *testing.T, ctx context.Context) (boundAddrs, <-chan error) {
	t.Helper()
	listening := make(chan boundAddrs, 1)
	done := make(chan error, 1)
	go func() {
		done <- runWith(ctx, []string{"serve"}, serveHooks{listening: func(addrs boundAddrs) { listening <- addrs }})
		close(done)
	}()
	select {
	case addrs := <-listening:
		return addrs, done
	case err := <-done:
		t.Fatalf("serve stopped before listening: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("serve did not bind its listeners")
	}
	return boundAddrs{}, done
}

// listenRecorder binds real listeners for serve and remembers each one with whether it
// was closed, so a test can assert that a failed startup released everything it bound.
type listenRecorder struct {
	mu     sync.Mutex
	opened []*recordedListener
}

type recordedListener struct {
	net.Listener
	closed atomic.Bool
}

func (l *recordedListener) Close() error {
	l.closed.Store(true)
	return l.Listener.Close()
}

func (r *listenRecorder) listen(ctx context.Context, network, address string) (net.Listener, error) {
	lis, err := (&net.ListenConfig{}).Listen(ctx, network, address)
	if err != nil {
		return nil, err
	}
	recorded := &recordedListener{Listener: lis}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opened = append(r.opened, recorded)
	return recorded, nil
}

// requireAllClosed fails unless exactly want listeners were bound and every one is closed.
func (r *listenRecorder) requireAllClosed(t *testing.T, want int) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.opened) != want {
		t.Fatalf("failed startup bound %d listeners, want %d", len(r.opened), want)
	}
	for _, l := range r.opened {
		if !l.closed.Load() {
			t.Fatalf("failed startup left %s bound", l.Addr())
		}
	}
}

func TestUnitServeFailsBeforeConnectingWithoutSigningKey(t *testing.T) {
	t.Setenv("SERVICE_NAME", "user")
	t.Setenv("DB_PRIMARY_DSN", "postgres://nowhere.invalid/db")
	t.Setenv("ENV", "prod")
	err := runServe(t.Context(), serveHooks{})
	if err == nil || !strings.Contains(err.Error(), "signing:") {
		t.Fatalf("error %v", err)
	}
}

// The whole serve path against a real database: config, pools, server, ops listener,
// readiness and an orderly stop on cancel.
func TestIntegrationServeRunsAndStops(t *testing.T) {
	for _, provider := range []string{"disabled", "zvonok"} {
		t.Run(provider, func(t *testing.T) { serveAndStop(t, provider) })
	}
}
func serveAndStop(t *testing.T, provider string) {
	t.Helper()
	t.Setenv("PEER_BANK_STATEMENT_DATA_ADDR", "passthrough:///127.0.0.1:1")
	t.Setenv("PEER_ANALYTICS_ADDR", "passthrough:///127.0.0.1:1")
	t.Setenv("PEER_ASSISTANT_ADDR", "passthrough:///127.0.0.1:1")
	setSigningKey(t)
	setPhoneKey(t)
	dsn := testdb.DSN(t)
	if err := migrate.Up(t.Context(), migrate.DefaultConfig(dsn), app.MigrationSources()); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"SERVICE_NAME": "user", "ENV": "dev", "AUTH_MODE": "insecure-dev", "LOG_LEVEL": "error", "DB_PRIMARY_DSN": dsn, "GRPC_ADDR": "127.0.0.1:0", "HTTP_ADDR": "127.0.0.1:0", "SHUTDOWN_DELAY": "0s", "SHUTDOWN_TIMEOUT": "5s", "PHONE_PROVIDER": provider} {
		t.Setenv(k, v)
	}
	if provider == "zvonok" {
		t.Setenv("ZVONOK_CALLBACK_ADDR", "127.0.0.1:0")
		t.Setenv("ZVONOK_PUBLIC_KEY", "fake-test-key")
		t.Setenv("ZVONOK_CAMPAIGN_ID", "123")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	bound, errc := startServe(t, ctx)
	if (bound.Callback != nil) != (provider == "zvonok") {
		t.Fatalf("callback listener bound %v for provider %s", bound.Callback, provider)
	}
	httpAddr := bound.HTTP.String()
	// Stop only once the service is actually up: wait for a condition with a deadline,
	// never a fixed delay that a loaded machine may outlast before the first ping.
	waitCtx, stopWait := context.WithTimeout(t.Context(), 30*time.Second)
	defer stopWait()
	status := func(url string) int {
		req, err := http.NewRequestWithContext(waitCtx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0
		}
		_ = r.Body.Close()
		return r.StatusCode
	}
	await := func(what string, ready func() bool) {
		t.Helper()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for !ready() {
			select {
			case err := <-errc:
				t.Fatal("serve stopped", err)
			case <-waitCtx.Done():
				t.Fatal(what + ": timed out")
			case <-tick.C:
			}
		}
	}
	await("readiness", func() bool { return status("http://"+httpAddr+"/readyz") == http.StatusOK })
	if provider == "zvonok" {
		await("callback listener", func() bool {
			code := status("http://" + bound.Callback.String() + "/callbacks/zvonok?ct_call_id=123")
			if code != 0 && code != http.StatusForbidden {
				t.Fatal(code)
			}
			return code == http.StatusForbidden
		})
	}
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal("serve", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop")
	}
}

func TestIntegrationServeFailsOnBadInput(t *testing.T) {
	setSigningKey(t)
	setPhoneKey(t)
	base := map[string]string{"SERVICE_NAME": "user", "ENV": "dev", "AUTH_MODE": "insecure-dev", "LOG_LEVEL": "error",
		"PEER_BANK_STATEMENT_DATA_ADDR": "passthrough:///127.0.0.1:1", "PEER_ANALYTICS_ADDR": "passthrough:///127.0.0.1:1", "PEER_ASSISTANT_ADDR": "passthrough:///127.0.0.1:1"}
	// The test keeps this port bound for its whole duration, so the conflict is certain.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = occupied.Close() }()
	cases := map[string]map[string]string{
		"config":   {"DB_PRIMARY_DSN": ""},
		"database": {"DB_PRIMARY_DSN": "postgres://nobody@127.0.0.1:1/db?connect_timeout=1"},
		"listen":   {"DB_PRIMARY_DSN": testdb.DSN(t), "GRPC_ADDR": "256.0.0.1:1"},
		"outbox":   {"DB_PRIMARY_DSN": testdb.DSN(t), "PEER_ASSISTANT_ADDR": ""},
		"http":     {"DB_PRIMARY_DSN": testdb.DSN(t), "GRPC_ADDR": "127.0.0.1:0", "HTTP_ADDR": occupied.Addr().String()},
		"callback": {"DB_PRIMARY_DSN": testdb.DSN(t), "GRPC_ADDR": "127.0.0.1:0", "HTTP_ADDR": "127.0.0.1:0", "PHONE_PROVIDER": "zvonok", "ZVONOK_PUBLIC_KEY": "fake-test-key", "ZVONOK_CAMPAIGN_ID": "123", "ZVONOK_CALLBACK_ADDR": occupied.Addr().String()},
	}
	// Listeners each failing startup binds successfully before the failing one.
	bound := map[string]int{"http": 1, "callback": 2}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			for k, v := range base {
				t.Setenv(k, v)
			}
			for k, v := range extra {
				t.Setenv(k, v)
			}
			listened := false
			recorder := &listenRecorder{}
			if err := runServe(t.Context(), serveHooks{listening: func(boundAddrs) { listened = true }, listen: recorder.listen}); err == nil {
				t.Fatal("want an error")
			} else if name == "outbox" && !strings.Contains(err.Error(), "peer address required") {
				t.Fatal(err)
			} else if name == "http" && !strings.HasPrefix(err.Error(), "http: ") {
				t.Fatal(err)
			}
			if listened {
				t.Fatal("failed startup reported its listeners as serving")
			}
			recorder.requireAllClosed(t, bound[name])
		})
	}
}

func setSigningKey(t *testing.T) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JWT_SIGNING_KEY_FILE", path)
	t.Setenv("JWT_KEY_ID", "serve-test")
	t.Setenv("JWT_ISSUER", "test-user")
	t.Setenv("JWT_AUDIENCE", "test-app")
	secretPath := filepath.Join(t.TempDir(), "auth.key")
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretPath, secret[:], 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTH_IDEMPOTENCY_KEY_FILE", secretPath)
}

func setPhoneKey(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "phone.key")
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PHONE_RESULT_KEY_FILE", path)
}
