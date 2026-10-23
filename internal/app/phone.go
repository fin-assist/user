package app

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/adapter/verificationcrypto"
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
	return []worker.Job{{Name: "phone-notifications", Run: notifications.Run}, cleanup}, notifications.Close, nil
}
