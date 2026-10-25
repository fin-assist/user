package testdb

import "testing"

func TestUnitExplicitServerDoesNotStartContainer(t *testing.T) {
	t.Setenv("TEST_DATABASE_URL", "explicit-test-server")
	t.Setenv("TEST_POSTGRES_IMAGE", "must-not-be-pulled")
	dsn, err := server()
	if err != nil || dsn != "explicit-test-server" {
		t.Fatalf("explicit server selection: %q %v", dsn, err)
	}
}
