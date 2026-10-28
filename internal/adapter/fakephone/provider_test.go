package fakephone

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/fin-assist/user/internal/verification"
)

func TestUnitDurableAttemptRecoveryAndPoolIsolation(t *testing.T) {
	now := time.Date(2032, time.February, 29, 23, 59, 0, 0, time.UTC)
	p := New(func() time.Time { return now })
	a := verification.Attempt{ID: 42, Phone: "+70000001234", Provider: "fake", CreatedAt: now, ExpiresAt: now.Add(verification.RequestTTL)}
	first, err := p.Create(t.Context(), a)
	if err != nil || first.ID != "fake:42" || first.Phone != a.Phone ||
		!first.StartedAt.Equal(a.CreatedAt) || !reflect.DeepEqual(first.Numbers, []string{CallbackNumberOne, CallbackNumberTwo}) {
		t.Fatal(first, err)
	}
	p = New(func() time.Time { return now }) // No process-local mapping or timer survives a restart.
	recovered, err := p.Recover(t.Context(), a)
	if err != nil || !reflect.DeepEqual(first, recovered) {
		t.Fatal(recovered, err)
	}
	first.Numbers[0] = "modified"
	recovered, err = p.Recover(t.Context(), a)
	if err != nil || recovered.Numbers[0] != CallbackNumberOne {
		t.Fatal("pool aliases mutable state", recovered, err)
	}
	a.ID++
	next, err := p.Create(t.Context(), a)
	if err != nil || next.ID == recovered.ID {
		t.Fatal("distinct requests share a call", next, err)
	}
}

func TestUnitRejectsNonSyntheticAndInvalidAttemptsWithoutNetwork(t *testing.T) {
	now := time.Now().UTC()
	p := New(time.Now)
	p.now = func() time.Time { return now }
	valid := verification.Attempt{ID: 1, Phone: "+70000000000", Provider: "fake", CreatedAt: now, ExpiresAt: now.Add(verification.RequestTTL)}
	for _, phone := range []string{"+70000009999", "+70000000001"} {
		a := valid
		a.Phone = phone
		if _, err := p.Create(t.Context(), a); err != nil {
			t.Fatal(phone, err)
		}
	}
	for _, mutate := range []func(*verification.Attempt){
		func(a *verification.Attempt) { a.Phone = "+79990001234" },
		func(a *verification.Attempt) { a.Phone = "+70000010000" },
		func(a *verification.Attempt) { a.Phone = "+7000000abcd" },
		func(a *verification.Attempt) { a.Phone = "+700000000000" },
		func(a *verification.Attempt) { a.ID = 0 },
		func(a *verification.Attempt) { a.Provider = "zvonok" },
		func(a *verification.Attempt) { a.CreatedAt = now.Add(time.Second) },
		func(a *verification.Attempt) { a.ExpiresAt = now },
		func(a *verification.Attempt) { a.CallID = "foreign" },
	} {
		a := valid
		mutate(&a)
		if _, err := p.Recover(t.Context(), a); !errors.Is(err, verification.ErrProviderRejected) {
			t.Fatal("invalid attempt accepted", a, err)
		}
	}
	valid.CallID = "fake:1"
	if _, err := p.Recover(t.Context(), valid); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.Create(ctx, valid); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
