package ratelimit

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type counterFunc func(context.Context, []Window) ([]int, error)

func (f counterFunc) Lock(ctx context.Context, w []Window) ([]int, error) { return f(ctx, w) }
func (f counterFunc) Increment(context.Context, []Window) error           { return nil }

type incrementFailure struct{ counterFunc }

func (f incrementFailure) Increment(context.Context, []Window) error {
	return errors.New("increment failed")
}

func TestUnitIncrementFailureAndCorruptCount(t *testing.T) {
	for _, store := range []CounterStore{
		incrementFailure{func(context.Context, []Window) ([]int, error) { return []int{0}, nil }},
		counterFunc(func(context.Context, []Window) ([]int, error) { return []int{-1}, nil }),
	} {
		l, err := New(Defaults(), store, time.Now, prometheus.NewRegistry())
		if err != nil {
			t.Fatal(err)
		}
		if _, receipt, err := l.Check(t.Context(), Watch, "", "192.0.2.1"); err == nil || receipt.once != nil {
			t.Fatalf("failed check produced receipt: %v", err)
		}
		if got := testutil.ToFloat64(l.consumed.WithLabelValues(string(WatchIP))); got != 0 {
			t.Fatal(got)
		}
	}
}

func TestUnitOperationsWindowsAndPrivateSubjects(t *testing.T) {
	now := time.Date(2026, 12, 31, 23, 59, 59, 0, time.FixedZone("device", 3*3600))
	for _, tt := range []struct {
		op     Operation
		scopes []Scope
	}{
		{Start, []Scope{VerificationPhoneShort, VerificationPhoneLong, VerificationIPShort, VerificationIPLong}},
		{Watch, []Scope{WatchIP}}, {Register, []Scope{RegisterIP}}, {Refresh, []Scope{RefreshIP}},
	} {
		t.Run(string(tt.scopes[0]), func(t *testing.T) {
			store := counterFunc(func(_ context.Context, w []Window) ([]int, error) {
				if len(w) != len(tt.scopes) {
					t.Fatalf("windows: %v", w)
				}
				counts := make([]int, len(w))
				seen := map[Scope]bool{}
				for i, v := range w {
					seen[v.Scope] = true
					rule := Defaults()[v.Scope]
					wantSubject := "192.0.2.1"
					if v.Scope == VerificationPhoneShort || v.Scope == VerificationPhoneLong {
						wantSubject = "+79001234567"
					}
					if v.Subject != wantSubject || v.Limit != rule.Limit || !v.Start.Equal(now.UTC().Truncate(rule.Window)) || !v.End.Equal(v.Start.Add(rule.Window)) || v.Start.Location() != time.UTC {
						t.Fatalf("window: %+v", v)
					}
					if i > 0 && w[i-1].Scope >= v.Scope {
						t.Fatal("lock order")
					}
					counts[i] = 1
				}
				for _, s := range tt.scopes {
					if !seen[s] {
						t.Fatalf("missing %s", s)
					}
				}
				return counts, nil
			})
			l, err := New(Defaults(), store, func() time.Time { return now }, prometheus.NewRegistry())
			if err != nil {
				t.Fatal(err)
			}
			if d, _, err := l.Check(t.Context(), tt.op, "+79001234567", "::ffff:192.0.2.1"); err != nil || !d.Allowed {
				t.Fatalf("%+v %v", d, err)
			}
		})
	}
}

func TestUnitMetricsRequireCommitAndReceiptIsIdempotent(t *testing.T) {
	reg := prometheus.NewRegistry()
	denied := false
	reset := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	l, err := New(Defaults(), counterFunc(func(context.Context, []Window) ([]int, error) {
		hits := 29
		if denied {
			hits = 60
		}
		return []int{hits}, nil
	}), func() time.Time { return reset.Add(-time.Second) }, reg)
	if err != nil {
		t.Fatal(err)
	}
	_, discarded, err := l.Check(t.Context(), Watch, "", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	_ = discarded // a transaction rollback never calls Committed
	if got := testutil.ToFloat64(l.consumed.WithLabelValues(string(WatchIP))); got != 0 {
		t.Fatal(got)
	}
	_, r, err := l.Check(t.Context(), Watch, "", "192.0.2.1")
	if err != nil {
		t.Fatal(err)
	}
	copyOfReceipt := r
	r.Committed()
	copyOfReceipt.Committed()
	Receipt{}.Committed()
	if got := testutil.ToFloat64(l.consumed.WithLabelValues(string(WatchIP))); got != 1 {
		t.Fatal(got)
	}
	denied = true
	d, r, err := l.Check(t.Context(), Watch, "", "192.0.2.1")
	if err != nil || d.Allowed || !d.RetryAt.Equal(reset) {
		t.Fatalf("%+v %v", d, err)
	}
	r.Committed()
	if got := testutil.ToFloat64(l.consumed.WithLabelValues(string(WatchIP))); got != 1 {
		t.Fatal(got)
	}
	if got := testutil.ToFloat64(l.checks.WithLabelValues(string(WatchIP), "allowed")); got != 2 {
		t.Fatal(got)
	}
	if got := testutil.ToFloat64(l.checks.WithLabelValues(string(WatchIP), "denied")); got != 1 {
		t.Fatal(got)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		for _, m := range f.Metric {
			for _, label := range m.Label {
				if label.GetName() != "scope" && label.GetName() != "result" {
					t.Fatalf("unbounded label: %v", label)
				}
			}
			if f.GetName() == "finassist_rate_limit_window_utilization_ratio" && (m.Histogram.GetSampleCount() != 2 || m.Histogram.GetSampleSum() != 1.5) {
				t.Fatalf("rollback inflated observations: %v", m)
			}
		}
	}
}

func TestUnitInvalidSubjectsAndCounterErrors(t *testing.T) {
	calls := 0
	broken := false
	l, err := New(Defaults(), counterFunc(func(ctx context.Context, _ []Window) ([]int, error) {
		calls++
		if broken {
			return nil, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("storage unavailable")
	}), time.Now, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		op        Operation
		phone, ip string
	}{{0, "", "192.0.2.1"}, {Watch, "", ""}, {Watch, "", "secret"}, {Watch, "", "fe80::1%eth0"}, {Start, "", "192.0.2.1"}, {Start, "+7900bad4567", "192.0.2.1"}, {Start, "+790012345678", "192.0.2.1"}} {
		if _, _, err := l.Check(t.Context(), tt.op, tt.phone, tt.ip); err == nil || strings.Contains(err.Error(), tt.ip) && tt.ip != "" {
			t.Fatalf("invalid input/error: %+v %v", tt, err)
		}
	}
	if calls != 0 {
		t.Fatal("invalid subject reached storage")
	}
	if _, _, err := l.Check(t.Context(), Watch, "", "192.0.2.1"); err == nil {
		t.Fatal("storage error hidden")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := l.Check(ctx, Watch, "", "192.0.2.1"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	broken = true
	if _, _, err := l.Check(t.Context(), Watch, "", "192.0.2.1"); err == nil {
		t.Fatal("missing count accepted")
	}
	if got := testutil.ToFloat64(l.checks.WithLabelValues(string(WatchIP), "error")); got != 3 {
		t.Fatal(got)
	}
}

func TestUnitRulesAndRegistration(t *testing.T) {
	store := counterFunc(func(context.Context, []Window) ([]int, error) { return nil, nil })
	for _, mutate := range []func(Rules){func(r Rules) { delete(r, WatchIP) }, func(r Rules) { r["phone-as-label"] = Rule{1, time.Minute} }, func(r Rules) { r[WatchIP] = Rule{0, time.Minute} }, func(r Rules) { r[WatchIP] = Rule{math.MaxInt32 + 1, time.Minute} }, func(r Rules) { r[WatchIP] = Rule{1, time.Millisecond} }, func(r Rules) { r[WatchIP] = Rule{1, 25 * time.Hour} }, func(r Rules) { r[WatchIP] = Rule{1, time.Second + time.Nanosecond} }} {
		r := Defaults()
		mutate(r)
		if _, err := New(r, store, time.Now, prometheus.NewRegistry()); err == nil {
			t.Fatal("invalid rules accepted")
		}
	}
	for _, tt := range []struct {
		store CounterStore
		now   func() time.Time
		reg   prometheus.Registerer
	}{{nil, time.Now, prometheus.NewRegistry()}, {store, nil, prometheus.NewRegistry()}, {store, time.Now, nil}} {
		if _, err := New(Defaults(), tt.store, tt.now, tt.reg); err == nil {
			t.Fatal("missing dependency")
		}
	}
	rules := Defaults()
	reg := prometheus.NewRegistry()
	l, err := New(rules, store, time.Now, reg)
	if err != nil {
		t.Fatal(err)
	}
	rules[WatchIP] = Rule{1, time.Second}
	if l.rules[WatchIP].Limit != 60 {
		t.Fatal("rules not copied")
	}
	if _, err := New(Defaults(), store, time.Now, reg); err == nil {
		t.Fatal("duplicate collectors accepted")
	}
	// A late registration failure rolls back only collectors registered by this call.
	late := prometheus.NewRegistry()
	late.MustRegister(prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "finassist_rate_limit_window_seconds", Help: "Configured UTC fixed-window duration in seconds."}, []string{"scope"}))
	if _, err := New(Defaults(), store, time.Now, late); err == nil {
		t.Fatal("late collision accepted")
	}
	late.MustRegister(prometheus.NewCounterVec(prometheus.CounterOpts{Name: "finassist_rate_limit_checks_total", Help: "Rate-limit checks before transaction completion, by fixed scope and result."}, []string{"scope", "result"}))
}
