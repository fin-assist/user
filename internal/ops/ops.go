// Package ops is the operational HTTP listener: /metrics for Prometheus, /healthz
// (the process is alive) and /readyz (it can serve: the database answers). It works the
// same under k3s and under systemd on the stand; gRPC health on the main port reports the
// same readiness.
package ops

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewRegistry returns a registry with the Go and process collectors.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return reg
}

// Readiness keeps the last result of the readiness check. Once Drain is called the
// process is shutting down and stays not ready for good, whatever the check says.
type Readiness struct {
	ready atomic.Bool
	// mu orders Drain against publishing a result (ready and onChange), so that no
	// onChange(true) runs after Drain has returned.
	mu       sync.Mutex
	draining bool
}

// Ready reports the last result; false after Drain.
func (r *Readiness) Ready() bool { return r.ready.Load() }

// Drain marks the process as shutting down: /readyz answers 503 from now on, so
// balancers stop sending new requests during SHUTDOWN_DELAY. It is irreversible. It
// waits for a result being published, so the caller's next state change (gRPC health
// NOT_SERVING) is never overwritten by a late onChange(true).
func (r *Readiness) Drain() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.draining = true
	r.ready.Store(false)
}

// Watch runs check every interval until ctx ends and calls onChange when the result flips.
func (r *Readiness) Watch(ctx context.Context, interval time.Duration, log *slog.Logger, check func(context.Context) error, onChange func(bool)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for first := true; ; first = false {
		cctx, cancel := context.WithTimeout(ctx, interval)
		err := check(cctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		r.publish(ctx, log, err, first, onChange)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// publish records the result of one check and reports a change; under mu, so it is
// ordered against Drain as a whole.
func (r *Readiness) publish(ctx context.Context, log *slog.Logger, err error, first bool, onChange func(bool)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ok := err == nil && !r.draining
	if r.ready.Swap(ok) != ok || first {
		if !ok {
			log.WarnContext(ctx, "not ready", "error", err)
		} else {
			log.InfoContext(ctx, "ready")
		}
		onChange(ok)
	}
}

// Handler serves /metrics, /healthz and /readyz.
func Handler(reg *prometheus.Registry, r *Readiness) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !r.Ready() {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// Server returns the HTTP server with timeouts; it carries no personal data.
func Server(addr string, h http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
}
