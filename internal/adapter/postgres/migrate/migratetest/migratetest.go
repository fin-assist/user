// Package migratetest provides a PostgreSQL for tests and the "up → down → up" check that
// every migration source (the service and each library) runs in CI.
package migratetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	appmigrate "github.com/fin-assist/user/internal/adapter/postgres/migrate"
)

// DefaultImage is the PostgreSQL image of the test container; override with TEST_POSTGRES_IMAGE.
const DefaultImage = "postgres:18-alpine"

var (
	adminOnce sync.Once
	adminDSN  string
	adminErr  error
)

// DSN returns the DSN of a fresh, empty database dropped at the end of the test.
//
// The server is TEST_DATABASE_URL when set (a local PostgreSQL), otherwise a container
// started once per test binary. Without either the test is skipped locally and fails in CI
// (CI is set), so the check cannot pass by silently not running.
func DSN(t testing.TB) string {
	t.Helper()
	adminOnce.Do(func() { adminDSN, adminErr = server() })
	if adminErr != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("no PostgreSQL for tests: %v", adminErr)
		}
		t.Skipf("no PostgreSQL for tests (set TEST_DATABASE_URL or run Docker): %v", adminErr)
	}

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()

	name := "t_" + randomHex(8)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), adminDSN)
		if err != nil {
			return
		}
		defer func() { _ = c.Close(context.Background()) }()
		_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

func server() (string, error) {
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		return dsn, nil
	}
	img := os.Getenv("TEST_POSTGRES_IMAGE")
	if img == "" {
		img = DefaultImage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c, err := tcpostgres.Run(ctx, img,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		return "", err
	}
	// The container lives until the test binary exits; Ryuk removes it.
	return c.ConnectionString(ctx, "sslmode=disable")
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// CheckUpDownUp applies the migrations of src one by one on an empty database, and for
// each version checks up → down → up: the down file really undoes the up file (the same
// up applies again) and the source ends at its last version, clean.
func CheckUpDownUp(t *testing.T, src appmigrate.Source) {
	t.Helper()
	if err := appmigrate.Validate(src); err != nil {
		t.Fatal(err)
	}
	dsn := DSN(t)
	m := open(t, dsn, src)

	for {
		before := version(t, m)
		err := m.Steps(1)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, migrate.ErrNoChange) {
			break // no next migration
		}
		if err != nil {
			t.Fatalf("%s: up from %d: %v", src.Name, before, err)
		}
		v := version(t, m)
		if irreversible(t, src, v) {
			continue // its down file only fails on purpose
		}
		if err := m.Steps(-1); err != nil {
			t.Fatalf("%s: down from %d: %v", src.Name, v, err)
		}
		if got := version(t, m); got != before {
			t.Fatalf("%s: down from %d left version %d, want %d", src.Name, v, got, before)
		}
		if err := m.Steps(1); err != nil {
			t.Fatalf("%s: up again to %d: %v", src.Name, v, err)
		}
	}
	if _, dirty, _ := m.Version(); dirty {
		t.Fatalf("%s: dirty at the end", src.Name)
	}
}

// IrreversibleMarker starts the down file of a step that cannot be rolled back (a "remove"
// step of a two-step schema change); the rest of the file raises an error.
const IrreversibleMarker = "-- irreversible"

func irreversible(t *testing.T, src appmigrate.Source, v uint) bool {
	t.Helper()
	files, err := fs.Glob(src.FS, "*_*.down.sql")
	var matches []string
	for _, name := range files {
		prefix, _, _ := strings.Cut(name, "_")
		n, parseErr := strconv.ParseUint(prefix, 10, 64)
		if parseErr == nil && n == uint64(v) {
			matches = append(matches, name)
		}
	}
	if err != nil || len(matches) != 1 {
		t.Fatalf("%s: down file of %d: %v %v", src.Name, v, matches, err)
	}
	b, err := fs.ReadFile(src.FS, matches[0])
	if err != nil {
		t.Fatal(err)
	}
	return strings.HasPrefix(strings.TrimSpace(string(b)), IrreversibleMarker)
}

func version(t *testing.T, m *migrate.Migrate) uint {
	t.Helper()
	v, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	if dirty {
		t.Fatalf("dirty at version %d", v)
	}
	return v
}

func open(t *testing.T, dsn string, src appmigrate.Source) *migrate.Migrate {
	t.Helper()
	pc, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDB(*pc)
	t.Cleanup(func() { _ = db.Close() })
	drv, err := pgxmigrate.WithInstance(db, &pgxmigrate.Config{MigrationsTable: src.Table})
	if err != nil {
		t.Fatal(err)
	}
	files, err := iofs.New(src.FS, ".")
	if err != nil {
		t.Fatal(fmt.Errorf("%s: %w", src.Name, err))
	}
	m, err := migrate.NewWithInstance("iofs", files, "pgx5", drv)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
