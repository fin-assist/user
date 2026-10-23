package app

import (
	"context"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	idempotency "github.com/fin-assist/go-idempotency"
	"github.com/prometheus/client_golang/prometheus"

	authgrpc "github.com/fin-assist/user/internal/adapter/grpc/auth"
	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/auth"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/worker"
)

func wireSessions(ctx context.Context, a *App, events auth.Events, reg prometheus.Registerer) (worker.Job, func(), error) {
	key, err := config.LoadSessionKey(a.Cfg.Sessions)
	if err != nil {
		return worker.Job{}, nil, err
	}
	cipher, err := idempotency.NewCipher(key)
	if err != nil {
		return worker.Job{}, nil, err
	}
	pool, err := postgres.Open(ctx, postgres.Options{PrimaryDSN: a.Cfg.DB.PrimaryDSN, MaxConns: a.Cfg.Sessions.MaxConns, AppName: a.Cfg.Service + "-auth"})
	if err != nil {
		return worker.Job{}, nil, err
	}
	engine, err := idempotency.New(pool, idempotency.Options{Clock: time.Now, Cipher: cipher, Registerer: reg, Methods: map[string]idempotency.Policy{
		userv1.AuthService_Register_FullMethodName:      {Encrypt: true},
		userv1.AuthService_RefreshTokens_FullMethodName: {Encrypt: true, TTL: time.Minute},
	}})
	if err != nil {
		pool.Close()
		return worker.Job{}, nil, err
	}
	repo := postgres.NewAuth(a.DB)
	sessions := auth.New(pool, repo, a.Tombstones, a.Signing, events, time.Now, token.New, a.Cfg.Sessions.RefreshTTL)
	authgrpc.Register(a.GRPC.GRPC(), sessions, engine, pool, a.DB, a.Limits, a.Tombstones)
	cleanup := worker.Every("auth-cleanup", time.Minute, a.Log, func(ctx context.Context) error {
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
				n, err = repo.CleanupSessions(ctx, time.Now().UTC(), 1000)
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
	return cleanup, pool.Close, nil
}
