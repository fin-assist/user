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
	"log/slog"
	"net"
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
)

// Env is a running service.
type Env struct {
	App  *app.App
	Conn *grpc.ClientConn
	DSN  string // the test database, for arranging and checking rows directly
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

	cfg := config.Config{
		Service: "apitest",
		Env:     config.EnvDev,
		Auth:    config.AuthConfig{Mode: config.AuthInsecureDev},
		DB:      config.DBConfig{PrimaryDSN: dsn, MaxConns: 4},
		GRPC:    config.GRPCConfig{MaxRecvMsgSize: 4 << 20},
		Peers:   map[string]string{},
	}
	for _, m := range mutate {
		m(&cfg)
	}
	a, err := app.New(ctx, cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry())
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
	return &Env{App: a, Conn: conn, DSN: dsn}
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
