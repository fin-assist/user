package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/verification"
)

func notificationBroker(max int) *VerificationNotifications {
	return &VerificationNotifications{max: max, ready: true, subscriptions: map[int32]map[*phoneSubscriber]struct{}{}, active: prometheus.NewGauge(prometheus.GaugeOpts{Name: "unit_active"}), connected: prometheus.NewGauge(prometheus.GaugeOpts{Name: "unit_ready"}), rejected: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "unit_rejected"}, []string{"reason"})}
}
func requirePhoneSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("phone signal missing")
	}
}
func noPhoneSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal("unexpected signal")
	default:
	}
}
func TestPhoneNotificationBoundedSubscriptionsAndCoalescing(t *testing.T) {
	n := notificationBroker(2)
	if _, err := n.Subscribe(0); err == nil {
		t.Fatal("invalid ID accepted")
	}
	first, err := n.Subscribe(7)
	if err != nil {
		t.Fatal(err)
	}
	second, err := n.Subscribe(8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = n.Subscribe(7); err == nil {
		t.Fatal("capacity ignored")
	}
	for _, payload := range []string{"", "0", "-1", "2147483648", "07", "private-token", " 7"} {
		n.notify(payload)
	}
	noPhoneSignal(t, first.Changed)
	for range 100 {
		n.notify("7")
	}
	requirePhoneSignal(t, first.Changed)
	noPhoneSignal(t, first.Changed)
	noPhoneSignal(t, second.Changed)
	first.Close()
	first.Close()
	requirePhoneSignal(t, first.Done)
	replacement, err := n.Subscribe(8)
	if err != nil {
		t.Fatal("capacity leaked", err)
	}
	n.notify("8")
	requirePhoneSignal(t, replacement.Changed)
	requirePhoneSignal(t, second.Changed)
	n.failSubscribers()
	requirePhoneSignal(t, replacement.Done)
	requirePhoneSignal(t, second.Done)
	replacement.Close()
	second.Close()
	if n.count != 0 || len(n.subscriptions) != 0 {
		t.Fatal("subscribers retained")
	}
	if _, err = n.Subscribe(8); err == nil {
		t.Fatal("disconnected listener accepted subscription")
	}
}
func TestPhoneNotificationRealPrimaryReconnectAndClose(t *testing.T) {
	db, _, _, _ := phoneState(t)
	reg := prometheus.NewRegistry()
	cfg := db.Primary().Config().ConnConfig.Copy()
	cfg.RuntimeParams["application_name"] = "test-phone-listener"
	n, err := NewVerificationNotifications(t.Context(), cfg, 2, reg)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	if _, err := NewVerificationNotifications(t.Context(), cfg, 2, reg); err == nil {
		t.Fatal("duplicate registry accepted")
	}
	for _, max := range []int{0, 100001} {
		if _, err := NewVerificationNotifications(t.Context(), cfg, max, prometheus.NewRegistry()); err == nil {
			t.Fatal("bad capacity")
		}
	}
	if _, err := NewVerificationNotifications(t.Context(), nil, 2, prometheus.NewRegistry()); err == nil {
		t.Fatal("no connection config")
	}
	first, _ := n.Subscribe(11)
	second, _ := n.Subscribe(11)
	if _, err := n.Subscribe(12); err == nil {
		t.Fatal("capacity ignored")
	}
	// The initial LISTEN is already established before Run, so a queued wakeup
	// arriving between subscription and worker startup is not lost.
	if _, err := db.Primary().Exec(t.Context(), `SELECT pg_notify('phone_verification','11')`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- n.Run(ctx) }()
	requirePhoneSignal(t, first.Changed)
	requirePhoneSignal(t, second.Changed)
	if err := n.Run(ctx); err == nil {
		t.Fatal("concurrent listener allowed")
	}
	n.mu.Lock()
	pid := n.conn.PgConn().PID()
	n.mu.Unlock()
	if _, err := db.Primary().Exec(t.Context(), `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatal(err)
	}
	requirePhoneSignal(t, first.Done)
	requirePhoneSignal(t, second.Done)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("disconnect became success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listener did not fail")
	}
	if _, err := n.Subscribe(11); err == nil {
		t.Fatal("disconnected listener accepted")
	}
	go func() { done <- n.Run(ctx) }()
	var resumed verification.Subscription
	waitPhoneCondition(t, func() bool { var err error; resumed, err = n.Subscribe(11); return err == nil })
	if _, err := db.Primary().Exec(t.Context(), `SELECT pg_notify('phone_verification','11')`); err != nil {
		t.Fatal(err)
	}
	requirePhoneSignal(t, resumed.Changed)
	n.Close()
	n.Close()
	requirePhoneSignal(t, resumed.Done)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listener not stopped")
	}
	if n.count != 0 {
		t.Fatal("close retained streams")
	}
	if err := n.Run(t.Context()); err == nil {
		t.Fatal("closed listener restarted")
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		for _, m := range f.Metric {
			for _, label := range m.Label {
				if label.GetName() != "reason" || (label.GetValue() != "capacity" && label.GetValue() != "not_ready") {
					t.Fatal("unbounded label")
				}
			}
			if f.GetName() == "finassist_phone_watch_active_streams" && m.GetGauge().GetValue() != 0 {
				t.Fatal("active gauge stale")
			}
			if f.GetName() == "finassist_phone_watch_listener_ready" && m.GetGauge().GetValue() != 0 {
				t.Fatal("ready gauge stale")
			}
		}
	}
}
func waitPhoneCondition(t *testing.T, check func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !check() {
		select {
		case <-ctx.Done():
			t.Fatal("phone condition not reached")
		case <-tick.C:
		}
	}
}

func TestPhoneNotificationConstructorRollsBackMetricsOnFailure(t *testing.T) {
	db, _, _, _ := phoneState(t)
	cfg := db.Primary().Config().ConnConfig.Copy()
	reg := prometheus.NewRegistry()
	reg.MustRegister(prometheus.NewGauge(prometheus.GaugeOpts{Name: "finassist_phone_watch_max_streams", Help: "Conflicting preexisting collector"}))
	if _, err := NewVerificationNotifications(t.Context(), cfg, 2, reg); err == nil {
		t.Fatal("conflicting capacity collector accepted")
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == "finassist_phone_watch_active_streams" {
			t.Fatal("partial metric registration retained")
		}
	}
	reg = prometheus.NewRegistry()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := NewVerificationNotifications(ctx, cfg, 2, reg); err == nil {
		t.Fatal("cancelled primary connection accepted")
	}
	families, err = reg.Gather()
	if err != nil || len(families) != 0 {
		t.Fatal("failed constructor retained metrics", err)
	}
}
