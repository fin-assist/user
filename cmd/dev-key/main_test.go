package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/signing"
)

func TestUnitGenerateDoesNotReplaceExistingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := generate(path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := signing.Load(signing.Options{PrivateKeyFile: path, KeyID: "local", Issuer: "local-user", Audience: "local-app", Clock: time.Now}); err != nil {
		t.Fatal(err)
	}
	if err := generate(path); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("key replaced")
	}
	if err := generate(filepath.Join(t.TempDir(), "missing", "key.pem")); err == nil {
		t.Fatal("missing directory accepted")
	}
}

func TestUnitAESKeyIsPrivateAndNeverReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.key")
	if err := generateAES(path); err != nil {
		t.Fatal(err)
	}
	c := config.SessionsConfig{IdempotencyKeyFile: path, RefreshTTL: time.Hour, MaxConns: 1}
	before, err := config.LoadSessionKey(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := generateAES(path); err != nil {
		t.Fatal(err)
	}
	after, err := config.LoadSessionKey(c)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("AES key replaced", err)
	}
	if err := generateAES(filepath.Join(t.TempDir(), "absent", "key")); err == nil {
		t.Fatal("missing directory accepted")
	}
}

func TestUnitDevKeyCLI(t *testing.T) {
	if os.Getenv("FINASSIST_TEST_DEV_KEY") == "1" {
		var args []string
		if err := json.Unmarshal([]byte(os.Getenv("FINASSIST_TEST_ARGS")), &args); err != nil {
			t.Fatal(err)
		}
		os.Args = append([]string{"dev-key"}, args...)
		main()
		return
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cli.pem")
	for _, tt := range []struct {
		args []string
		code int
	}{
		{nil, 2}, {[]string{filepath.Join(t.TempDir(), "absent", "key")}, 1}, {[]string{path}, 0},
		{[]string{"--aes", filepath.Join(t.TempDir(), "aes.key")}, 0},
	} {
		args, err := json.Marshal(tt.args)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestUnitDevKeyCLI$")
		cmd.Env = append(os.Environ(), "FINASSIST_TEST_DEV_KEY=1", "FINASSIST_TEST_ARGS="+string(args))
		if err := cmd.Run(); err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != tt.code {
				t.Fatalf("exit %v, want %d", err, tt.code)
			}
		} else if tt.code != 0 {
			t.Fatalf("expected exit %d", tt.code)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
