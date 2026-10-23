// Package ratelimit enforces the fixed windows of the authentication methods.
// The service owns the transaction and records consumption only after its commit.
package ratelimit

import (
	"cmp"
	"context"
	"errors"
	"math"
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Scope is a fixed policy name, never a value taken from a request.
type Scope string

const (
	VerificationPhoneShort Scope = "verification_phone_short" // per phone, hourly by default
	VerificationPhoneLong  Scope = "verification_phone_long"  // per phone, daily by default
	VerificationIPShort    Scope = "verification_ip_short"    // per IP, minutely by default
	VerificationIPLong     Scope = "verification_ip_long"     // per IP, hourly by default
	WatchIP                Scope = "watch_ip"                 // connection attempts, including invalid tokens
	RegisterIP             Scope = "register_ip"              // requests, including invalid tokens
	RefreshIP              Scope = "refresh_ip"               // requests, including invalid tokens
)

// Rule sets a positive limit and a UTC fixed-window length.
type Rule struct {
	Limit  int
	Window time.Duration
}

// Rules contains exactly the seven authentication scopes.
type Rules map[Scope]Rule

// Defaults returns an independent copy of the policies agreed in FIN-219.
func Defaults() Rules {
	return Rules{
		VerificationPhoneShort: {3, time.Hour}, VerificationPhoneLong: {10, 24 * time.Hour},
		VerificationIPShort: {30, time.Minute}, VerificationIPLong: {300, time.Hour},
		WatchIP: {60, time.Minute}, RegisterIP: {30, time.Minute}, RefreshIP: {120, time.Minute},
	}
}

var scopes = []Scope{VerificationPhoneShort, VerificationPhoneLong, VerificationIPShort, VerificationIPLong, WatchIP, RegisterIP, RefreshIP}

// Validate rejects missing/unknown scopes, overflow and subsecond or unbounded windows.
func (rules Rules) Validate() error {
	if len(rules) != len(scopes) {
		return errors.New("rate limits: exactly seven authentication scopes required")
	}
	for _, scope := range scopes {
		r, ok := rules[scope]
		if !ok || r.Limit < 1 || r.Limit > math.MaxInt32 || r.Window < time.Second || r.Window > 24*time.Hour || r.Window%time.Second != 0 {
			return errors.New("rate limits: invalid rule for " + string(scope))
		}
	}
	return nil
}

// Operation selects the fixed set of checks for an authentication method.
type Operation uint8

const (
	Start    Operation = iota + 1 // phone and IP, checked before the provider
	Watch                         // IP, checked before the verification token
	Register                      // IP, checked after idempotency and before the registration token
	Refresh                       // IP, checked after idempotency and before the refresh token
)

// Window is one counter key. Subject is private input, never a metric label or error.
type Window struct {
	Scope      Scope
	Subject    string
	Start, End time.Time
	Limit      int
}

// Decision describes one atomic attempt: all windows increment, or none do.
type Decision struct {
	Allowed bool
	RetryAt time.Time // latest end among the exhausted windows
}

// CounterStore locks and updates ordered windows in the caller's primary transaction.
// Lock returns current counts in the same order, without consuming an attempt.
// The limiter decides whether to Increment. An error requires transaction rollback.
type CounterStore interface {
	Lock(context.Context, []Window) ([]int, error)
	Increment(context.Context, []Window) error
}

// Limiter is immutable apart from its concurrency-safe metric collectors.
type Limiter struct {
	rules            Rules
	store            CounterStore
	now              func() time.Time
	checks, consumed *prometheus.CounterVec
	utilization      *prometheus.HistogramVec
}

// New validates and copies rules, then registers metrics in the supplied registry.
func New(rules Rules, store CounterStore, now func() time.Time, reg prometheus.Registerer) (*Limiter, error) {
	if err := rules.Validate(); err != nil {
		return nil, err
	}
	if store == nil || now == nil || reg == nil {
		return nil, errors.New("rate limits: store, clock and registry required")
	}
	l := &Limiter{rules: make(Rules, len(rules)), store: store, now: now,
		checks:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "finassist_rate_limit_checks_total", Help: "Rate-limit checks before transaction completion, by fixed scope and result."}, []string{"scope", "result"}),
		consumed:    prometheus.NewCounterVec(prometheus.CounterOpts{Name: "finassist_rate_limit_consumed_total", Help: "Committed attempts consumed by fixed scope; excludes replays and rollbacks."}, []string{"scope"}),
		utilization: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "finassist_rate_limit_window_utilization_ratio", Help: "Window fill observed after committed attempts or committed denials, without subject labels.", Buckets: []float64{0, .25, .5, .75, .9, 1}}, []string{"scope"}),
	}
	limit := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "finassist_rate_limit_configured_limit", Help: "Configured maximum attempts in a fixed window."}, []string{"scope"})
	window := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "finassist_rate_limit_window_seconds", Help: "Configured UTC fixed-window duration in seconds."}, []string{"scope"})
	collectors := []prometheus.Collector{l.checks, l.consumed, l.utilization, limit, window}
	for i, c := range collectors {
		if err := reg.Register(c); err != nil {
			for _, registered := range collectors[:i] {
				reg.Unregister(registered)
			}
			return nil, err
		}
	}
	for _, scope := range scopes {
		r := rules[scope]
		l.rules[scope] = r
		limit.WithLabelValues(string(scope)).Set(float64(r.Limit))
		window.WithLabelValues(string(scope)).Set(r.Window.Seconds())
		l.consumed.WithLabelValues(string(scope))
		for _, result := range []string{"allowed", "denied", "error"} {
			l.checks.WithLabelValues(string(scope), result)
		}
	}
	return l, nil
}

type sample struct {
	scope       Scope
	hits, limit int
}

// Receipt contains no subject data. Call Committed once the enclosing transaction
// commits; discard it on rollback. Copies share the same once guard.
type Receipt struct {
	once    *sync.Once
	limiter *Limiter
	samples []sample
	allowed bool
}

// Committed records consumption and observed utilization at most once. In particular,
// checking a limit inside an idempotency callback does not count a rolled-back effect.
func (r Receipt) Committed() {
	if r.once == nil {
		return
	}
	r.once.Do(func() {
		for _, s := range r.samples {
			if r.allowed {
				r.limiter.consumed.WithLabelValues(string(s.scope)).Inc()
			}
			r.limiter.utilization.WithLabelValues(string(s.scope)).Observe(float64(s.hits) / float64(s.limit))
		}
	})
}

// Check validates the subject and checks all windows in the caller's transaction.
// It must run only after idempotency has excluded a replay/conflict. A denial is a
// decision, not a transaction error: the service chooses whether to commit or roll
// back, then constructs finerr.RateLimited(RetryAt) outside the transaction.
// The IP must come from verified bank-bff metadata, never from a message field.
func (l *Limiter) Check(ctx context.Context, op Operation, phone, ip string) (Decision, Receipt, error) {
	selected, err := operationScopes(op)
	if err != nil {
		return Decision{}, Receipt{}, err
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil || addr.Zone() != "" {
		return Decision{}, Receipt{}, errors.New("rate limits: trusted client IP required")
	}
	ip = addr.Unmap().String()
	if op == Start && !validPhone(phone) {
		return Decision{}, Receipt{}, errors.New("rate limits: valid phone required")
	}
	now := l.now().UTC()
	windows := make([]Window, 0, len(selected))
	for _, scope := range selected {
		rule := l.rules[scope]
		subject := ip
		if scope == VerificationPhoneShort || scope == VerificationPhoneLong {
			subject = phone
		}
		start := now.Truncate(rule.Window)
		windows = append(windows, Window{Scope: scope, Subject: subject, Start: start, End: start.Add(rule.Window), Limit: rule.Limit})
	}
	// All callers acquire overlapping rows in the same order.
	slices.SortFunc(windows, func(a, b Window) int { return cmp.Compare(a.Scope, b.Scope) })
	counts, err := l.store.Lock(ctx, windows)
	if err == nil && len(counts) != len(windows) {
		err = errors.New("rate limits: invalid counter result")
	}
	decision := Decision{Allowed: true}
	if err == nil {
		for i, w := range windows {
			if counts[i] < 0 {
				err = errors.New("rate limits: invalid stored counter")
				break
			}
			if counts[i] >= w.Limit {
				decision.Allowed = false
				if w.End.After(decision.RetryAt) {
					decision.RetryAt = w.End
				}
			}
		}
		if err == nil && decision.Allowed {
			err = l.store.Increment(ctx, windows)
			if err == nil {
				for i := range counts {
					counts[i]++
				}
			}
		}
	}
	if err != nil {
		for _, w := range windows {
			l.checks.WithLabelValues(string(w.Scope), "error").Inc()
		}
		return Decision{}, Receipt{}, err
	}
	r := Receipt{once: &sync.Once{}, limiter: l, allowed: decision.Allowed}
	for i, w := range windows {
		result := "allowed"
		if !decision.Allowed && counts[i] >= w.Limit {
			result = "denied"
		}
		l.checks.WithLabelValues(string(w.Scope), result).Inc()
		r.samples = append(r.samples, sample{w.Scope, counts[i], w.Limit})
	}
	return decision, r, nil
}

func operationScopes(op Operation) ([]Scope, error) {
	switch op {
	case Start:
		return []Scope{VerificationPhoneShort, VerificationPhoneLong, VerificationIPShort, VerificationIPLong}, nil
	case Watch:
		return []Scope{WatchIP}, nil
	case Register:
		return []Scope{RegisterIP}, nil
	case Refresh:
		return []Scope{RefreshIP}, nil
	default:
		return nil, errors.New("rate limits: unknown operation")
	}
}

func validPhone(s string) bool {
	if len(s) != 12 || s[:2] != "+7" {
		return false
	}
	for _, c := range s[2:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
