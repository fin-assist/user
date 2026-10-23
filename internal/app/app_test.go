package app

import (
	"context"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/config"
)

// Missing cryptographic dependencies fail closed; dev credentials require dev explicitly.
func TestIdentityFailsClosed(t *testing.T) {
	_, err := buildIdentity(config.Config{Service: "user", Env: config.EnvProd, Auth: config.AuthConfig{Mode: config.AuthVerify}}, nil, prometheus.NewRegistry())
	if err == nil {
		t.Fatalf("error %v", err)
	}
	ids, err := buildIdentity(config.Config{Service: "user", Env: config.EnvDev, Auth: config.AuthConfig{Mode: config.AuthInsecureDev}}, nil, prometheus.NewRegistry())
	if err != nil || ids.services == nil || ids.users == nil || ids.tokens == nil {
		t.Fatalf("dev: %v", err)
	}
}

func TestNewFailsOnUnreachableDatabase(t *testing.T) {
	cfg := config.Config{Service: "user", Env: config.EnvDev, Auth: config.AuthConfig{Mode: config.AuthInsecureDev},
		Signing: signingConfig(t),
		DB:      config.DBConfig{PrimaryDSN: "postgres://nobody@127.0.0.1:1/db?connect_timeout=1", MaxConns: 1}}
	if _, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry()); err == nil {
		t.Fatal("want an error")
	}
}
