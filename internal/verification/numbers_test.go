package verification

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type numberStore struct {
	pool     []string
	selected string
	rows     []PoolOutcome
	totals   map[string]int64
	err      error
}

func (s *numberStore) SavePool(_ context.Context, _ int32, p []string, n string) error {
	s.pool = append([]string(nil), p...)
	s.selected = n
	return s.err
}
func (s *numberStore) PoolOutcomes(context.Context, time.Time) ([]PoolOutcome, map[string]int64, error) {
	return s.rows, s.totals, s.err
}

func TestNumbersPersistOnlyFreshPoolAndHandleFailures(t *testing.T) {
	s := &numberStore{}
	n, err := NewNumbers(s, slog.New(slog.DiscardHandler), prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	pool := []string{"+74951234567", "+74957654321"}
	for i := range pool {
		n.choose = func(size int) (int, error) {
			if size != 2 {
				t.Fatal(size)
			}
			return i, nil
		}
		got, err := n.Select(t.Context(), 1, pool)
		if err != nil || got != pool[i] || s.selected != got || len(s.pool) != 2 {
			t.Fatal(got, err)
		}
	}
	for _, bad := range [][]string{nil, {"bad"}, {pool[0], pool[0]}, make([]string, 1001)} {
		if _, err := n.Select(t.Context(), 1, bad); !errors.Is(err, ErrProviderUnavailable) {
			t.Fatal("bad pool", err)
		}
	}
	if _, err := n.Select(t.Context(), 0, pool); err == nil {
		t.Fatal("zero id")
	}
	n.choose = func(int) (int, error) { return 0, errDependency }
	if _, err := n.Select(t.Context(), 1, pool); !errors.Is(err, errDependency) {
		t.Fatal(err)
	}
	n.choose = func(int) (int, error) { return 2, nil }
	if _, err := n.Select(t.Context(), 1, pool); err == nil {
		t.Fatal("out of range")
	}
	n.choose = func(int) (int, error) { return 0, nil }
	s.err = errDependency
	if _, err := n.Select(t.Context(), 1, pool); !errors.Is(err, errDependency) {
		t.Fatal(err)
	}
}

func TestNumberOutcomesAreRestartSafeAndLabelsBounded(t *testing.T) {
	s := &numberStore{totals: map[string]int64{"confirmed": 300, "expired": 1}}
	for i := range 300 {
		s.rows = append(s.rows, PoolOutcome{Number: fmt.Sprintf("+7495%07d", i), State: "confirmed", Count: 1})
	}
	n, err := NewNumbers(s, slog.New(slog.DiscardHandler), prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if err = n.Report(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(n.known) != 256 || testutil.ToFloat64(n.overflow) != 45 || testutil.ToFloat64(n.outcomes.WithLabelValues("other", "confirmed")) != 44 {
		t.Fatal(len(n.known), testutil.ToFloat64(n.overflow))
	}
	s.rows = nil
	s.totals = map[string]int64{}
	if err = n.Report(t.Context()); err != nil || testutil.ToFloat64(n.overflow) != 0 {
		t.Fatal(err)
	}
	s.err = errDependency
	if err = n.Report(t.Context()); !errors.Is(err, errDependency) {
		t.Fatal(err)
	}
	if _, err = NewNumbers(nil, slog.New(slog.DiscardHandler), prometheus.NewRegistry()); err == nil {
		t.Fatal("nil repository")
	}
}
