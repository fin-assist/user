package app

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/config"
)

// Outside dev the service refuses to start until real verifiers are wired (FIN-119),
// instead of serving with identities nobody checks.
func TestIdentityFailsClosed(t *testing.T) {
	_, _, _, err := identity(config.Config{Service: "user", Env: config.EnvProd, Auth: config.AuthConfig{Mode: config.AuthVerify}})
	if err == nil || !strings.Contains(err.Error(), "FIN-119") {
		t.Fatalf("error %v", err)
	}
	s, u, tok, err := identity(config.Config{Service: "user", Env: config.EnvDev, Auth: config.AuthConfig{Mode: config.AuthInsecureDev}})
	if err != nil || s == nil || u == nil || tok == nil {
		t.Fatalf("dev: %v", err)
	}
}

func TestNewFailsOnUnreachableDatabase(t *testing.T) {
	cfg := config.Config{Service: "user", Env: config.EnvDev, Auth: config.AuthConfig{Mode: config.AuthInsecureDev},
		DB: config.DBConfig{PrimaryDSN: "postgres://nobody@127.0.0.1:1/db?connect_timeout=1", MaxConns: 1}}
	if _, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry()); err == nil {
		t.Fatal("want an error")
	}
}
