package app

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/config"
)

func TestOutboxAssemblyFailsClosed(t *testing.T) {
	a := &App{Cfg: config.Config{Outbox: config.OutboxConfig{TimezoneTargets: []string{"assistant", "bank-statement-data"}, MaxConns: 5, PollInterval: time.Second}}}
	if _, _, _, err := wireOutbox(t.Context(), a, prometheus.NewRegistry()); err == nil || !strings.Contains(err.Error(), "peer address required") {
		t.Fatal(err)
	}
	a.Cfg.Outbox.MaxConns = 1
	if _, _, _, err := wireOutbox(t.Context(), a, prometheus.NewRegistry()); err == nil {
		t.Fatal("insufficient pool")
	}
	a.Cfg.Outbox.MaxConns = 5
	a.Cfg.Peers = map[string]string{"assistant": "passthrough:///peer", "bank-statement-data": "passthrough:///peer"}
	a.Cfg.DB.PrimaryDSN = ":invalid-dsn"
	if _, _, _, err := wireOutbox(t.Context(), a, prometheus.NewRegistry()); err == nil {
		t.Fatal("invalid dedicated pool")
	}
}
