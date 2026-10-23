// Package apitest runs the whole service in-process for API tests: a fresh PostgreSQL
// database with every migration source applied, the assembly of app.New (the same
// interceptors, access table and handlers production serves) on an in-memory listener,
// and a client connection to it. A test calls the service the way bank-bff or another
// service does, with generated clients, and asserts what a caller sees: status code,
// ErrorInfo reason and metadata, response fields (AGENTS.md, «Tests»).
//
//	env := apitest.Start(t)
//	c := operationsv1.NewOperationsServiceClient(env.Conn)
//	_, err := c.GetOperation(apitest.User(t.Context(), "u1"), &operationsv1.GetOperationRequest{Id: other})
//	if apitest.Reason(err) != "NOT_FOUND" { … }
package apitest

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"

	"github.com/fin-assist/user/internal/adapter/grpc/server"
	"github.com/fin-assist/user/internal/adapter/postgres/migrate"
	"github.com/fin-assist/user/internal/adapter/postgres/migrate/migratetest"
	"github.com/fin-assist/user/internal/app"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/ratelimit"
)

// Env is a running service.
type Env struct {
	App      *app.App
	Conn     *grpc.ClientConn
	DSN      string               // the test database, for arranging and checking rows directly
	Registry *prometheus.Registry // the same collectors served by the operational endpoint
}

// Start runs the service for the test and stops it at the end. mutate, if given, adjusts
// the configuration (peers, limits) before the assembly.
func Start(t *testing.T, mutate ...func(*config.Config)) *Env {
	t.Helper()
	ctx := context.Background()
	dsn := migratetest.DSN(t)

	mcfg := migrate.DefaultConfig(dsn)
	mcfg.Logger = slog.New(slog.DiscardHandler)
	if err := migrate.Up(ctx, mcfg, app.MigrationSources()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "user.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Service: "user",
		Signing: config.SigningConfig{PrivateKeyFile: keyPath, KeyID: "apitest", Issuer: "apitest-user", Audience: "apitest-client"},
		Env:     config.EnvDev,
		Auth:    config.AuthConfig{Mode: config.AuthInsecureDev},
		DB:      config.DBConfig{PrimaryDSN: dsn, MaxConns: 4},
		GRPC:    config.GRPCConfig{MaxRecvMsgSize: 4 << 20},
		Peers:   map[string]string{"assistant": "passthrough:///127.0.0.1:1", "bank-statement-data": "passthrough:///127.0.0.1:1"},
		Outbox:  config.OutboxConfig{TimezoneTargets: []string{"bank-statement-data", "assistant"}, MaxConns: 5, PollInterval: 20 * time.Millisecond},
		Limits:  ratelimit.Defaults(),
	}
	for _, m := range mutate {
		m(&cfg)
	}
	registry := prometheus.NewRegistry()
	a, err := app.New(ctx, cfg, slog.New(slog.DiscardHandler), registry)
	if err != nil {
		t.Fatalf("app: %v", err)
	}
	if err := a.GRPC.Check(); err != nil {
		a.Close()
		t.Fatalf("server: %v", err)
	}

	lis := bufconn.Listen(1 << 20)
	go func() { _ = a.GRPC.Serve(lis) }()
	a.GRPC.SetServing(true)
	conn, err := grpc.NewClient("passthrough:///apitest",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.GRPC.Shutdown(sctx, 0)
		a.Close()
	})
	return &Env{App: a, Conn: conn, DSN: dsn, Registry: registry}
}

// User returns ctx for a user call made by bank-bff on behalf of userID.
func User(ctx context.Context, userID string) context.Context {
	return metadata.AppendToOutgoingContext(ctx,
		"x-service-authorization", "Bearer dev:bank-bff",
		"authorization", "Bearer dev:"+userID)
}

// Caller returns ctx for a system call made by the service caller (no user token).
func Caller(ctx context.Context, caller string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "x-service-authorization", "Bearer dev:"+caller)
}

// Reason returns ErrorInfo.reason of err: "" for nil, a bare status or a non-status error.
func Reason(err error) string { return server.Reason(err) }
