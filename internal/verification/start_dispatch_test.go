package verification

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fin-assist/user/internal/domain"
)

func TestUnitDispatchStorageUncertaintyKeepsOriginalKeyRecoverable(t *testing.T) {
	for _, markerCommitted := range []bool{false, true} {
		t.Run(map[bool]string{false: "marker-not-committed", true: "ambiguous-marker-commit"}[markerCommitted], func(t *testing.T) {
			f := startFixture(t)
			f.repo.dispatchErr = errDependency
			f.repo.dispatchMarkOnError = markerCommitted
			out, err := f.run(t.Context())
			var advice *domain.RequestRetryError
			if !errors.Is(err, errDependency) || errors.As(err, &advice) || out != (Started{}) || f.provider.creates != 0 || f.provider.recovers != 0 || f.repo.terminated != 0 {
				t.Fatal("storage failure was assigned workflow advice or dispatched", out, err)
			}
			f.repo.dispatchErr = nil
			out, err = f.run(t.Context())
			if err != nil || out.Token == "" || f.repo.claimed != 1 || f.counters.hits[0] != 1 {
				t.Fatal("same-key retry created new admission or lost the attempt", out, err)
			}
			if markerCommitted && (f.provider.creates != 0 || f.provider.recovers != 1) || !markerCommitted && (f.provider.creates != 1 || f.provider.recovers != 0) {
				t.Fatal("retry ignored durable marker", f.provider.creates, f.provider.recovers)
			}
		})
	}
}

func TestUnitDispatchMissingBeforeAndAtCapturedDeadlineIsTerminal(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-open", true: "deadline"}[expired], func(t *testing.T) {
			f := startFixture(t)
			f.repo.dispatchErr = ErrMissing
			if expired {
				f.repo.onDispatch = func() { f.now = f.now.Add(RequestTTL) }
			}
			out, err := f.run(t.Context())
			var advice *domain.RequestRetryError
			if !errors.As(err, &advice) || advice.RequestRetryPermission() || out != (Started{}) || f.provider.creates != 0 || f.provider.recovers != 0 {
				t.Fatal("missing attempt authorized dispatch/recovery", out, err)
			}
			if (f.repo.terminated == 1) != expired {
				t.Fatal("deadline termination was lost", f.repo.terminated)
			}
		})
	}
}

func TestUnitNetworkBoundaryPreservesCompletionTransactionBudget(t *testing.T) {
	f := startFixture(t)
	// A short library completion window exercises the real deadline boundary
	// without making the test wait seven seconds. The same three-second reserve
	// applies to this inherited deadline and the ordinary ten-second maximum.
	f.engine.completionTimeout = 3100 * time.Millisecond
	var networkDeadline time.Time
	f.provider.create = func(ctx context.Context, _ Attempt) (Call, error) {
		var ok bool
		networkDeadline, ok = ctx.Deadline()
		if !ok {
			t.Fatal("network has no deadline")
		}
		timer := time.NewTimer(max(0, time.Until(networkDeadline)-10*time.Millisecond))
		defer timer.Stop()
		select {
		case <-timer.C:
			return f.provider.call, nil
		case <-ctx.Done():
			t.Fatal("network did not retain its bounded interval", ctx.Err())
			return Call{}, ctx.Err()
		}
	}
	f.s.SetImmediateConfirmation(createdConfirmFunc(func(ctx context.Context, _ Proof, _ string) (bool, error) {
		deadline, ok := ctx.Deadline()
		if !ok || deadline.Sub(networkDeadline) < 3*time.Second || ctx.Err() != nil {
			t.Fatal("network consumed the completion reserve", deadline, networkDeadline, ctx.Err())
		}
		return true, nil
	}))
	out, err := f.run(t.Context())
	if err != nil || out.Token == "" || f.repo.completed != 1 || f.engine.replay == nil || f.provider.creates != 1 {
		t.Fatal("successful boundary response failed to publish", out, err)
	}
}
