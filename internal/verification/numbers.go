package verification

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// MaxCallbackPoolNumbers bounds an untrusted provider pool before validation
// and selection. It is a defensive capacity, not a documented provider quota.
const MaxCallbackPoolNumbers = 1000

// maxCallbackMetricNumbers caps distinct public numbers retained as metric
// labels for a process lifetime. Additional requests contribute to other.
const maxCallbackMetricNumbers = 256

// callbackOutcomeWindow is the rolling diagnostic period of the 24h gauges.
const callbackOutcomeWindow = 24 * time.Hour

// PoolOutcome is an aggregate of public provider callback numbers, never caller phones.
type PoolOutcome struct {
	Number, State string
	Count         int64
}

// NumberRepository persists a fresh pool in the Start transaction and reads
// rolling outcomes from primary. Details are bounded; totals include every row.
type NumberRepository interface {
	SavePool(context.Context, int32, []string, string) error
	PoolOutcomes(context.Context, time.Time) ([]PoolOutcome, map[string]int64, error)
}

// Numbers selects uniformly from a call's fresh pool without shared random state.
type Numbers struct {
	repo     NumberRepository
	choose   func(int) (int, error)
	now      func() time.Time
	log      *slog.Logger
	mu       sync.Mutex
	known    map[string]bool
	outcomes *prometheus.GaugeVec
	overflow prometheus.Gauge
}

// NewNumbers caps public-number labels at 256 per process. Additional numbers
// contribute to "other" and the overflow gauge, with exact numbers in logs.
func NewNumbers(repo NumberRepository, log *slog.Logger, reg prometheus.Registerer) (*Numbers, error) {
	if repo == nil || log == nil {
		return nil, errors.New("verification: number repository and logger required")
	}
	n := &Numbers{repo: repo, log: log, now: time.Now, known: map[string]bool{}, choose: func(size int) (int, error) {
		v, err := rand.Int(rand.Reader, big.NewInt(int64(size)))
		if err != nil {
			return 0, err
		}
		return int(v.Int64()), nil
	}, outcomes: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "finassist_phone_callback_outcomes_24h", Help: "Requests created in the last 24 hours by public callback number and pending/confirmed/expired outcome; other is bounded-label overflow."}, []string{"callback_number", "outcome"}), overflow: prometheus.NewGauge(prometheus.GaugeOpts{Name: "finassist_phone_callback_overflow_requests_24h", Help: "Requests included under other because the public callback-number label limit was reached."})}
	if err := reg.Register(n.outcomes); err != nil {
		return nil, err
	}
	if err := reg.Register(n.overflow); err != nil {
		return nil, err
	}
	return n, nil
}

// Select runs after the request row is locked by Start. A rollback removes both
// pool/selection and the idempotent reply; a replay never selects again.
func (n *Numbers) Select(ctx context.Context, id int32, pool []string) (string, error) {
	if id < 1 || len(pool) < 1 || len(pool) > MaxCallbackPoolNumbers {
		return "", ErrProviderUnavailable
	}
	seen := map[string]bool{}
	for _, v := range pool {
		if !callbackNumber(v) || seen[v] {
			return "", ErrProviderUnavailable
		}
		seen[v] = true
	}
	i, err := n.choose(len(pool))
	if err != nil {
		return "", err
	}
	if i < 0 || i >= len(pool) {
		return "", ErrProviderUnavailable
	}
	selected := pool[i]
	if err = n.repo.SavePool(ctx, id, pool, selected); err != nil {
		return "", err
	}
	return selected, nil
}

// Report derives restart-safe rolling gauges from stored lifecycle state. Logs
// contain public provider numbers and aggregate counts, no request IDs or phones.
func (n *Numbers) Report(ctx context.Context) error {
	rows, totals, err := n.repo.PoolOutcomes(ctx, n.now().Add(-callbackOutcomeWindow))
	if err != nil {
		return err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.outcomes.Reset()
	remaining := map[string]int64{"pending": totals["pending"], "confirmed": totals["confirmed"], "expired": totals["expired"]}
	for _, r := range rows {
		n.log.InfoContext(ctx, "phone callback outcome (rolling 24h)", "callback_number", r.Number, "outcome", r.State, "requests", r.Count)
		if !n.known[r.Number] && len(n.known) < maxCallbackMetricNumbers {
			n.known[r.Number] = true
		}
		if n.known[r.Number] {
			n.outcomes.WithLabelValues(r.Number, r.State).Set(float64(r.Count))
			remaining[r.State] -= r.Count
		}
	}
	var overflow int64
	for state, count := range remaining {
		n.outcomes.WithLabelValues("other", state).Set(float64(count))
		overflow += count
	}
	n.overflow.Set(float64(overflow))
	return nil
}
