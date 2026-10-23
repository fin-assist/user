package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fin-assist/user/internal/signing"
)

func TestGenerateDoesNotReplaceExistingKey(t *testing.T) {
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
