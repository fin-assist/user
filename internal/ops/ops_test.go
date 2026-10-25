package ops

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestUnitReadinessFollowsCheck(t *testing.T) {
	var fail atomic.Bool
	changes := make(chan bool, 10)
	r := &Readiness{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go r.Watch(ctx, 5*time.Millisecond, slog.New(slog.DiscardHandler), func(context.Context) error {
		if fail.Load() {
			return errors.New("db down")
		}
		return nil
	}, func(ok bool) { changes <- ok })

	if !<-changes {
		t.Fatal("first result must be reported: ready")
	}
	fail.Store(true)
	if <-changes {
		t.Fatal("want not ready after the check fails")
	}

	h := Handler(NewRegistry(), r)
	for path, want := range map[string]int{"/healthz": 200, "/readyz": 503, "/metrics": 200} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("%s: %d, want %d", path, rec.Code, want)
		}
	}
}

// After Drain neither a passing check nor a stale result makes the process ready again:
// /readyz answers 503 for the whole SHUTDOWN_DELAY while liveness stays up.
func TestUnitDrainKeepsReadinessDownWhileTheCheckPasses(t *testing.T) {
	r := &Readiness{}
	changes := make(chan bool, 10)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Watch(ctx, 5*time.Millisecond, slog.New(slog.DiscardHandler), func(context.Context) error { return nil }, func(ok bool) { changes <- ok })
	}()
	if !<-changes || !r.Ready() {
		t.Fatal("passing check did not report ready")
	}
	h := Handler(NewRegistry(), r)
	status := func(path string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec.Code
	}
	if status("/readyz") != http.StatusOK {
		t.Fatal("ready process answered not ready")
	}
	r.Drain()
	if r.Ready() || status("/readyz") != http.StatusServiceUnavailable {
		t.Fatal("draining process still ready")
	}
	// Several more passing checks must not bring readiness back.
	deadline := time.After(50 * time.Millisecond)
	for wait := true; wait; {
		select {
		case ok := <-changes:
			if ok {
				t.Fatal("watch reported ready after drain")
			}
		case <-deadline:
			wait = false
		}
	}
	if r.Ready() || status("/readyz") != http.StatusServiceUnavailable || status("/healthz") != http.StatusOK {
		t.Fatal("drain did not hold or changed liveness")
	}
	cancel()
	<-done
}

// Drain arriving while Watch publishes «ready» waits for that publication, and no
// later result is published as ready. Otherwise a late onChange(true) (gRPC
// SetServing(true)) could land after the caller already reported NOT_SERVING.
func TestUnitDrainWaitsForAPublicationInProgress(t *testing.T) {
	r := &Readiness{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls atomic.Int64
	checked := make(chan int64, 1)
	check := func(context.Context) error {
		n := calls.Add(1)
		select { // report the latest call without blocking the loop
		case checked <- n:
		default:
		}
		if n == 1 {
			return errors.New("db down") // start not ready, so the next pass is a flip
		}
		return nil
	}
	inPublication, release := make(chan struct{}), make(chan struct{})
	var published, lateReady atomic.Bool
	onChange := func(ok bool) {
		if !ok {
			return
		}
		if published.Load() {
			lateReady.Store(true)
			return
		}
		if r.mu.TryLock() {
			r.mu.Unlock()
			t.Error("onChange ran outside the lock that orders it against Drain")
		}
		close(inPublication)
		<-release
		published.Store(true)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Watch(ctx, time.Millisecond, slog.New(slog.DiscardHandler), check, onChange)
	}()
	<-inPublication
	drained := make(chan bool, 1)
	go func() {
		r.Drain()
		drained <- published.Load()
	}()
	// A correct Drain blocks while onChange(true) runs; a Drain that does not wait
	// returns within this window and is caught. The window bounds a negative check,
	// it does not synchronise the test.
	select {
	case <-drained:
		t.Fatal("Drain returned while onChange(true) was still running")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case wasPublished := <-drained:
		if !wasPublished {
			t.Fatal("Drain returned before onChange(true) finished")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Drain did not return")
	}
	// The loop is sequential: once check call base+2 has started, the publication of
	// call base+1 — a passing check that began after Drain — has completed.
	base := calls.Load()
	for n := int64(0); n < base+2; {
		select {
		case n = <-checked:
		case <-time.After(3 * time.Second):
			t.Fatal("watch stopped checking after drain")
		}
	}
	cancel()
	<-done
	if lateReady.Load() || r.Ready() {
		t.Fatal("ready published after Drain")
	}
}
