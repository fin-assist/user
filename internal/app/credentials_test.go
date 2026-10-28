package app

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/ratelimit"
	"github.com/fin-assist/user/internal/testdb"
)

// FIN-349: the whole assembly against PostgreSQL starts with the private keys systemd
// hands a DynamicUser (0400, or 0440 with the ACL mask on tmpfs) in $CREDENTIALS_DIRECTORY,
// and refuses 0440 outside it and a credential readable by others, before serving.
func TestIntegrationAssemblyStartsWithSystemdCredentialModes(t *testing.T) {
	dsn := testdb.DSN(t)
	for _, tc := range []struct {
		name   string
		inside bool
		perm   os.FileMode
		only   string // when set, only this file gets perm; the others are 0440 credentials
		want   string // "" starts
	}{
		{"credentials 0400", true, 0o400, "", ""},
		{"credentials 0440", true, 0o440, "", ""},
		{"credentials 0444", true, 0o444, "", "invalid key file or permissions"},
		{"0440 outside the credentials directory", false, 0o440, "", "invalid key file or permissions"},
		{"auth key 0444", true, 0o444, "auth-idempotency", "auth: key must be a regular owner-only file"},
		{"auth key 0440 outside the credentials directory", false, 0o440, "auth-idempotency", "auth: key must be a regular owner-only file"},
		{"phone key 0444", true, 0o444, "phone-result", "phone result: key must be a regular owner-only file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credentials, outside := t.TempDir(), t.TempDir()
			t.Setenv("CREDENTIALS_DIRECTORY", credentials)
			dir := outside
			if tc.inside {
				dir = credentials
			}
			// With only, the other files are valid credentials and dir holds just that file.
			placeOf := func(name string) (string, os.FileMode) {
				if tc.only == "" || tc.only == name {
					return dir, tc.perm
				}
				return credentials, 0o440
			}
			signingCfg := signingConfig(t)
			pemData, err := os.ReadFile(signingCfg.PrivateKeyFile)
			if err != nil {
				t.Fatal(err)
			}
			files := map[string][]byte{"jwt-signing": pemData, "auth-idempotency": bytes.Repeat([]byte{1}, 32), "phone-result": bytes.Repeat([]byte{2}, 32)}
			paths := map[string]string{}
			for name, content := range files {
				where, perm := placeOf(name)
				paths[name] = filepath.Join(where, name)
				if err := os.WriteFile(paths[name], content, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(paths[name], perm); err != nil {
					t.Fatal(err)
				}
			}
			signingCfg.PrivateKeyFile = paths["jwt-signing"]
			cfg := config.Config{
				Service: "user", Env: config.EnvDev, Auth: config.AuthConfig{Mode: config.AuthInsecureDev},
				Signing:  signingCfg,
				DB:       config.DBConfig{PrimaryDSN: dsn, MaxConns: 2},
				GRPC:     config.GRPCConfig{MaxRecvMsgSize: 4 << 20},
				Phone:    config.PhoneConfig{ResultKeyFile: paths["phone-result"], ResultKeyID: "test", MaxStreams: 2, UnknownTimeout: 5 * time.Minute, ResyncInterval: time.Second},
				Sessions: config.SessionsConfig{IdempotencyKeyFile: paths["auth-idempotency"], RefreshTTL: time.Hour, MaxConns: 2},
				Outbox:   config.OutboxConfig{TimezoneTargets: []string{"assistant"}, DeletionTargets: []string{"bank-statement-data", "analytics", "assistant"}, MaxConns: 6, PollInterval: time.Second},
				Peers:    map[string]string{"bank-statement-data": "passthrough:///127.0.0.1:1", "analytics": "passthrough:///127.0.0.1:1", "assistant": "passthrough:///127.0.0.1:1"},
				Limits:   ratelimit.Defaults(),
			}
			a, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry())
			if tc.want != "" {
				if err == nil {
					a.Close()
					t.Fatal("unsafe credential mode accepted")
				}
				if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), dir) {
					t.Fatalf("unclear or path-leaking refusal: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			if err := a.GRPC.Check(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
