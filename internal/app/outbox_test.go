package app

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/go-grpc/client"

	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/testdb"
)

func TestUnitOutboxAssemblyFailsClosed(t *testing.T) {
	a := &App{Cfg: config.Config{Outbox: config.OutboxConfig{DeletionTargets: []string{"bank-statement-data", "analytics", "assistant"}, TimezoneTargets: []string{"assistant", "bank-statement-data"}, MaxConns: 6, PollInterval: time.Second}}}
	if _, _, _, err := wireOutbox(t.Context(), a, prometheus.NewRegistry()); err == nil || !strings.Contains(err.Error(), "peer address required") {
		t.Fatal(err)
	}
	a.Cfg.Outbox.MaxConns = 1
	if _, _, _, err := wireOutbox(t.Context(), a, prometheus.NewRegistry()); err == nil {
		t.Fatal("insufficient pool")
	}
	a.Cfg.Outbox.MaxConns = 6
	a.Cfg.Peers = map[string]string{"analytics": "passthrough:///peer", "assistant": "passthrough:///peer", "bank-statement-data": "passthrough:///peer"}
	a.Cfg.DB.PrimaryDSN = ":invalid-dsn"
	if _, _, _, err := wireOutbox(t.Context(), a, prometheus.NewRegistry()); err == nil {
		t.Fatal("invalid dedicated pool")
	}
}

func TestIntegrationOutboxConstructionFailureClosesDedicatedPool(t *testing.T) {
	dsn := testdb.DSN(t)
	db, err := postgres.Open(t.Context(), postgres.Options{PrimaryDSN: dsn, MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reg := prometheus.NewRegistry()
	metrics, err := client.NewMetrics(reg)
	if err != nil {
		t.Fatal(err)
	}
	a := &App{DB: db, Tokens: client.DevToken("user"), Clients: metrics, Log: slog.New(slog.DiscardHandler), Cfg: config.Config{
		Service: "constructor-test", DB: config.DBConfig{PrimaryDSN: dsn}, Outbox: config.OutboxConfig{DeletionTargets: []string{"bank-statement-data", "analytics", "assistant"}, TimezoneTargets: []string{"assistant", "bank-statement-data"}, MaxConns: 6, PollInterval: time.Second}, Peers: map[string]string{"analytics": "passthrough:///127.0.0.1:1", "assistant": "passthrough:///127.0.0.1:1", "bank-statement-data": "dns:///%"},
	}}
	if _, _, _, err := wireOutbox(t.Context(), a, reg); err == nil {
		t.Fatal("invalid second peer accepted")
	}
	checkConnections := func(want int) {
		t.Helper()
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			var n int
			if err := db.Primary().QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND application_name='constructor-test-outbox'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n == want {
				return
			}
			select {
			case <-deadline.C:
				t.Fatalf("dedicated connections=%d expected=%d", n, want)
			case <-tick.C:
			}
		}
	}
	checkConnections(0)
	a.Cfg.Peers["bank-statement-data"] = "passthrough:///127.0.0.1:1"
	_, _, closeFirst, err := wireOutbox(t.Context(), a, reg)
	if err != nil {
		t.Fatal(err)
	}
	defer closeFirst()
	if _, _, _, err := wireOutbox(t.Context(), a, reg); err == nil {
		t.Fatal("duplicate metric registry accepted")
	}
	checkConnections(1)
	closeFirst()
	checkConnections(0)
}
