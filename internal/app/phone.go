package app

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/adapter/verificationcrypto"
	"github.com/fin-assist/user/internal/adapter/zvonok"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/verification"
	"github.com/fin-assist/user/internal/worker"
)

func wirePhone(ctx context.Context, a *App, reg prometheus.Registerer) ([]worker.Job, func(), error) {
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
	cleanup := worker.Every("phone-cleanup", time.Minute, a.Log, func(ctx context.Context) error {
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
	jobs := []worker.Job{{Name: "phone-notifications", Run: notifications.Run}, cleanup}
	closeBackend := func() {}
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
		numbers, err := verification.NewNumbers(repo, a.Log, reg)
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
		jobs = append(jobs, worker.Every("phone-callback-outcomes", time.Minute, a.Log, func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			return numbers.Report(ctx)
		}))
	}
	return jobs, func() { closeBackend(); notifications.Close() }, nil
}
