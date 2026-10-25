// Package testdb selects the test-owned PostgreSQL server. It remains part of the
// service coverage denominator; database isolation and migration checks belong to
// the public go-migrate harness. It is never called by the runtime assembly.
package testdb

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/fin-assist/go-migrate/migratetest"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// DefaultImage is PostgreSQL 18; TEST_POSTGRES_IMAGE can override the container image.
const DefaultImage = "postgres:18-alpine"

var (
	adminOnce sync.Once
	adminDSN  string
	adminErr  error
)

// DSN returns a fresh database dropped by the shared harness at test cleanup.
// The admin server comes from TEST_DATABASE_URL or a per-binary container. Missing
// dependencies fail in CI; local skips are rejected by the coverage suite guard.
func DSN(t testing.TB) string {
	t.Helper()
	adminOnce.Do(func() { adminDSN, adminErr = server() })
	if adminErr != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("no PostgreSQL for tests: set TEST_DATABASE_URL or run Docker")
		}
		t.Skip("no PostgreSQL for tests: set TEST_DATABASE_URL or run Docker")
	}
	return migratetest.DSN(t, adminDSN)
}

func server() (string, error) {
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		return dsn, nil
	}
	image := os.Getenv("TEST_POSTGRES_IMAGE")
	if image == "" {
		image = DefaultImage
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, err := tcpostgres.Run(ctx, image,
		tcpostgres.WithDatabase("postgres"), tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"), tcpostgres.BasicWaitStrategies())
	if err != nil {
		return "", errors.New("start test PostgreSQL failed")
	}
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return "", errors.New("read test PostgreSQL connection failed")
	}
	return dsn, nil
}
