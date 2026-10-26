package retryinfo

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/fin-assist/contracts/gen/go/finerr"

	"github.com/fin-assist/user/internal/domain"
)

func TestUnitApplyPreservesAbsenceAndOtherDetails(t *testing.T) {
	if Apply(nil) != nil {
		t.Fatal("nil changed")
	}
	plain := errors.New("private SQL")
	if Apply(domain.WithRequestRetry(plain, true)) == nil {
		t.Fatal("lost failure")
	}
	legacy := finerr.RateLimited(time.Now().Add(time.Hour))
	original, _ := finerr.ToREST(legacy)
	for _, permission := range []bool{false, true} {
		got, ok := finerr.ToREST(Apply(fmt.Errorf("secret wrapper: %w", domain.WithRequestRetry(legacy, permission))))
		if !ok || got.RequestRetryable == nil || *got.RequestRetryable != permission || got.Metadata["retry_at"] != original.Metadata["retry_at"] || got.RetryAfter <= 0 {
			t.Fatal(got, ok)
		}
	}
	got, _ := finerr.ToREST(Apply(legacy))
	if got.RequestRetryable != nil {
		t.Fatal("invented advice")
	}
}
