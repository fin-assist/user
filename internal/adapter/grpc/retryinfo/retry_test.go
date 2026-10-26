package retryinfo

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/user/internal/domain"
)

func TestUnitApplyPreservesAbsenceAndOtherDetails(t *testing.T) {
	if Apply(nil) != nil {
		t.Fatal("nil changed")
	}
	plain := errors.New("private SQL")
	unknown := domain.WithRequestRetry(plain, true)
	gotUnknown := Apply(unknown)
	if !errors.Is(gotUnknown, unknown) || !errors.Is(gotUnknown, plain) {
		t.Fatal("unknown failure chain changed", gotUnknown)
	}
	if rest, ok := finerr.ToREST(gotUnknown); ok && rest.RequestRetryable != nil {
		t.Fatal("unknown failure acquired advice", rest)
	}
	// This helper is not the public error sanitizer: preserving the unknown cause
	// lets the server interceptor log it and emit a generic INTERNAL at the wire.

	legacy := finerr.RateLimited(time.Now().Add(time.Hour))
	original, _ := finerr.ToREST(legacy)
	for _, permission := range []bool{false, true} {
		mapped := Apply(fmt.Errorf("secret wrapper: %w", domain.WithRequestRetry(legacy, permission)))
		if strings.Contains(status.Convert(mapped).Message(), "secret wrapper") || strings.Contains(status.Convert(mapped).Message(), "private SQL") {
			t.Fatal("wrapper diagnostics serialized", mapped)
		}
		got, ok := finerr.ToREST(mapped)
		if !ok || got.RequestRetryable == nil || *got.RequestRetryable != permission || got.Metadata["retry_at"] != original.Metadata["retry_at"] || got.RetryAfter <= 0 {
			t.Fatal(got, ok)
		}
	}
	got, _ := finerr.ToREST(Apply(legacy))
	if got.RequestRetryable != nil {
		t.Fatal("invented advice")
	}
}
