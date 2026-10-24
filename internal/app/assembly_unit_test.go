package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/config"
)

func TestUnitPhoneAndSessionAssemblyRejectsInvalidKeysBeforeResources(t *testing.T) {
	a := &App{Log: slog.New(slog.DiscardHandler), Cfg: config.Config{Env: config.EnvDev, Phone: config.PhoneConfig{ResultKeyID: "key", MaxStreams: 1, ResyncInterval: time.Second}}}
	if _, _, err := wirePhone(t.Context(), a, prometheus.NewRegistry()); err == nil {
		t.Fatal("missing phone key accepted")
	}
	path := filepath.Join(t.TempDir(), "phone.key")
	if err := os.WriteFile(path, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	a.Cfg.Phone.ResultKeyFile = path
	if _, _, err := wirePhone(t.Context(), a, prometheus.NewRegistry()); err == nil {
		t.Fatal("missing session key accepted")
	}
	a.Cfg.Sessions = config.SessionsConfig{IdempotencyKeyFile: path, RefreshTTL: time.Hour, MaxConns: 1}
	if _, _, err := wirePhone(t.Context(), a, prometheus.NewRegistry()); err == nil {
		t.Fatal("same encryption key accepted for both purposes")
	}
	a.Cfg.Sessions.IdempotencyKeyFile = ""
	if _, _, err := wireSessions(t.Context(), a, nil, prometheus.NewRegistry()); err == nil {
		t.Fatal("missing replay key accepted")
	}
	a.Cfg.Sessions.IdempotencyKeyFile = path
	a.Cfg.DB.PrimaryDSN = "not a DSN"
	if _, _, err := wireSessions(t.Context(), a, nil, prometheus.NewRegistry()); err == nil {
		t.Fatal("invalid database config accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	a.Cfg.DB.PrimaryDSN = "postgres://localhost:1/postgres?pool_min_conns=0&pool_min_idle_conns=0"
	if _, _, err := wireSessions(ctx, a, nil, prometheus.NewRegistry()); err == nil {
		t.Fatal("cancelled assembly accepted")
	}
	if len(AccessPolicy()) == 0 || len(MigrationSources()) != 4 {
		t.Fatal("assembly declarations missing")
	}
}
