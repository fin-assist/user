package verification

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// ErrUnconfirmed means an authenticated lookup did not prove pincode_ok.
var ErrUnconfirmed = errors.New("verification: provider has not confirmed the call")

// StatusChecker is the optional provider fallback used only by pending Watches.
type StatusChecker interface {
	Check(context.Context, Request, bool) error
}

// Confirmation is the common atomic confirmation path, shared with all providers.
type Confirmation interface {
	Confirm(context.Context, Proof) (bool, error)
}

// Checker validates authenticated provider state before entering Confirmation.
// Its bounded per-process cache coalesces open Watches; the account-wide API gate
// remains the cross-replica capacity boundary, including postbacks.
type Checker struct {
	provider StatusProvider
	name     string
	repo     Repository
	confirm  Confirmation
	now      func() time.Time
	interval time.Duration
	capacity int
	mu       sync.Mutex
	next     map[int32]time.Time
	checks   *prometheus.CounterVec
}

// NewChecker accepts only assembly-owned dependencies and bounded polling settings.
func NewChecker(name string, provider StatusProvider, repo Repository, confirm Confirmation, interval time.Duration, capacity int, reg prometheus.Registerer) (*Checker, error) {
	if !providerName.MatchString(name) || provider == nil || repo == nil || confirm == nil || interval < time.Second || interval > time.Minute || capacity < 1 {
		return nil, errors.New("verification: invalid status checker")
	}
	c := &Checker{provider: provider, name: name, repo: repo, confirm: confirm, now: time.Now, interval: interval, capacity: capacity, next: map[int32]time.Time{}, checks: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "finassist_phone_provider_checks_total", Help: "Authenticated phone checks by fixed source and result; no private labels."}, []string{"source", "result"})}
	metrics := []prometheus.Collector{c.checks,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "finassist_phone_provider_poll_interval_seconds", Help: "Configured minimum interval between fallback checks of one request."}, func() float64 { return c.interval.Seconds() }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "finassist_phone_provider_poll_capacity", Help: "Configured maximum entries in the fallback coalescing map."}, func() float64 { return float64(c.capacity) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "finassist_phone_provider_poll_active", Help: "Requests currently coalesced until their next fallback interval."}, func() float64 {
			c.mu.Lock()
			defer c.mu.Unlock()
			var active float64
			for _, next := range c.next {
				if c.now().Before(next) {
					active++
				}
			}
			return active
		}),
	}
	for _, metric := range metrics {
		if err := reg.Register(metric); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// CheckCall resolves the local request before any provider call. Unknown IDs are
// never sent to the provider, and postback phone/status hints are never used.
func (c *Checker) CheckCall(ctx context.Context, id string) error {
	r, err := c.repo.ByCall(ctx, c.name, id)
	if err != nil {
		return err
	}
	return c.check(ctx, r, "postback")
}

// Check coalesces ordinary fallback checks. The expiry check always runs once per
// Watch so an exhausted cache cannot silently replace the final provider lookup.
func (c *Checker) Check(ctx context.Context, r Request, final bool) error {
	if r.Provider != c.name || r.CallID == "" || r.VerifiedAt != nil || r.Closed || r.Released {
		return nil
	}
	if !final {
		c.mu.Lock()
		now := c.now()
		if next, ok := c.next[r.ID]; ok && now.Before(next) {
			c.mu.Unlock()
			return nil
		}
		for id, next := range c.next {
			if !now.Before(next) {
				delete(c.next, id)
			}
		}
		if len(c.next) >= c.capacity {
			c.mu.Unlock()
			c.checks.WithLabelValues("fallback", "capacity").Inc()
			return nil
		}
		c.next[r.ID] = now.Add(c.interval)
		c.mu.Unlock()
	}
	return c.check(ctx, r, "fallback")
}

func (c *Checker) check(ctx context.Context, r Request, source string) (err error) {
	result := "unavailable"
	defer func() { c.checks.WithLabelValues(source, result).Inc() }()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	a := Attempt{ID: r.ID, Phone: r.Phone, Provider: r.Provider, CallID: r.CallID, Campaign: r.Campaign, FirstDispatchAt: r.FirstDispatchAt, DispatchNotAfter: r.DispatchNotAfter, CreatedAt: r.CreatedAt, ExpiresAt: r.ExpiresAt}
	p, err := c.provider.Status(ctx, a)
	if err != nil {
		if errors.Is(err, ErrProof) {
			result = "mismatch"
		}
		return err
	}
	if p.ID != r.CallID || p.Phone != r.Phone {
		result = "mismatch"
		return ErrProof
	}
	if p.State != Verified {
		result = "unconfirmed"
		return ErrUnconfirmed
	}
	changed, err := c.confirm.Confirm(ctx, Proof{Provider: c.name, CallID: p.ID, Phone: p.Phone})
	if err != nil {
		return err
	}
	result = "unchanged"
	if changed {
		result = "confirmed"
	}
	return nil
}
