// Package idempotency adapts usecase error advice before the library encodes an outcome.
package idempotency

import (
	"context"

	idem "github.com/fin-assist/go-idempotency"

	"github.com/fin-assist/user/internal/adapter/grpc/retryinfo"
	"github.com/fin-assist/user/internal/verification"
)

// StartEngine preserves phase advice in encoded phone outcomes without changing replay.
type StartEngine struct{ verification.StartEngine }

// DoReserved translates the callback's decision before persistence, never after a failed COMMIT.
func (e StartEngine) DoReserved(ctx context.Context, key idem.Reservation, effect func(context.Context) (idem.Response, error)) (idem.Result, error) {
	return e.StartEngine.DoReserved(ctx, key, func(ctx context.Context) (idem.Response, error) {
		response, err := effect(ctx)
		return response, retryinfo.Apply(err)
	})
}
