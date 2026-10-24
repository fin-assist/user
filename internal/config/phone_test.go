package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUnitPhoneConfigurationAndIndependentKey(t *testing.T) {
	get, list := env(base())
	cfg, err := load(get, noFiles, list)
	if err != nil || cfg.Phone.MaxStreams != 1024 || cfg.Phone.ResyncInterval != 5*time.Second || cfg.Phone.ResultKeyID != "primary" {
		t.Fatal(cfg.Phone, err)
	}
	for name, values := range map[string][]string{"PHONE_WATCH_MAX_STREAMS": {"0", "100001", "bad"}, "PHONE_WATCH_RESYNC_INTERVAL": {"0s", "31s", "bad"}, "PHONE_RESULT_KEY_ID": {"spaces bad", "path/id", strings.Repeat("a", 65)}} {
		for _, v := range values {
			vars := base()
			vars[name] = v
			g, l := env(vars)
			if _, err := load(g, noFiles, l); err == nil {
				t.Fatal(name, "invalid configuration accepted")
			}
		}
	}
	path := filepath.Join(t.TempDir(), "result.key")
	key := bytes.Repeat([]byte{42}, 32)
	if err := os.WriteFile(path, key, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.Phone.ResultKeyFile = path
	got, err := LoadPhoneKey(cfg.Phone)
	if err != nil || !bytes.Equal(got, key) {
		t.Fatal("key load", err)
	}
	for _, mutate := range []func(*PhoneConfig){func(c *PhoneConfig) { c.ResultKeyFile = "" }, func(c *PhoneConfig) { c.ResultKeyID = "bad/id" }, func(c *PhoneConfig) { c.ResultKeyFile = t.TempDir() }, func(c *PhoneConfig) { c.ResultKeyFile = "/missing/result.key" }, func(c *PhoneConfig) { c.MaxStreams = 0 }} {
		bad := cfg.Phone
		mutate(&bad)
		if _, err := LoadPhoneKey(bad); err == nil {
			t.Fatal("bad key accepted")
		}
	}
	for _, data := range [][]byte{make([]byte, 31), make([]byte, 33)} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadPhoneKey(cfg.Phone); err == nil {
			t.Fatal("key length accepted")
		}
	}
	if err := os.WriteFile(path, key, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPhoneKey(cfg.Phone); err == nil {
		t.Fatal("public key accepted")
	}
}
