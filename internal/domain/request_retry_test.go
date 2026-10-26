package domain

import (
	"errors"
	"fmt"
	"testing"
)

func TestUnitRequestRetryPreservesCauseAndExplicitDecision(t *testing.T) {
	if WithRequestRetry(nil, true) != nil {
		t.Fatal("nil became failure")
	}
	cause := errors.New("private cause")
	for _, allowed := range []bool{false, true} {
		err := fmt.Errorf("context: %w", WithRequestRetry(cause, allowed))
		var decision interface{ RequestRetryPermission() bool }
		if !errors.Is(err, cause) || !errors.As(err, &decision) || decision.RequestRetryPermission() != allowed {
			t.Fatal(err)
		}
		if WithRequestRetry(cause, allowed).Error() != cause.Error() {
			t.Fatal("changed cause")
		}
	}
}
