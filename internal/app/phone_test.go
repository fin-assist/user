package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	idempotency "github.com/fin-assist/go-idempotency"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/adapter/client"
	"github.com/fin-assist/user/internal/adapter/fakephone"
	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/adapter/postgres/migrate/migratetest"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/verification"
)

func TestIntegrationPhoneAssemblyKeySeparationAndFailureCleanup(t *testing.T) {
	dsn := migratetest.DSN(t)
	db, err := postgres.Open(t.Context(), postgres.Options{PrimaryDSN: dsn, MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	authPath, phonePath := filepath.Join(t.TempDir(), "auth.key"), filepath.Join(t.TempDir(), "phone.key")
	if err = os.WriteFile(authPath, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(phonePath, bytes.Repeat([]byte{42}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	goodPhone := config.PhoneConfig{ResultKeyFile: phonePath, ResultKeyID: "test", MaxStreams: 2, ResyncInterval: time.Second}
	a := &App{DB: db, Log: slog.New(slog.DiscardHandler), Tokens: client.DevToken("user"), Cfg: config.Config{Service: "phone-wiring", DB: config.DBConfig{PrimaryDSN: dsn}, Phone: goodPhone, Sessions: config.SessionsConfig{IdempotencyKeyFile: authPath, RefreshTTL: time.Hour, MaxConns: 2}, Outbox: config.OutboxConfig{TimezoneTargets: []string{"assistant"}, DeletionTargets: []string{"bank-statement-data", "analytics", "assistant"}, MaxConns: 6, PollInterval: time.Second}, Peers: map[string]string{"bank-statement-data": "passthrough:///127.0.0.1:1", "analytics": "passthrough:///127.0.0.1:1", "assistant": "passthrough:///127.0.0.1:1"}}}
	a.Cfg.Phone.ResultKeyFile = "/missing/phone.key"
	if _, _, err = wirePhone(t.Context(), a, prometheus.NewRegistry()); err == nil {
		t.Fatal("missing phone key accepted")
	}
	a.Cfg.Phone = goodPhone
	a.Cfg.Sessions.IdempotencyKeyFile = "/missing/auth.key"
	if _, _, err = wirePhone(t.Context(), a, prometheus.NewRegistry()); err == nil {
		t.Fatal("missing auth key accepted")
	}
	a.Cfg.Sessions.IdempotencyKeyFile = authPath
	a.Cfg.Phone.ResultKeyFile = authPath
	if _, _, err = wirePhone(t.Context(), a, prometheus.NewRegistry()); err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatal("same AES key accepted", err)
	}
	// Different paths with equal material must also be rejected.
	if err = os.WriteFile(phonePath, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	a.Cfg.Phone = goodPhone
	if _, _, err = wirePhone(t.Context(), a, prometheus.NewRegistry()); err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatal("copied key accepted", err)
	}
	if err = os.WriteFile(phonePath, bytes.Repeat([]byte{42}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	a.Cfg.Env = config.EnvProd
	prodRegistry := prometheus.NewRegistry()
	if _, closePhone, err := wirePhone(t.Context(), a, prodRegistry); err != nil {
		t.Fatal(err)
	} else {
		if changed, err := a.Verifications.Confirm(t.Context(), verification.Proof{Provider: "zvonok", CallID: "prod-call", Phone: "+70000001234"}); changed || !errors.Is(err, verification.ErrProof) {
			t.Fatal("prod assembly omitted protection", changed, err)
		}
		closePhone()
	}
	// Registration failure must also close the listener just opened by assembly.
	badGuardRegistry := prometheus.NewRegistry()
	badGuardRegistry.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "finassist_phone_test_confirmation_rejected_total", Help: "Already registered"}))
	if _, _, err := wirePhone(t.Context(), a, badGuardRegistry); err == nil {
		t.Fatal("duplicate prod guard metric accepted")
	}
	a.Cfg.Env, a.Cfg.Zvonok.Provider = config.EnvDev, "fake"
	a.phoneProvider = fakephone.New()
	if _, _, err := wirePhone(t.Context(), a, prometheus.NewRegistry()); err == nil {
		t.Fatal("two backends accepted")
	}
	a.phoneProvider = nil
	fakeRegistry := prometheus.NewRegistry()
	fakeRegistry.MustRegister(prometheus.NewGauge(prometheus.GaugeOpts{Name: "finassist_phone_callback_outcomes_24h", Help: "Already registered"}))
	if _, _, err := wirePhone(t.Context(), a, fakeRegistry); err == nil {
		t.Fatal("duplicate number collector accepted")
	}
	a.Cfg.Zvonok.Provider = ""
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewGauge(prometheus.GaugeOpts{Name: "finassist_phone_watch_active_streams", Help: "Already registered"}))
	if _, _, err = wirePhone(t.Context(), a, reg); err == nil {
		t.Fatal("duplicate listener collector accepted")
	}
	reg = prometheus.NewRegistry()
	a.Clients, err = client.NewMetrics(reg)
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := idempotency.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = idempotency.New(db, idempotency.Options{Clock: time.Now, Cipher: cipher, Registerer: reg, Methods: map[string]idempotency.Policy{userv1.AuthService_Register_FullMethodName: {Encrypt: true}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = wire(t.Context(), a, reg); err == nil {
		t.Fatal("duplicate auth collector accepted")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for {
		var n int
		if err = db.Primary().QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND application_name LIKE 'phone-wiring-%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		tick := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			tick.Stop()
			t.Fatal("assembly failure leaked resources")
		case <-tick.C:
		}
	}
}
