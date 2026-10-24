package verification

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type statusFunc func(context.Context, Attempt) (ProviderStatus, error)

func (f statusFunc) Status(ctx context.Context, a Attempt) (ProviderStatus, error) { return f(ctx, a) }

type confirmFunc func(context.Context, Proof) (bool, error)

func (f confirmFunc) Confirm(ctx context.Context, p Proof) (bool, error) { return f(ctx, p) }

func TestCheckedProofNeverTrustsHintsAndCoalescesPolling(t *testing.T) {
	f := fixture(t)
	f.r.r.Provider = "test"
	f.r.r.CallID = "call-1"
	var lookups, confirmations int
	p := statusFunc(func(_ context.Context, a Attempt) (ProviderStatus, error) {
		lookups++
		return ProviderStatus{ID: a.CallID, Phone: a.Phone, State: Verified}, nil
	})
	c, err := NewChecker("test", p, f.r, confirmFunc(func(_ context.Context, p Proof) (bool, error) {
		confirmations++
		if p != goodProof() {
			t.Fatal(p)
		}
		return true, nil
	}), 30*time.Second, 1, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Check(t.Context(), f.r.r, false); err != nil {
		t.Fatal(err)
	}
	if err = c.Check(t.Context(), f.r.r, false); err != nil || lookups != 1 || confirmations != 1 {
		t.Fatal(err, lookups, confirmations)
	}
	other := f.r.r
	other.ID++
	_ = c.Check(t.Context(), other, false)
	if lookups != 1 {
		t.Fatal("capacity")
	}
	if err = c.Check(t.Context(), f.r.r, true); err != nil || lookups != 2 {
		t.Fatal(err, lookups)
	}
	if err = c.CheckCall(t.Context(), "call-1"); err != nil || lookups != 3 {
		t.Fatal(err, lookups)
	}
	c.next[f.r.r.ID] = time.Now().Add(-time.Second)
	_ = c.Check(t.Context(), f.r.r, false)
	if lookups != 4 {
		t.Fatal("expired cache")
	}
	other.Provider = "foreign"
	_ = c.Check(t.Context(), other, false)
	if lookups != 4 {
		t.Fatal("wrong provider")
	}
}

func TestProviderCheckNegativeProofsAndDependencies(t *testing.T) {
	for _, tt := range []struct {
		name                          string
		state                         State
		phone, id                     string
		providerErr, confirmErr, want error
	}{
		{"pending", Pending, "+79001234567", "call-1", nil, nil, ErrUnconfirmed},
		{"expired", Expired, "+79001234567", "call-1", nil, nil, ErrUnconfirmed},
		{"other-phone", Verified, "+79001234568", "call-1", nil, nil, ErrProof},
		{"other-call", Verified, "+79001234567", "call-2", nil, nil, ErrProof},
		{"unavailable", Verified, "+79001234567", "call-1", errDependency, nil, errDependency},
		{"commit-failed", Verified, "+79001234567", "call-1", nil, errDependency, errDependency},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := fixture(t)
			c, err := NewChecker("test", statusFunc(func(context.Context, Attempt) (ProviderStatus, error) {
				return ProviderStatus{ID: tt.id, Phone: tt.phone, State: tt.state}, tt.providerErr
			}), f.r, confirmFunc(func(context.Context, Proof) (bool, error) { return false, tt.confirmErr }), time.Second, 2, prometheus.NewRegistry())
			if err != nil {
				t.Fatal(err)
			}
			if err = c.CheckCall(t.Context(), "call-1"); !errors.Is(err, tt.want) {
				t.Fatal(err)
			}
			f.r.failure = "missing"
			if err = c.CheckCall(t.Context(), "unknown"); !errors.Is(err, ErrMissing) {
				t.Fatal(err)
			}
		})
	}
	if _, err := NewChecker("invalid/name", nil, nil, nil, 0, 0, prometheus.NewRegistry()); err == nil {
		t.Fatal("invalid checker")
	}
}

type checkingFunc func(context.Context, Request, bool) error

func (f checkingFunc) Check(ctx context.Context, r Request, last bool) error { return f(ctx, r, last) }

func TestWatchFallbackConfirmsAndFinalCheckPreservesExpiry(t *testing.T) {
	for _, final := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "expired"}[final], func(t *testing.T) {
			f := fixture(t)
			f.s.resync = time.Millisecond
			if final {
				f.s.resync = time.Hour
			}
			f.r.r.ExpiresAt = f.now.Add(4 * time.Millisecond)
			calls := 0
			lastSeen := false
			f.s.SetStatusChecker(checkingFunc(func(_ context.Context, r Request, last bool) error {
				calls++
				lastSeen = lastSeen || last
				if final {
					f.now = f.r.r.ExpiresAt
					return errDependency
				}
				_, err := f.s.Confirm(t.Context(), goodProof())
				return err
			}))
			var states []State
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			err := f.s.Watch(ctx, "secret", func(o Outcome) error { states = append(states, o.State); return nil })
			if err != nil || len(states) != 2 || states[0] != Pending || calls == 0 {
				t.Fatal(states, calls, err)
			}
			if final {
				if states[1] != Expired {
					t.Fatal(states)
				}
				if !lastSeen {
					t.Fatal("missing final check")
				}
			} else if states[1] != Verified {
				t.Fatal(states)
			}
		})
	}
}

// NOTIFY already follows a committed proof. An unavailable provider must not
// delay delivery of that verified database result by another HTTP timeout.
func TestWatchNotifyDeliversCommittedResultWithoutProviderLookup(t *testing.T) {
	f := fixture(t)
	f.s.SetStatusChecker(checkingFunc(func(context.Context, Request, bool) error {
		t.Fatal("provider called for committed notification")
		return nil
	}))
	var states []State
	err := f.s.Watch(t.Context(), "secret", func(out Outcome) error {
		states = append(states, out.State)
		if out.State == Pending {
			if _, err := f.s.Confirm(t.Context(), goodProof()); err != nil {
				return err
			}
			f.n.changed <- struct{}{}
		}
		return nil
	})
	if err != nil || len(states) != 2 || states[1] != Verified {
		t.Fatal(states, err)
	}
}
