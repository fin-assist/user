package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/fin-assist/go-worker"

	"github.com/fin-assist/user/internal/app"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/logx"
	"github.com/fin-assist/user/internal/ops"
)

// boundAddrs holds the actual addresses of the listeners after every bind succeeded.
// Callback is nil when the phone callback listener is not configured.
type boundAddrs struct {
	GRPC, HTTP, Callback net.Addr
}

// serveHooks are optional observers of the serve lifecycle; the zero value observes nothing.
type serveHooks struct {
	// listening is called once, after all listeners are bound and before serving starts.
	// A configured port 0 is reported as the port the kernel actually assigned.
	listening func(boundAddrs)
}

// runServe starts the gRPC server, the ops listener and the background jobs, and stops
// them in order on SIGTERM: NOT_SERVING, a pause for balancers, graceful stop, jobs, pools.
// Every listener is bound before any of them serves; a failed bind releases the others.
func runServe(ctx context.Context, hooks serveHooks) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := logx.New(os.Stderr, cfg.Log.Format, cfg.Log.Level, cfg.Service)
	slog.SetDefault(log)
	if cfg.Auth.Mode == config.AuthInsecureDev {
		log.Warn("AUTH_MODE=insecure-dev: identities are not verified, dev only")
	}

	reg := ops.NewRegistry()
	a, err := app.New(ctx, cfg, log, reg)
	if err != nil {
		return err
	}
	defer a.Close()
	srv, db, jobs := a.GRPC, a.DB, a.Jobs

	grpcLis, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.GRPC.Addr)
	if err != nil {
		return err
	}
	httpLis, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.HTTP.Addr)
	if err != nil {
		_ = grpcLis.Close()
		return fmt.Errorf("http: %w", err)
	}
	readiness := &ops.Readiness{}
	httpSrv := ops.Server(cfg.HTTP.Addr, ops.Handler(reg, readiness))
	var callbackSrv *http.Server
	var callbackLis net.Listener
	bound := boundAddrs{GRPC: grpcLis.Addr(), HTTP: httpLis.Addr()}
	if a.PhoneCallback != nil {
		callbackLis, err = (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Zvonok.CallbackAddr)
		if err != nil {
			_ = grpcLis.Close()
			_ = httpLis.Close()
			return err
		}
		bound.Callback = callbackLis.Addr()
		callbackSrv = &http.Server{Handler: a.PhoneCallback, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8192}
	}
	if hooks.listening != nil {
		hooks.listening(bound)
	}

	runCtx, stopRun := context.WithCancel(context.WithoutCancel(ctx))
	defer stopRun()
	var wg sync.WaitGroup
	errc := make(chan error, 4)
	if callbackSrv != nil {
		wg.Go(func() {
			if err := callbackSrv.Serve(callbackLis); !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("callback listener: %w", err)
			}
		})
	}
	wg.Go(func() {
		if err := srv.Serve(grpcLis); err != nil {
			errc <- fmt.Errorf("grpc: %w", err)
		}
	})
	wg.Go(func() {
		if err := httpSrv.Serve(httpLis); !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("http: %w", err)
		}
	})
	wg.Go(func() { readiness.Watch(runCtx, 5*time.Second, log, db.Ping, srv.SetServing) })
	wg.Go(func() {
		if err := worker.Run(runCtx, worker.Options{Logger: log, Clock: worker.SystemClock{}, RestartDelay: 5 * time.Second}, jobs...); err != nil && runCtx.Err() == nil {
			errc <- fmt.Errorf("jobs: %w", err)
		}
	})
	log.Info("serving", "version", version, "grpc", bound.GRPC.String(), "http", bound.HTTP.String(), "env", cfg.Env)

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case runErr = <-errc:
		log.Error("listener failed, shutting down", "error", runErr)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownDelay+cfg.ShutdownTimeout)
	defer cancel()
	// HTTP /readyz goes not ready together with gRPC health, before SHUTDOWN_DELAY.
	readiness.Drain()
	srv.Shutdown(shutdownCtx, cfg.ShutdownDelay)
	stopRun()
	_ = httpSrv.Shutdown(shutdownCtx)
	if callbackSrv != nil {
		if err := callbackSrv.Shutdown(shutdownCtx); err != nil {
			_ = callbackSrv.Close()
		}
	}
	wg.Wait()
	return runErr
}
