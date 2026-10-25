package migrations_test

import (
	"testing"

	"github.com/fin-assist/go-migrate/migratetest"

	"github.com/fin-assist/go-migrate"

	"github.com/fin-assist/user/internal/testdb"
	"github.com/fin-assist/user/migrations"
)

// Every migration of the service applies, rolls back and applies again on an empty database.
func TestIntegrationUpDownUp(t *testing.T) {
	src := migrate.Source{Name: "service", Table: "schema_migrations", FS: migrations.FS}
	if err := migrate.Validate(src); err != nil {
		t.Fatal(err)
	}
	if !hasMigrations(t) {
		t.Skip("the service has no migrations yet")
	}
	migratetest.CheckUpDownUp(t, testdb.DSN(t), src)
}

func hasMigrations(t *testing.T) bool {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if len(e.Name()) > 7 && e.Name()[len(e.Name())-7:] == ".up.sql" {
			return true
		}
	}
	return false
}
