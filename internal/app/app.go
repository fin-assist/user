// Package app assembles the service: identity, database pools, the gRPC server with its
// handlers, clients of other services and background jobs. cmd/user runs it with real
// listeners; apitest runs the same assembly in-process for API tests, so what the tests
// call is what production serves.
package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	tombstone "github.com/fin-assist/go-tombstone"

	"github.com/fin-assist/user/internal/ratelimit"
	"github.com/fin-assist/user/internal/signing"
	"github.com/fin-assist/user/internal/verification"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/go-grpc/client"
	"github.com/fin-assist/go-grpc/server"
	"github.com/fin-assist/go-worker"

	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/config"
)

// App is the assembled service. Wire reads its fields to build adapters and usecases.
type App struct {
	authConfirmation verification.CreationConfirmation

	Cfg        config.Config
	Log        *slog.Logger
	DB         *postgres.DB
	GRPC       *server.Server
	Clients    *client.Metrics
	Tokens     client.TokenSource
	Jobs       []worker.Job
	Signing    *signing.Keys
	Users      server.UserVerifier
	Tombstones *tombstone.Store
	Limits     *ratelimit.Limiter
	// Verifications accepts only provider-verified proofs from trusted Go adapters.
	Verifications *verification.Service
	phoneProvider verification.Provider
	phoneSelector verification.NumberSelector
	// PhoneCallback is served on a separate ingress listener, never on ops or gRPC.
	PhoneCallback   http.Handler
	zvonokTransport http.RoundTripper
	// limitNow is the clock of the authentication rate limiter (time.Now by default).
	limitNow func() time.Time

	closers []func()
}

// New assembles the service from cfg. Close releases what it opened.
func New(ctx context.Context, cfg config.Config, log *slog.Logger, reg prometheus.Registerer, options ...Option) (*App, error) {
	if err := cfg.Zvonok.Validate(cfg.Env); err != nil {
		return nil, err
	}
	keys, err := signing.Load(signing.Options{PrivateKeyFile: cfg.Signing.PrivateKeyFile, PublicKeysFile: cfg.Signing.PublicKeysFile, KeyID: cfg.Signing.KeyID, Issuer: cfg.Signing.Issuer, Audience: cfg.Signing.Audience, Clock: time.Now})
	if err != nil {
		return nil, err
	}
	ids, err := buildIdentity(cfg, keys, reg)
	if err != nil {
		return nil, err
	}
	db, err := postgres.Open(ctx, postgres.Options{
		PrimaryDSN: cfg.DB.PrimaryDSN, ReplicaDSN: cfg.DB.ReplicaDSN,
		MaxConns: cfg.DB.MaxConns, AppName: cfg.Service,
	})
	if err != nil {
		ids.close()
		return nil, err
	}
	a := &App{Cfg: cfg, Log: log, DB: db, Tokens: ids.tokens, Signing: keys, Users: ids.users, Jobs: ids.jobs, closers: []func(){ids.close, db.Close}, limitNow: time.Now}
	for _, option := range options {
		option(a)
	}
	fail := func(err error) (*App, error) { a.Close(); return nil, err }
	a.Tombstones, err = tombstone.New(db, db.Primary(), time.Now)
	if err != nil {
		return fail(err)
	}

	if a.GRPC, err = server.New(server.Options{MaxRecvMsgSize: cfg.GRPC.MaxRecvMsgSize, Reflection: cfg.GRPC.Reflection}, server.Deps{
		Logger: log, Metrics: reg, Policy: AccessPolicy(),
		Services: ids.services, Users: ids.users, Tombstones: deletedUsers{a.Tombstones},
	}); err != nil {
		return fail(err)
	}
	if a.Clients, err = client.NewMetrics(reg); err != nil {
		return fail(err)
	}
	if a.Limits, err = ratelimit.New(cfg.Limits, postgres.NewLimits(db), a.limitNow, reg); err != nil {
		return fail(err)
	}
	jobs, closeWired, err := wire(ctx, a, reg)
	if err != nil {
		return fail(err)
	}
	a.Jobs = append(a.Jobs, jobs...)
	a.closers = append(a.closers, closeWired)
	return a, nil
}

// Option supplies trusted assembly dependencies before any handler is registered.
type Option func(*App)

// WithPhoneBackend installs the provider and transactional number selector. The
// default executable keeps creation disabled; PHONE_PROVIDER=fake or zvonok opts
// into a concrete guarded assembly. This seam is used by controlled API-test providers.
func WithPhoneBackend(provider verification.Provider, selector verification.NumberSelector) Option {
	return func(a *App) { a.phoneProvider, a.phoneSelector = provider, selector }
}

// WithZvonokTransport supplies a trusted transport for HTTP integration tests;
// production configuration cannot override the pinned HTTPS endpoint.
func WithZvonokTransport(transport http.RoundTripper) Option {
	return func(a *App) { a.zvonokTransport = transport }
}

// WithLimitClock pins the clock that selects the fixed UTC windows of the
// authentication rate limiter. API tests use it so that one scenario never
// straddles a window boundary; production keeps time.Now.
func WithLimitClock(now func() time.Time) Option {
	return func(a *App) {
		if now != nil {
			a.limitNow = now
		}
	}
}

// Close releases what New opened, in reverse order.
func (a *App) Close() {
	for i := len(a.closers) - 1; i >= 0; i-- {
		a.closers[i]()
	}
	a.closers = nil
}

// deletedUsers adapts the library's primary-backed checker to the existing server chain.
type deletedUsers struct{ store *tombstone.Store }

func (d deletedUsers) IsDeleted(ctx context.Context, id string) (bool, error) {
	return d.store.Deleted(ctx, id)
}
