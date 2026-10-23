package migratetest

import (
	"testing"
	"testing/fstest"

	appmigrate "github.com/fin-assist/user/internal/adapter/postgres/migrate"
)

func TestIrreversibleSupportsLibraryAndServiceNumbering(t *testing.T) {
	for _, prefix := range []string{"001", "000001"} {
		for _, body := range []string{"DROP TABLE example;", "-- irreversible\nSELECT 1;"} {
			source := appmigrate.Source{Name: "test", FS: fstest.MapFS{prefix + "_step.down.sql": {Data: []byte(body)}, "002_other.down.sql": {Data: []byte("-- irreversible")}}}
			if got := irreversible(t, source, 1); got != (body[0:2] == "--") {
				t.Fatalf("%s: %t", prefix, got)
			}
		}
	}
}
