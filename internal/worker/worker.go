// Package worker runs the background loops of a pod: the outbox dispatcher (FIN-109),
// scheduled jobs. Every pod runs them; jobs that must run once take a lock or work by
// FOR UPDATE SKIP LOCKED, as the outbox does (grpc-api.md 6.1).
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// Job is a named loop. Run works until ctx ends; an error or a panic restarts it after a
// pause, so one bad iteration does not stop the pod's other work.
type Job struct {
	Name string
	Run  func(ctx context.Context) error
}

// Every makes a job that calls fn every interval (first call right away). An error of
// one call is logged and the next call happens on schedule.
func Every(name string, interval time.Duration, log *slog.Logger, fn func(ctx context.Context) error) Job {
	return Job{Name: name, Run: func(ctx context.Context) error {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			if err := fn(ctx); err != nil && ctx.Err() == nil {
				log.ErrorContext(ctx, "job iteration failed", "job", name, "error", err)
			}
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
			}
		}
	}}
}

// RestartDelay is the pause before a failed job starts again.
var RestartDelay = 5 * time.Second

// Run runs the jobs until ctx ends and waits for all of them to return.
func Run(ctx context.Context, log *slog.Logger, jobs ...Job) {
	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				err := safeRun(ctx, j)
				if ctx.Err() != nil {
					return
				}
				log.ErrorContext(ctx, "job stopped, restarting", "job", j.Name, "error", err, "delay", RestartDelay)
				select {
				case <-ctx.Done():
				case <-time.After(RestartDelay):
				}
			}
		}()
	}
	wg.Wait()
}

func safeRun(ctx context.Context, j Job) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panic: %v\n%s", p, debug.Stack())
		}
	}()
	if err = j.Run(ctx); err == nil {
		err = errors.New("returned before shutdown")
	}
	return err
}
