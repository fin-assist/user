package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunRestartsFailedAndPanickingJobs(t *testing.T) {
	RestartDelay = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	var fails, panics atomic.Int32
	done := make(chan struct{})
	go func() {
		Run(ctx, slog.New(slog.DiscardHandler),
			Job{Name: "fails", Run: func(context.Context) error { fails.Add(1); return errors.New("x") }},
			Job{Name: "panics", Run: func(context.Context) error { panics.Add(1); panic("bug") }},
		)
		close(done)
	}()
	deadline := time.After(5 * time.Second)
	for fails.Load() < 2 || panics.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("not restarted: fails %d, panics %d", fails.Load(), panics.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunDoesNotRestartJobStoppedByShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var runs atomic.Int32
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		Run(ctx, slog.New(slog.DiscardHandler), Job{Name: "blocks", Run: func(ctx context.Context) error {
			if runs.Add(1) == 1 {
				close(started)
			}
			<-ctx.Done()
			return ctx.Err()
		}})
		close(done)
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("job ran %d times, want 1", n)
	}
}

func TestEvery(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	var n atomic.Int32
	j := Every("tick", 5*time.Millisecond, slog.New(slog.DiscardHandler), func(context.Context) error {
		if n.Add(1) == 3 {
			cancel()
		}
		return errors.New("one bad iteration does not stop the job")
	})
	if err := j.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if n.Load() < 3 {
		t.Fatalf("%d calls", n.Load())
	}
}
