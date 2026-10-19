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

func TestReadinessFollowsCheck(t *testing.T) {
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
