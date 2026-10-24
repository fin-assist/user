package migrate

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

type failingFS struct{}

func (failingFS) Open(string) (fs.File, error) { return nil, fs.ErrPermission }

func TestUnitMigrationSourcesFailBeforeDatabaseAndKeepDiagnostics(t *testing.T) {
	valid := Source{Name: "service", Table: "schema_migrations", FS: fstest.MapFS{}}
	for _, sources := range [][]Source{{{}}, {valid, valid}, {{Name: "service", Table: "table", FS: failingFS{}}}} {
		if err := validateAll(sources); err == nil {
			t.Fatal("invalid source list accepted")
		}
	}
	if err := validateAll([]Source{valid}); err != nil {
		t.Fatal(err)
	}
	if err := Validate(Source{Name: "service", FS: failingFS{}}); !errors.Is(err, fs.ErrPermission) {
		t.Fatal("filesystem error lost", err)
	}
	if err := Up(t.Context(), DefaultConfig("invalid DSN"), []Source{valid}); err == nil {
		t.Fatal("invalid DSN accepted")
	}
	if err := Up(t.Context(), DefaultConfig(""), nil); err != nil {
		t.Fatal(err)
	}
	if statuses, err := Versions(t.Context(), DefaultConfig(""), nil); err != nil || len(statuses) != 0 {
		t.Fatal(statuses, err)
	}
	if _, err := Versions(t.Context(), DefaultConfig("invalid DSN"), []Source{valid}); err == nil {
		t.Fatal("invalid version DSN accepted")
	}
	for _, steps := range []int{0, -1} {
		if err := Down(t.Context(), DefaultConfig(""), valid, steps); err == nil {
			t.Fatal("nonpositive rollback accepted")
		}
	}
	bad := Source{Name: "service", Table: "table", FS: fstest.MapFS{"000001_missing.up.sql": {Data: []byte("SELECT 1")}}}
	if err := Down(t.Context(), DefaultConfig(""), bad, 1); err == nil {
		t.Fatal("unpaired rollback accepted")
	}
	if err := Force(t.Context(), DefaultConfig(""), bad, 1); err == nil {
		t.Fatal("unpaired force accepted")
	}
	for _, call := range []func() error{func() error { return Down(t.Context(), DefaultConfig("invalid DSN"), valid, 1) }, func() error { return Force(t.Context(), DefaultConfig("invalid DSN"), valid, 1) }} {
		if err := call(); err == nil {
			t.Fatal("invalid DSN hidden")
		}
	}
}

func TestUnitMigrationFileSelectionAndLazyConnectionOptions(t *testing.T) {
	for _, tc := range []struct {
		files fs.FS
		empty bool
	}{{fstest.MapFS{}, true}, {fstest.MapFS{"README.md": {Data: []byte("notes")}}, true}, {fstest.MapFS{"000001_a.up.sql": {Data: []byte("SELECT 1")}}, false}, {failingFS{}, false}} {
		if got := empty(tc.files); got != tc.empty {
			t.Fatalf("empty=%v want=%v", got, tc.empty)
		}
	}
	if millis(1500*time.Millisecond) != "1500" {
		t.Fatal("timeout precision lost")
	}
	if _, err := open(DefaultConfig("not a DSN")); err == nil {
		t.Fatal("invalid lazy connection accepted")
	}
	db, err := open(DefaultConfig("postgres://127.0.0.1:1/postgres"))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close() // OpenDB is lazy; no Ping or SQL is executed here.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if release, err := waitTurn(ctx, DefaultConfig("postgres://127.0.0.1:1/postgres"), Source{Table: "schema_migrations"}); err == nil || release != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled wait: release/error", release != nil, err)
	}
	sources := []Source{{Name: "z"}, {Name: "a"}}
	if _, err := Find(sources, "missing"); err == nil || !strings.Contains(err.Error(), "[a z]") {
		t.Fatal("source list not deterministic", err)
	}
	var out bytes.Buffer
	l := logger{l: slog.New(slog.NewTextHandler(&out, nil))}
	l.Printf("version %d", 2)
	if l.Verbose() || !strings.Contains(out.String(), "version 2") {
		t.Fatal("migration log adapter", out.String())
	}
}
