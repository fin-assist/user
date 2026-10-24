package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	idempotency "github.com/fin-assist/go-idempotency"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/adapter/client"
	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/adapter/postgres/migrate/migratetest"
	"github.com/fin-assist/user/internal/config"
)

func TestIntegrationSessionsAssemblyFailuresCloseDedicatedPools(t *testing.T) {
	dsn := migratetest.DSN(t)
	db, err := postgres.Open(t.Context(), postgres.Options{PrimaryDSN: dsn, MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keyFile := filepath.Join(t.TempDir(), "auth.key")
	if err := os.WriteFile(keyFile, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := prometheus.NewRegistry()
	metrics, err := client.NewMetrics(reg)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{DB: db, Tokens: client.DevToken("user"), Clients: metrics, Log: slog.New(slog.DiscardHandler), Cfg: config.Config{Service: "assembly-test", DB: config.DBConfig{PrimaryDSN: dsn}, Sessions: config.SessionsConfig{RefreshTTL: time.Hour, MaxConns: 1}, Outbox: config.OutboxConfig{DeletionTargets: []string{"bank-statement-data", "analytics", "assistant"}, TimezoneTargets: []string{"assistant"}, MaxConns: 6, PollInterval: time.Second}, Peers: map[string]string{"analytics": "passthrough:///127.0.0.1:1", "bank-statement-data": "passthrough:///127.0.0.1:1", "assistant": "passthrough:///127.0.0.1:1"}}}
	if _, _, err := wire(t.Context(), a, reg); err == nil {
		t.Fatal("missing key accepted")
	}
	a.Cfg.Sessions.IdempotencyKeyFile = keyFile
	a.Cfg.DB.PrimaryDSN = ":invalid"
	if _, _, err := wireSessions(t.Context(), a, nil, reg); err == nil {
		t.Fatal("invalid auth database accepted")
	}
	a.Cfg.DB.PrimaryDSN = dsn
	cipher, err := idempotency.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idempotency.New(db, idempotency.Options{Clock: time.Now, Cipher: cipher, Registerer: reg, Methods: map[string]idempotency.Policy{userv1.AuthService_Register_FullMethodName: {Encrypt: true}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := wireSessions(t.Context(), a, nil, reg); err == nil {
		t.Fatal("duplicate metric collector accepted")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var n int
		if err := db.Primary().QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND application_name IN ('assembly-test-outbox','assembly-test-auth')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("dedicated pool leaked")
		case <-tick.C:
		}
	}
}
