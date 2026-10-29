package verification

import (
	"context"
	"errors"
	"time"
)

// RequestTTL is the provider's fixed lifetime, independent of result redelivery.
const RequestTTL = 5 * time.Minute

var (
	// ErrProviderRejected means the adapter knows no call was accepted. The local
	// reservation can be released and its rate-limit slots returned.
	ErrProviderRejected = errors.New("verification: provider rejected creation")
	// ErrProviderDuplicate means a foreign call already occupied the phone. It
	// must never be adopted or expose its numbers through a different client key.
	ErrProviderDuplicate = errors.New("verification: provider duplicate")
	// ErrProviderUnavailable includes unknown outcomes: they require recovery,
	// never an unconditional repeat of Create.
	ErrProviderUnavailable = errors.New("verification: provider unavailable")
	// ErrProviderTerminal is an authenticated terminal/expired call which cannot
	// authorize this attempt or manufacture a future busy fence.
	ErrProviderTerminal = errors.New("verification: provider terminal")
)

// ProviderDuplicateError carries only an authoritative active-call upper bound.
type ProviderDuplicateError struct{ BusyUntil time.Time }

func (e *ProviderDuplicateError) Error() string { return ErrProviderDuplicate.Error() }
func (e *ProviderDuplicateError) Unwrap() error { return ErrProviderDuplicate }

// Attempt is durable identity supplied only by the service. Recover must examine
// provider state for this attempt; knowing Phone alone never authorizes adoption.
type Attempt struct {
	ID                                int32
	Phone, Provider, CallID           string
	Campaign                          string
	CreatedAt, ExpiresAt              time.Time
	FirstDispatchAt, DispatchNotAfter time.Time
}

// Call contains an authenticated pending candidate. StartedAt is authoritative
// provider creation time, never a local fallback. Adapter clock bounds determine
// the conservative public deadline and independent latest known-busy deadline.
type Call struct {
	ID, Phone          string
	StartedAt          time.Time
	Numbers            []string
	Campaign           string
	ReceivedAt         time.Time
	ClockSkewTolerance time.Duration
	State              State
	Terminal           bool
}

// Provider is implemented by FIN-139 and FIN-122. All methods run outside a DB
// transaction and must obey context cancellation. Create is called once for a
// committed new attempt. Any uncertain result, including a crash before the
// response is saved, uses Recover thereafter.
//
// Recover is strictly read-only, including when no provider ID was persisted.
// Missing history is an unknown outcome and never authorizes another write.
type Provider interface {
	Name() string
	Create(context.Context, Attempt) (Call, error)
	Recover(context.Context, Attempt) (Call, error)
}

// ProviderStatus is the result of an authenticated provider lookup. Confirmation
// adapters must match ID/Phone and pass Verified through the common Confirm path.
type ProviderStatus struct {
	ID, Phone string
	State     State
}

// StatusProvider is the lookup surface for FIN-139 postbacks/fallback polling and
// FIN-122. Creation does not poll it; those adapters own when to check status.
type StatusProvider interface {
	Status(context.Context, Attempt) (ProviderStatus, error)
}

// NumberSelector runs inside the completion transaction. FIN-216 selects from
// exactly this response and persists the pool and selection for requestID. Concrete
// Zvonok assembly remains guarded from prod until FIN-81.
type NumberSelector interface {
	Select(context.Context, int32, []string) (string, error)
}

// PublicExpiry is the conservative verification deadline, captured on admission.
func (c Call) PublicExpiry() time.Time { return c.StartedAt.Add(RequestTTL - c.ClockSkewTolerance) }

// BusyUntil includes the adapter's finite clock tolerance in the opposite direction.
func (c Call) BusyUntil() time.Time { return c.StartedAt.Add(RequestTTL + c.ClockSkewTolerance) }
func validateCall(a Attempt, c Call, now time.Time) error {
	received := c.ReceivedAt
	if received.IsZero() {
		received = now
	}
	dispatch := a.FirstDispatchAt
	if dispatch.IsZero() {
		dispatch = a.CreatedAt
	}
	if c.Campaign != a.Campaign || (a.CallID != "" && (a.CallID != c.ID || !a.CreatedAt.Equal(c.StartedAt))) || c.Phone != a.Phone || c.ID == "" || len(c.ID) > 128 || c.ClockSkewTolerance < 0 || c.ClockSkewTolerance > time.Second || c.Terminal || c.State != Pending || c.StartedAt.Before(dispatch.Truncate(time.Millisecond).Add(-c.ClockSkewTolerance)) || c.StartedAt.After(received.Add(c.ClockSkewTolerance)) || !c.PublicExpiry().After(now) || !a.DispatchNotAfter.After(now) || len(c.Numbers) < 1 || len(c.Numbers) > 1000 {
		return ErrProviderUnavailable
	}
	seen := make(map[string]bool, len(c.Numbers))
	for _, n := range c.Numbers {
		if !callbackNumber(n) || seen[n] {
			return ErrProviderUnavailable
		}
		seen[n] = true
	}
	return nil
}

func callbackNumber(s string) bool {
	if len(s) < 3 || len(s) > 16 || s[0] != '+' || s[1] < '1' || s[1] > '9' {
		return false
	}
	for _, c := range s[2:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
