package postgres

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/verification"
)

type phoneSubscriber struct {
	changed chan struct{}
	done    chan struct{}
	closed  bool
}

// VerificationNotifications shares one primary LISTEN connection per instance,
// not one pool connection per stream. Run belongs to the normal worker lifecycle.
type VerificationNotifications struct {
	mu                          sync.Mutex
	cfg                         *pgx.ConnConfig
	conn                        *pgx.Conn
	ready, running, closed      bool
	max                         int
	subscriptions               map[int32]map[*phoneSubscriber]struct{}
	count                       int
	root                        context.Context
	cancel                      context.CancelFunc
	stopped                     chan struct{}
	active, capacity, connected prometheus.Gauge
	rejected                    *prometheus.CounterVec
}

func NewVerificationNotifications(ctx context.Context, cfg *pgx.ConnConfig, max int, reg prometheus.Registerer) (*VerificationNotifications, error) {
	if cfg == nil || max < 1 || max > 100000 || reg == nil {
		return nil, errors.New("phone notifications: valid primary config, capacity and registry required")
	}
	root, cancel := context.WithCancel(context.Background())
	n := &VerificationNotifications{cfg: cfg.Copy(), max: max, root: root, cancel: cancel, subscriptions: map[int32]map[*phoneSubscriber]struct{}{},
		active:    prometheus.NewGauge(prometheus.GaugeOpts{Name: "finassist_phone_watch_active_streams", Help: "Active pending phone watches on this instance."}),
		capacity:  prometheus.NewGauge(prometheus.GaugeOpts{Name: "finassist_phone_watch_max_streams", Help: "Configured maximum pending phone watches on this instance."}),
		connected: prometheus.NewGauge(prometheus.GaugeOpts{Name: "finassist_phone_watch_listener_ready", Help: "Whether the shared primary LISTEN connection is established."}),
		rejected:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "finassist_phone_watch_rejected_total", Help: "Pending watches refused by local capacity or listener availability."}, []string{"reason"}),
	}
	collectors := []prometheus.Collector{n.active, n.capacity, n.connected, n.rejected}
	for i, c := range collectors {
		if err := reg.Register(c); err != nil {
			for _, registered := range collectors[:i] {
				reg.Unregister(registered)
			}
			cancel()
			return nil, err
		}
	}
	n.capacity.Set(float64(max))
	for _, reason := range []string{"capacity", "not_ready"} {
		n.rejected.WithLabelValues(reason)
	}
	var err error
	n.conn, err = n.connect(ctx)
	if err != nil {
		for _, c := range collectors {
			reg.Unregister(c)
		}
		cancel()
		return nil, err
	}
	n.ready = true
	n.connected.Set(1)
	return n, nil
}
func (n *VerificationNotifications) connect(ctx context.Context) (*pgx.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := pgx.ConnectConfig(ctx, n.cfg.Copy())
	if err != nil {
		return nil, errors.New("phone notifications: primary connection unavailable")
	}
	if _, err = c.Exec(ctx, `LISTEN phone_verification`); err != nil {
		closePhoneListener(c)
		return nil, errors.New("phone notifications: LISTEN unavailable")
	}
	return c, nil
}
func closePhoneListener(c *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Close(ctx)
}

// Subscribe is bounded and nonblocking. Subscribe before re-reading primary state.
func (n *VerificationNotifications) Subscribe(id int32) (verification.Subscription, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.ready || n.closed {
		n.rejected.WithLabelValues("not_ready").Inc()
		return verification.Subscription{}, errors.New("phone notifications: listener not ready")
	}
	if id < 1 {
		return verification.Subscription{}, errors.New("phone notifications: invalid request ID")
	}
	if n.count >= n.max {
		n.rejected.WithLabelValues("capacity").Inc()
		return verification.Subscription{}, errors.New("phone notifications: watch capacity reached")
	}
	sub := &phoneSubscriber{changed: make(chan struct{}, 1), done: make(chan struct{})}
	if n.subscriptions[id] == nil {
		n.subscriptions[id] = map[*phoneSubscriber]struct{}{}
	}
	n.subscriptions[id][sub] = struct{}{}
	n.count++
	n.active.Set(float64(n.count))
	return verification.Subscription{Changed: sub.changed, Done: sub.done, Close: func() { n.remove(id, sub) }}, nil
}
func (n *VerificationNotifications) remove(id int32, sub *phoneSubscriber) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if sub.closed {
		return
	}
	sub.closed = true
	close(sub.done)
	delete(n.subscriptions[id], sub)
	if len(n.subscriptions[id]) == 0 {
		delete(n.subscriptions, id)
	}
	n.count--
	n.active.Set(float64(n.count))
}
func (n *VerificationNotifications) failSubscribers() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ready = false
	n.connected.Set(0)
	for _, subs := range n.subscriptions {
		for sub := range subs {
			if !sub.closed {
				sub.closed = true
				close(sub.done)
			}
		}
	}
	n.subscriptions = map[int32]map[*phoneSubscriber]struct{}{}
	n.count = 0
	n.active.Set(0)
}
func (n *VerificationNotifications) notify(payload string) {
	id, err := strconv.ParseInt(payload, 10, 32)
	if err != nil || id < 1 || strconv.FormatInt(id, 10) != payload {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for sub := range n.subscriptions[int32(id)] {
		select {
		case sub.changed <- struct{}{}:
		default:
		}
	}
}

// Run reconnects on the next worker invocation after an error. Every existing
// stream fails on disconnect and can resume from primary with its same secret.
func (n *VerificationNotifications) Run(ctx context.Context) error {
	n.mu.Lock()
	if n.closed || n.running {
		n.mu.Unlock()
		return errors.New("phone notifications: unavailable worker")
	}
	n.running = true
	n.stopped = make(chan struct{})
	c := n.conn
	n.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(n.root, cancel)
	defer func() {
		stop()
		cancel()
		n.failSubscribers()
		if c != nil {
			closePhoneListener(c)
		}
		n.mu.Lock()
		n.conn = nil
		n.running = false
		close(n.stopped)
		n.mu.Unlock()
	}()
	if c == nil {
		var err error
		c, err = n.connect(ctx)
		if err != nil {
			return err
		}
		n.mu.Lock()
		n.conn = c
		n.ready = true
		n.connected.Set(1)
		n.mu.Unlock()
	}
	for {
		notification, err := c.WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("phone notifications: connection lost")
		}
		if notification.Channel == "phone_verification" {
			n.notify(notification.Payload)
		}
	}
}

// Close also stops a running listener, so an assembly failure cannot leak it.
func (n *VerificationNotifications) Close() {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	n.closed = true
	n.cancel()
	running, done, c := n.running, n.stopped, n.conn
	n.mu.Unlock()
	if running {
		<-done
	} else {
		n.failSubscribers()
		if c != nil {
			closePhoneListener(c)
		}
	}
}
