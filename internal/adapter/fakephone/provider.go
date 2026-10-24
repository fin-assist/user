// Package fakephone supplies the opt-in, network-free development phone backend.
package fakephone

import (
	"context"
	"strconv"
	"time"

	"github.com/fin-assist/user/internal/verification"
)

const (
	// CallbackNumberOne is a synthetic display value, never dialled by this backend.
	CallbackNumberOne = verification.TestPhonePrefix + "0001"
	// CallbackNumberTwo supplies a distinct synthetic alternative to the common selector.
	CallbackNumberTwo = verification.TestPhonePrefix + "0002"
)

// Provider reconstructs calls from the durable attempt, without process-local state.
type Provider struct{ now func() time.Time }

// New returns the stateless development backend. Runtime assembly forbids prod.
func New() *Provider { return &Provider{now: time.Now} }

// Name is the persistent identity used by the common creation/confirmation paths.
func (*Provider) Name() string { return "fake" }

// Create accepts only the synthetic namespace. A definite refusal has no external effect.
func (p *Provider) Create(ctx context.Context, a verification.Attempt) (verification.Call, error) {
	if err := ctx.Err(); err != nil {
		return verification.Call{}, err
	}
	now := p.now()
	if a.ID < 1 || a.Provider != p.Name() || !verification.IsTestPhone(a.Phone) ||
		a.CreatedAt.After(now) || !a.ExpiresAt.After(now) ||
		!a.ExpiresAt.Equal(a.CreatedAt.Add(verification.RequestTTL)) {
		return verification.Call{}, verification.ErrProviderRejected
	}
	id := "fake:" + strconv.FormatInt(int64(a.ID), 10)
	if a.CallID != "" && a.CallID != id {
		return verification.Call{}, verification.ErrProviderRejected
	}
	return verification.Call{ID: id, Phone: a.Phone, StartedAt: a.CreatedAt,
		Numbers: []string{CallbackNumberOne, CallbackNumberTwo}}, nil
}

// Recover preserves identity, lifetime and pool after process restart or rollback.
func (p *Provider) Recover(ctx context.Context, a verification.Attempt) (verification.Call, error) {
	return p.Create(ctx, a)
}
