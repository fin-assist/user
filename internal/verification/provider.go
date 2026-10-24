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
)

// Attempt is durable identity supplied only by the service. Recover must examine
// provider state for this attempt; knowing Phone alone never authorizes adoption.
type Attempt struct {
	ID                      int32
	Phone, Provider, CallID string
	CreatedAt, ExpiresAt    time.Time
}

// Call contains a checked provider response. StartedAt is the conservative local
// start of that call, not the time a recovery response happened to arrive.
// A recovered replacement has a new ID and a new StartedAt. Its predecessor's
// confirmation is never transferred. Numbers are fresh for this call only.
type Call struct {
	ID, Phone string
	StartedAt time.Time
	Numbers   []string
}

// Provider is implemented by FIN-139 and FIN-122. All methods run outside a DB
// transaction and must obey context cancellation. Create is called once for a
// committed new attempt. Any uncertain result, including a crash before the
// response is saved, uses Recover thereafter.
//
// Recover checks status and call ID before repeating a provider operation. For
// Zvonok it also handles the terminal-state race explicitly: a new call gets a
// fresh lifetime and no proof from its predecessor. It may return an error when
// it cannot establish safe ownership. Blind confirm retries are forbidden.
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

func validateCall(a Attempt, c Call, now time.Time) error {
	if c.Phone != a.Phone || c.ID == "" || len(c.ID) > 128 ||
		c.StartedAt.Before(a.CreatedAt) || c.StartedAt.After(now) ||
		!c.StartedAt.Add(RequestTTL).After(now) || len(c.Numbers) == 0 || len(c.Numbers) > 1000 {
		return ErrProviderUnavailable
	}
	if a.CallID != "" && a.CallID == c.ID && !a.CreatedAt.Equal(c.StartedAt) {
		return ErrProviderUnavailable
	}
	for _, number := range c.Numbers {
		if !callbackNumber(number) {
			return ErrProviderUnavailable
		}
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
