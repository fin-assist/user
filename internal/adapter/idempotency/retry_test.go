package idempotency

import (
	"context"
	"errors"
	"testing"

	"github.com/fin-assist/contracts/gen/go/finerr"
	idem "github.com/fin-assist/go-idempotency"

	"github.com/fin-assist/user/internal/domain"
	"github.com/fin-assist/user/internal/verification"
)

type outcomeEngine struct {
	verification.StartEngine
	captured error
	failure  error
}

func (e *outcomeEngine) DoReserved(ctx context.Context, _ idem.Reservation, fn func(context.Context) (idem.Response, error)) (idem.Result, error) {
	r, err := fn(ctx)
	e.captured = err
	return idem.Result{Response: r, Err: err}, e.failure
}
func TestUnitStartAdviceIsMappedBeforeEncodingNotAfterCommit(t *testing.T) {
	for _, permission := range []bool{false, true} {
		commit := errors.New("unknown commit")
		engine := &outcomeEngine{failure: commit}
		_, err := (StartEngine{StartEngine: engine}).DoReserved(t.Context(), idem.Reservation{}, func(context.Context) (idem.Response, error) {
			return idem.Response{}, domain.WithRequestRetry(finerr.ConsentOutdated(), permission)
		})
		rest, ok := finerr.ToREST(engine.captured)
		if !ok || rest.RequestRetryable == nil || *rest.RequestRetryable != permission || !errors.Is(err, commit) {
			t.Fatal(rest, err)
		}
		if _, ok := finerr.ToREST(err); ok {
			t.Fatal("commit inherited callback advice")
		}
	}
}
