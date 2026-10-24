package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/fin-assist/user/internal/adapter/postgres/migrate/migratetest"
)

func TestIntegrationMigrationCLIReportsAndRepairsVersionWithoutCrossSourceRollback(t *testing.T) {
	dsn := migratetest.DSN(t)
	t.Setenv("DB_PRIMARY_DSN", dsn)
	var out bytes.Buffer
	invoke := func(args ...string) {
		t.Helper()
		out.Reset()
		if err := runMigrate(t.Context(), args, &out); err != nil {
			t.Fatal(args, err)
		}
	}
	invoke("up")
	invoke("version")
	before := out.String()
	if !strings.Contains(before, "service\t4") || !strings.Contains(before, "idempotency\t") {
		t.Fatal(before)
	}
	invoke("down", "service", "1")
	invoke("version")
	if !strings.Contains(out.String(), "service\t3") {
		t.Fatal(out.String())
	}
	// Force clears a dirty marker at the known existing schema version. It must
	// leave library version tables untouched and allow the pending upgrade.
	invoke("force", "service", "3")
	invoke("up")
	invoke("version")
	if out.String() != before {
		t.Fatalf("migration sources diverged:\nbefore %s\nafter %s", before, out.String())
	}
	for _, args := range [][]string{nil, {"up", "extra"}, {"version", "extra"}, {"down"}, {"force"}, {"down", "unknown", "1"}, {"force", "service", "bad"}, {"down", "service", "0"}, {"bogus"}} {
		if err := runMigrate(t.Context(), args, &out); err == nil {
			t.Fatalf("invalid deployment command accepted: %v", args)
		}
	}
	// Removing the database is an operational failure, not a misleading version.
	t.Setenv("DB_PRIMARY_DSN", "postgres://postgres@127.0.0.1:1/unavailable?sslmode=disable&connect_timeout=1")
	if err := runMigrate(t.Context(), []string{"version"}, &out); err == nil {
		t.Fatal("unavailable DB reported success")
	}
}
