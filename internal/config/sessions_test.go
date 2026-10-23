package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSessionsConfigAndKeyFile(t *testing.T) {
	r := reader{command: "serve", getenv: func(string) string { return "" }}
	c := r.sessions()
	if c.RefreshTTL != 60*24*time.Hour || c.MaxConns != 8 || c.IdempotencyKeyFile != "" || len(r.errs) != 0 {
		t.Fatal(c, r.errs)
	}
	for name, value := range map[string]string{"AUTH_DB_MAX_CONNS": "0", "REFRESH_TOKEN_TTL": "-1s"} {
		r := reader{command: "serve", getenv: func(k string) string {
			if k == name {
				return value
			}
			return ""
		}}
		_ = r.sessions()
		if len(r.errs) == 0 {
			t.Fatal(name)
		}
	}
	for _, v := range []string{"2147483648", "no-int"} {
		r := reader{command: "serve", getenv: func(k string) string {
			if k == "AUTH_DB_MAX_CONNS" {
				return v
			}
			return ""
		}}
		_ = r.sessions()
		if len(r.errs) == 0 {
			t.Fatal(v)
		}
	}
	path := filepath.Join(t.TempDir(), "key")
	want := bytes.Repeat([]byte{42}, 32)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	c.IdempotencyKeyFile = path
	got, err := LoadSessionKey(c)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("key load failed", err)
	}
	for _, mutate := range []func(*SessionsConfig){
		func(c *SessionsConfig) { c.RefreshTTL = 0 }, func(c *SessionsConfig) { c.MaxConns = 0 },
		func(c *SessionsConfig) { c.IdempotencyKeyFile = "" }, func(c *SessionsConfig) { c.IdempotencyKeyFile = "/absent-test-key" },
		func(c *SessionsConfig) { c.IdempotencyKeyFile = t.TempDir() },
	} {
		bad := c
		mutate(&bad)
		if _, err := LoadSessionKey(bad); err == nil {
			t.Fatal("bad key configuration accepted")
		}
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionKey(c); err == nil {
		t.Fatal("public key file accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 31, 33, 1000} {
		if err := os.WriteFile(path, []byte(strings.Repeat("s", n)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadSessionKey(c); err == nil || strings.Contains(err.Error(), strings.Repeat("s", 31)) {
			t.Fatal("bad length or secret disclosure", err)
		}
	}
}
