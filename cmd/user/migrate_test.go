package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestUnitMigrateConfig(t *testing.T) {
	t.Setenv("DB_PRIMARY_DSN", "")
	if _, err := migrateConfig(); err == nil {
		t.Fatal("missing DSN must be an error")
	}

	t.Setenv("DB_PRIMARY_DSN", "postgres://localhost/db")
	cfg, err := migrateConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LockTimeout != 5*time.Second || cfg.StatementTimeout != 15*time.Minute || cfg.WaitTimeout != 15*time.Minute {
		t.Fatalf("defaults: %+v", cfg)
	}

	t.Setenv("MIGRATE_LOCK_TIMEOUT", "2s")
	t.Setenv("MIGRATE_WAIT_TIMEOUT", "1h")
	cfg, err = migrateConfig()
	if err != nil || cfg.LockTimeout != 2*time.Second || cfg.WaitTimeout != time.Hour {
		t.Fatalf("overrides: %+v %v", cfg, err)
	}

	for _, bad := range []string{"soon", "0s", "-1s"} {
		t.Setenv("MIGRATE_STATEMENT_TIMEOUT", bad)
		if _, err := migrateConfig(); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}

func TestUnitMigrateUsage(t *testing.T) {
	t.Setenv("DB_PRIMARY_DSN", "postgres://localhost/db")
	for _, args := range [][]string{
		{}, {"sideways"}, {"up", "extra"}, {"version", "extra"},
		{"down", "service"}, {"force", "service", "x"},
	} {
		err := runMigrate(context.Background(), args, &bytes.Buffer{})
		var u usageError
		if !errors.As(err, &u) {
			t.Errorf("%v: error %v, want usage", args, err)
		}
	}
	err := runMigrate(context.Background(), []string{"down", "nope", "1"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "unknown source") {
		t.Errorf("unknown source: %v", err)
	}
}
