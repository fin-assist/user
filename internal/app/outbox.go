package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	usersettingsv1 "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	outboxlib "github.com/fin-assist/go-outbox"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/user/internal/adapter/client"
	events "github.com/fin-assist/user/internal/adapter/outbox"
	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/worker"
)

func wireOutbox(ctx context.Context, a *App, reg prometheus.Registerer) (*events.Settings, worker.Job, func(), error) {
	if err := a.Cfg.Outbox.Validate(); err != nil {
		return nil, worker.Job{}, nil, err
	}
	for _, target := range a.Cfg.Outbox.TimezoneTargets {
		if a.Cfg.Peers[target] == "" {
			return nil, worker.Job{}, nil, fmt.Errorf("outbox: peer address required for %s", target)
		}
	}
	pool, err := postgres.Open(ctx, postgres.Options{PrimaryDSN: a.Cfg.DB.PrimaryDSN, MaxConns: a.Cfg.Outbox.MaxConns, AppName: a.Cfg.Service + "-outbox"})
	if err != nil {
		return nil, worker.Job{}, nil, err
	}
	closers := []func(){pool.Close}
	closeAll := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	fail := func(err error) (*events.Settings, worker.Job, func(), error) {
		closeAll()
		return nil, worker.Job{}, nil, err
	}
	publisher, err := outboxlib.NewPublisher(a.DB, time.Now)
	if err != nil {
		return fail(err)
	}
	routes := map[string]outboxlib.Delivery{}
	method := usersettingsv1.UserSettingsSyncService_SetUserTimezone_FullMethodName
	for _, target := range a.Cfg.Outbox.TimezoneTargets {
		conn, err := client.Dial(a.Cfg.Peers[target], client.Options{Peer: target, Tokens: a.Tokens, Metrics: a.Clients, Retry: []string{strings.TrimPrefix(method, "/")}, DefaultTimeout: 30 * time.Second})
		if err != nil {
			return fail(err)
		}
		closers = append(closers, func() { _ = conn.Close() })
		send, err := outboxlib.GRPCDelivery(conn, a.Tokens, method, func() proto.Message { return &usersettingsv1.SetUserTimezoneRequest{} }, func() proto.Message { return &usersettingsv1.SetUserTimezoneResponse{} })
		if err != nil {
			return fail(err)
		}
		routes[target] = send
	}
	dispatcher, err := outboxlib.NewDispatcher(outboxlib.Options{Pool: pool.Primary(), Routes: outboxlib.Routes{events.TimezoneTopic: routes}, Now: time.Now, Logger: a.Log, Registerer: reg, PollInterval: a.Cfg.Outbox.PollInterval})
	if err != nil {
		return fail(err)
	}
	return events.NewSettings(publisher), worker.Job{Name: "outbox", Run: dispatcher.Run}, closeAll, nil
}
