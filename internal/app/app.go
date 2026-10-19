// Package app assembles the service: identity, database pools, the gRPC server with its
// handlers, clients of other services and background jobs. cmd/user runs it with real
// listeners; apitest runs the same assembly in-process for API tests, so what the tests
// call is what production serves.
package app

import (
	"context"
	"errors"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/adapter/client"
	"github.com/fin-assist/user/internal/adapter/grpc/server"
	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/worker"
)

// App is the assembled service. Wire reads its fields to build adapters and usecases.
type App struct {
	Cfg     config.Config
	Log     *slog.Logger
	DB      *postgres.DB
	GRPC    *server.Server
	Clients *client.Metrics
	Tokens  client.TokenSource
	Jobs    []worker.Job

	closers []func()
}

// New assembles the service from cfg. Close releases what it opened.
func New(ctx context.Context, cfg config.Config, log *slog.Logger, reg prometheus.Registerer) (*App, error) {
	services, users, tokens, err := identity(cfg)
	if err != nil {
		return nil, err
	}
	db, err := postgres.Open(ctx, postgres.Options{
		PrimaryDSN: cfg.DB.PrimaryDSN, ReplicaDSN: cfg.DB.ReplicaDSN,
		MaxConns: cfg.DB.MaxConns, AppName: cfg.Service,
	})
	if err != nil {
		return nil, err
	}
	a := &App{Cfg: cfg, Log: log, DB: db, Tokens: tokens, closers: []func(){db.Close}}
	fail := func(err error) (*App, error) { a.Close(); return nil, err }

	if a.GRPC, err = server.New(server.Options{MaxRecvMsgSize: cfg.GRPC.MaxRecvMsgSize, Reflection: cfg.GRPC.Reflection}, server.Deps{
		Logger: log, Metrics: reg, Policy: AccessPolicy(),
		Services: services, Users: users, Tombstones: tombstones(db),
	}); err != nil {
		return fail(err)
	}
	if a.Clients, err = client.NewMetrics(reg); err != nil {
		return fail(err)
	}
	jobs, closeWired, err := wire(a)
	if err != nil {
		return fail(err)
	}
	a.Jobs = jobs
	a.closers = append(a.closers, closeWired)
	return a, nil
}

// Close releases what New opened, in reverse order.
func (a *App) Close() {
	for i := len(a.closers) - 1; i >= 0; i-- {
		a.closers[i]()
	}
	a.closers = nil
}

// identity returns the verifiers of incoming calls and the token of outgoing ones.
func identity(cfg config.Config) (server.ServiceVerifier, server.UserVerifier, client.TokenSource, error) {
	switch cfg.Auth.Mode {
	case config.AuthInsecureDev:
		return server.DevServiceVerifier{}, server.DevUserVerifier{}, client.DevToken(cfg.Service), nil
	default:
		// TODO(FIN-119): JWT (Ed25519, GetSigningKeys) and ServiceAccount tokens (JWKS of
		// the API server); the outgoing token from client.FileToken{Path: SERVICE_TOKEN_FILE}.
		// Until then the service does not start outside dev rather than trust anyone.
		return nil, nil, nil, errors.New("AUTH_MODE=verify needs the identity library (FIN-119); use AUTH_MODE=insecure-dev with ENV=dev")
	}
}

// tombstones returns the check of deleted users.
func tombstones(*postgres.DB) server.Tombstones {
	// TODO(FIN-110): the tombstone library on db. A service that stores no user data keeps
	// NoTombstones; every data service must replace it (grpc-api.md 7).
	return server.NoTombstones{}
}
