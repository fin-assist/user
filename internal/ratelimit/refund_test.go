package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type refundStore struct {
	counterFunc
	calls int
	err   error
}

func (s *refundStore) Refund(context.Context, []Window) error { s.calls++; return s.err }

func TestReservationRefundSnapshotAndMetrics(t *testing.T) {
	s := &refundStore{counterFunc: func(_ context.Context, w []Window) ([]int, error) { return make([]int, len(w)), nil }}
	l, err := New(Defaults(), s, time.Now, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	d, r, err := l.Check(t.Context(), Start, "+79001234567", "192.0.2.1")
	if err != nil || !d.Allowed {
		t.Fatal(d, err)
	}
	w := r.ReservedWindows()
	if len(w) != 4 {
		t.Fatal(len(w))
	}
	w[0].Subject = "changed"
	if r.ReservedWindows()[0].Subject == "changed" {
		t.Fatal("shared private snapshot")
	}
	w = r.ReservedWindows()
	commit, err := l.Refund(t.Context(), w)
	if err != nil || s.calls != 1 {
		t.Fatal(err, s.calls)
	}
	if n := testutil.ToFloat64(l.refunded.WithLabelValues(string(w[0].Scope))); n != 0 {
		t.Fatal("refund observed before commit")
	}
	commit()
	commit()
	if n := testutil.ToFloat64(l.refunded.WithLabelValues(string(w[0].Scope))); n != 1 {
		t.Fatal(n)
	}
	s.err = errors.New("storage")
	if commit, err = l.Refund(t.Context(), w); err == nil || commit != nil {
		t.Fatal("failed refund has receipt")
	}
	if _, err = l.Refund(t.Context(), w[:3]); err == nil {
		t.Fatal("invalid windows")
	}
	w[0].Scope = WatchIP
	if _, err = l.Refund(t.Context(), w); err == nil {
		t.Fatal("foreign scope")
	}
	if (Receipt{}).ReservedWindows() != nil {
		t.Fatal("empty receipt")
	}
	other, err := New(Defaults(), counterFunc(func(_ context.Context, w []Window) ([]int, error) { return make([]int, len(w)), nil }), time.Now, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.Refund(t.Context(), r.ReservedWindows()); err == nil {
		t.Fatal("non-refunding store")
	}
}
