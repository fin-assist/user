package app

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/adapter/fakephone"
	"github.com/fin-assist/user/internal/config"
)

func TestFakePhoneProdStartupFailsBeforeOpeningResources(t *testing.T) {
	cfg := config.Config{Env: config.EnvProd, Zvonok: config.ZvonokConfig{Provider: "fake"}}
	if a, err := New(t.Context(), cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry()); a != nil || err == nil || !strings.Contains(err.Error(), "fake backend") {
		t.Fatal(a, err)
	}
	// Trusted test-assembly seams cannot accidentally run the fake in prod either.
	a := &App{Cfg: config.Config{Env: config.EnvProd}, phoneProvider: fakephone.New()}
	if _, _, err := wirePhone(t.Context(), a, a.DB, a.Tombstones, prometheus.NewRegistry()); err == nil || !strings.Contains(err.Error(), "forbidden in prod") {
		t.Fatal(err)
	}
}
