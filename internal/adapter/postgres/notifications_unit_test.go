package postgres

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func unitNotifications() *VerificationNotifications {
	root, cancel := context.WithCancel(context.Background())
	return &VerificationNotifications{max: 1, root: root, cancel: cancel, subscriptions: map[int32]map[*phoneSubscriber]struct{}{},
		active:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "unit_active", Help: "Active subscriptions."}),
		capacity:  prometheus.NewGauge(prometheus.GaugeOpts{Name: "unit_capacity", Help: "Subscription capacity."}),
		connected: prometheus.NewGauge(prometheus.GaugeOpts{Name: "unit_connected", Help: "Listener ready."}),
		rejected:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "unit_rejected", Help: "Refused subscriptions."}, []string{"reason"})}
}

func TestUnitNotificationSubscriptionsAreBoundedCoalescedAndClosedIdempotently(t *testing.T) {
	n := unitNotifications()
	defer n.Close()
	if _, err := n.Subscribe(1); err == nil {
		t.Fatal("unavailable listener admitted watch")
	}
	n.ready = true
	if _, err := n.Subscribe(0); err == nil {
		t.Fatal("invalid ID admitted")
	}
	sub, err := n.Subscribe(7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.Subscribe(8); err == nil {
		t.Fatal("capacity exceeded")
	}
	for _, payload := range []string{"", "invalid", "0", "2147483648", "8"} {
		n.notify(payload)
		select {
		case <-sub.Changed:
			t.Fatalf("foreign payload delivered: %s", payload)
		default:
		}
	}
	n.notify("7")
	n.notify("7")
	select {
	case <-sub.Changed:
	default:
		t.Fatal("committed notification lost")
	}
	select {
	case <-sub.Changed:
		t.Fatal("notifications were not coalesced")
	default:
	}
	if got := testutil.ToFloat64(n.active); got != 1 {
		t.Fatal("active gauge", got)
	}
	sub.Close()
	sub.Close()
	if got := testutil.ToFloat64(n.active); got != 0 || n.count != 0 || len(n.subscriptions) != 0 {
		t.Fatal("closed subscription retained", got, n.count, len(n.subscriptions))
	}
	// Releasing a subscription permits a later watcher of the same request.
	next, err := n.Subscribe(7)
	if err != nil {
		t.Fatal(err)
	}
	n.failSubscribers()
	select {
	case <-next.Done:
	default:
		t.Fatal("listener failure did not close subscribers")
	}
	if n.ready || n.count != 0 || testutil.ToFloat64(n.connected) != 0 {
		t.Fatal("failed listener still ready")
	}
	next.Close()
	n.Close()
	n.Close()
	if _, err := n.Subscribe(7); err == nil {
		t.Fatal("closed listener admitted watch")
	}
	if err := n.Run(t.Context()); err == nil {
		t.Fatal("closed worker restarted")
	}
}

func TestUnitNotificationConstructorRejectsInvalidConfigAndRollsBackMetricRegistration(t *testing.T) {
	cfg, err := pgx.ParseConfig("postgres://localhost/postgres")
	if err != nil {
		t.Fatal(err)
	}
	for _, max := range []int{0, 100001} {
		if _, err := NewVerificationNotifications(t.Context(), cfg, max, prometheus.NewRegistry()); err == nil {
			t.Fatal("invalid capacity accepted")
		}
	}
	if _, err := NewVerificationNotifications(t.Context(), nil, 1, prometheus.NewRegistry()); err == nil {
		t.Fatal("nil config accepted")
	}
	if _, err := NewVerificationNotifications(t.Context(), cfg, 1, nil); err == nil {
		t.Fatal("nil metrics accepted")
	}
	names := []string{"finassist_phone_watch_active_streams", "finassist_phone_watch_max_streams", "finassist_phone_watch_listener_ready", "finassist_phone_watch_rejected_total"}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			existing := prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: "Existing collector."})
			reg.MustRegister(existing)
			if _, err := NewVerificationNotifications(t.Context(), cfg, 1, reg); err == nil {
				t.Fatal("duplicate metric accepted")
			}
			families, err := reg.Gather()
			if err != nil || len(families) != 1 || families[0].GetName() != name {
				t.Fatalf("constructor leaked collectors: %v %v", families, err)
			}
		})
	}
}

func TestUnitCancelledNotificationStartupReleasesCollectorsAndWorkerState(t *testing.T) {
	cfg, err := pgx.ParseConfig("postgres://127.0.0.1:1/postgres")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	reg := prometheus.NewRegistry()
	if n, err := NewVerificationNotifications(ctx, cfg, 1, reg); err == nil || n != nil {
		t.Fatal("cancelled listener startup succeeded", n, err)
	}
	if families, err := reg.Gather(); err != nil || len(families) != 0 {
		t.Fatal("failed listener leaked registered collectors", families, err)
	}
	n := unitNotifications()
	n.cfg = cfg
	if err := n.Run(ctx); err == nil {
		t.Fatal("cancelled worker opened a listener")
	}
	if n.running || n.ready || n.conn != nil {
		t.Fatal("cancelled worker retained live state")
	}
	select {
	case <-n.stopped:
	default:
		t.Fatal("cancelled worker did not signal stop")
	}
	n.Close()
}
