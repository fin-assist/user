package app

import (
	"context"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	idempotency "github.com/fin-assist/go-idempotency"
	outboxlib "github.com/fin-assist/go-outbox"
	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/go-worker"

	authgrpc "github.com/fin-assist/user/internal/adapter/grpc/auth"
	idempotencyadapter "github.com/fin-assist/user/internal/adapter/idempotency"
	eventsadapter "github.com/fin-assist/user/internal/adapter/outbox"
	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/auth"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/verification"
)

func wireSessions(ctx context.Context, a *App, reg prometheus.Registerer) ([]worker.Job, func(), error) {
	now := a.clock()
	key, err := config.LoadSessionKey(a.Cfg.Sessions)
	if err != nil {
		return nil, nil, err
	}
	cipher, err := idempotency.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	pool, err := postgres.Open(ctx, postgres.Options{PrimaryDSN: a.Cfg.DB.PrimaryDSN, MaxConns: a.Cfg.Sessions.MaxConns, AppName: a.Cfg.Service + "-auth"})
	if err != nil {
		return nil, nil, err
	}
	engine, err := idempotency.New(pool, idempotency.Options{Clock: now, Cipher: cipher, Registerer: reg, Methods: map[string]idempotency.Policy{
		userv1.AuthService_StartPhoneVerification_FullMethodName: {Encrypt: true},
		userv1.AuthService_Register_FullMethodName:               {Encrypt: true},
		userv1.AuthService_RefreshTokens_FullMethodName:          {Encrypt: true, TTL: time.Minute},
	}})
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	guard, err := tombstone.New(pool, pool.Primary(), now)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	publisher, err := outboxlib.NewPublisher(pool, now)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	events := eventsadapter.NewEvents(publisher)
	phoneJobs, closePhone, err := wirePhone(ctx, a, pool, guard, reg)
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	closeAll := func() { closePhone(); pool.Close() }
	repo := postgres.NewAuth(pool)
	sessions := auth.New(pool, repo, guard, a.Signing, events, now, token.New, a.Cfg.Sessions.RefreshTTL)
	starts, err := verification.NewStarter(pool, postgres.NewVerificationStarts(pool).WithClock(now), idempotencyadapter.StartEngine{StartEngine: engine}, a.Limits.WithStore(postgres.NewLimits(pool)), repo, guard, a.phoneProvider, a.phoneSelector, now, token.New, a.Log, reg)
	if err != nil {
		closeAll()
		return nil, nil, err
	}
	if err := starts.SetUnknownTimeout(a.Cfg.Phone.UnknownTimeout); err != nil {
		closeAll()
		return nil, nil, err
	}
	if a.Cfg.Zvonok.Provider == "fake" {
		starts.SetImmediateConfirmation(a.authConfirmation)
	}
	authgrpc.Register(a.GRPC.GRPC(), sessions, engine, postgres.NewSessionTransactions(pool), a.DB, a.Limits, guard, a.Verifications, starts)
	cleanup, err := worker.Every("auth-cleanup", time.Minute, a.Log, worker.SystemClock{}, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		for {
			var cleaned int64
			err := pool.Run(ctx, func(ctx context.Context) error {
				n, err := engine.Cleanup(ctx, 1000)
				if err != nil {
					return err
				}
				cleaned += n
				n, err = repo.CleanupSessions(ctx, now().UTC(), 1000)
				cleaned += n
				return err
			})
			if err != nil {
				return err
			}
			if cleaned < 1000 {
				return nil
			}
		}
	})
	if err != nil {
		closeAll()
		return nil, nil, err
	}
	return append(phoneJobs, cleanup), closeAll, nil
}
