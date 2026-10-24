package main

import (
	"errors"
	"strings"
	"testing"
)

func TestUnitCommandDispatchValidatesArgumentsAndPreservesFailureType(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"unknown"}, {"serve", "extra"}} {
		err := run(t.Context(), args)
		var usageError usageError
		if !errors.As(err, &usageError) || !strings.Contains(err.Error(), "service serve") {
			t.Fatalf("usage contract: %v %v", args, err)
		}
	}
	t.Setenv("DB_PRIMARY_DSN", "")
	if err := run(t.Context(), []string{"serve"}); err == nil {
		t.Fatal("serve accepted missing configuration")
	}
	if err := run(t.Context(), []string{"migrate", "up"}); err == nil {
		t.Fatal("migrate accepted missing configuration")
	}
}
