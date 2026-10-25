package app

import (
	"bytes"
	"context"
	"errors"
	"time"

	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/go-worker"

	"github.com/fin-assist/user/internal/adapter/fakephone"
	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/adapter/verificationcrypto"
	"github.com/fin-assist/user/internal/adapter/zvonok"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/verification"
)

func wirePhone(ctx context.Context, a *App, authDB *postgres.DB, authGuard *tombstone.Store, reg prometheus.Registerer) ([]worker.Job, func(), error) {
	if err := a.Cfg.Zvonok.Validate(a.Cfg.Env); err != nil {
		return nil, nil, err
	}
	if a.Cfg.Env == config.EnvProd && a.phoneProvider != nil && a.phoneProvider.Name() == "fake" {
		return nil, nil, errors.New("phone: fake backend is forbidden in prod")
	}
	key, err := config.LoadPhoneKey(a.Cfg.Phone)
	if err != nil {
		return nil, nil, err
	}
	authKey, err := config.LoadSessionKey(a.Cfg.Sessions)
	if err != nil {
		return nil, nil, err
	}
	if bytes.Equal(key, authKey) {
		return nil, nil, errors.New("phone result key must differ from auth replay key")
	}
	codec, err := verificationcrypto.New(key, a.Cfg.Phone.ResultKeyID)
	if err != nil {
		return nil, nil, err
	}
	cfg := a.DB.Primary().Config().ConnConfig.Copy()
	cfg.RuntimeParams["application_name"] = a.Cfg.Service + "-phone-listener"
	notifications, err := postgres.NewVerificationNotifications(ctx, cfg, a.Cfg.Phone.MaxStreams, reg)
	if err != nil {
		return nil, nil, err
	}
	a.Verifications = verification.New(a.DB, postgres.NewVerifications(a.DB), postgres.NewAuth(a.DB), a.Tombstones, a.Signing, codec, notifications, time.Now, token.New, a.Cfg.Sessions.RefreshTTL, a.Cfg.Phone.ResyncInterval)
	a.authConfirmation = verification.New(authDB, postgres.NewVerifications(authDB), postgres.NewAuth(authDB), authGuard, a.Signing, codec, notifications, time.Now, token.New, a.Cfg.Sessions.RefreshTTL, a.Cfg.Phone.ResyncInterval)
	if a.Cfg.Env == config.EnvProd {
		guard, err := verification.NewProductionProofGuard(a.Log, reg)
		if err != nil {
			notifications.Close()
			return nil, nil, err
		}
		a.Verifications.SetProofGuard(guard)
	}
	cleanup, err := worker.Every("phone-cleanup", time.Minute, a.Log, worker.SystemClock{}, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		for {
			n, err := a.Verifications.Cleanup(ctx, 1000)
			if err != nil {
				return err
			}
			if n < 1000 {
				return nil
			}
		}
	})
	if err != nil {
		notifications.Close()
		return nil, nil, err
	}
	jobs := []worker.Job{{Name: "phone-notifications", Run: notifications.Run}, cleanup}
	closeBackend := func() {}
	if a.Cfg.Zvonok.Provider == "fake" {
		if a.phoneProvider != nil || a.phoneSelector != nil {
			notifications.Close()
			return nil, nil, errors.New("phone: multiple configured backends")
		}
		numbers, err := verification.NewNumbers(callbackNumberRepository{postgres.NewVerifications(authDB), postgres.NewVerifications(a.DB)}, a.Log, reg)
		if err != nil {
			notifications.Close()
			return nil, nil, err
		}
		a.phoneProvider, a.phoneSelector = fakephone.New(), numbers
		outcomes, err := callbackOutcomesJob(a, numbers)
		if err != nil {
			closeBackend()
			notifications.Close()
			return nil, nil, err
		}
		jobs = append(jobs, outcomes)
	}
	if a.Cfg.Zvonok.Provider == "zvonok" {
		fail := func(err error) ([]worker.Job, func(), error) {
			closeBackend()
			notifications.Close()
			return nil, nil, err
		}
		if err := a.Cfg.Zvonok.Validate(a.Cfg.Env); err != nil {
			return fail(err)
		}
		if a.phoneProvider != nil || a.phoneSelector != nil {
			return fail(errors.New("phone: multiple configured backends"))
		}
		provider, err := zvonok.New(a.Cfg.Zvonok.PublicKey, a.Cfg.Zvonok.CampaignID, postgres.NewZvonokGate(a.DB, a.Cfg.Zvonok.PublicKey), a.zvonokTransport, reg)
		if err != nil {
			return fail(err)
		}
		closeBackend = provider.Close
		repo := postgres.NewVerifications(a.DB)
		numbers, err := verification.NewNumbers(callbackNumberRepository{postgres.NewVerifications(authDB), repo}, a.Log, reg)
		if err != nil {
			return fail(err)
		}
		checker, err := verification.NewChecker(provider.Name(), provider, repo, a.Verifications, a.Cfg.Zvonok.PollInterval, a.Cfg.Phone.MaxStreams, reg)
		if err != nil {
			return fail(err)
		}
		handler, err := zvonok.NewPostback(checker, a.Cfg.Zvonok.TrustedProxies, reg)
		if err != nil {
			return fail(err)
		}
		a.phoneProvider, a.phoneSelector = provider, numbers
		a.PhoneCallback = handler
		a.Verifications.SetStatusChecker(checker)
		outcomes, err := callbackOutcomesJob(a, numbers)
		if err != nil {
			closeBackend()
			notifications.Close()
			return nil, nil, err
		}
		jobs = append(jobs, outcomes)
	}
	return jobs, func() { closeBackend(); notifications.Close() }, nil
}

func callbackOutcomesJob(a *App, numbers *verification.Numbers) (worker.Job, error) {
	return worker.Every("phone-callback-outcomes", time.Minute, a.Log, worker.SystemClock{}, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return numbers.Report(ctx)
	})
}

// Selection writes in Start's dedicated transaction; reports keep using the ordinary pool.
type callbackNumberRepository struct{ write, read *postgres.Verifications }

func (r callbackNumberRepository) SavePool(ctx context.Context, id int32, pool []string, selected string) error {
	return r.write.SavePool(ctx, id, pool, selected)
}
func (r callbackNumberRepository) PoolOutcomes(ctx context.Context, since time.Time) ([]verification.PoolOutcome, map[string]int64, error) {
	return r.read.PoolOutcomes(ctx, since)
}
