package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	userdeletionv1 "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	usersettingsv1 "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	outboxlib "github.com/fin-assist/go-outbox"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/user/internal/adapter/client"
	events "github.com/fin-assist/user/internal/adapter/outbox"
	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/worker"
)

func wireOutbox(ctx context.Context, a *App, reg prometheus.Registerer) (*events.Events, worker.Job, func(), error) {
	if err := a.Cfg.Outbox.Validate(); err != nil {
		return nil, worker.Job{}, nil, err
	}
	peers := map[string]bool{}
	for _, targets := range [][]string{a.Cfg.Outbox.TimezoneTargets, a.Cfg.Outbox.DeletionTargets} {
		for _, target := range targets {
			if a.Cfg.Peers[target] == "" {
				return nil, worker.Job{}, nil, fmt.Errorf("outbox: peer address required for %s", target)
			}
			peers[target] = true
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
	fail := func(err error) (*events.Events, worker.Job, func(), error) {
		closeAll()
		return nil, worker.Job{}, nil, err
	}
	publisher, err := outboxlib.NewPublisher(a.DB, time.Now)
	if err != nil {
		return fail(err)
	}
	routes := outboxlib.Routes{events.TimezoneTopic: {}, events.DeletionTopic: {}}
	connections := map[string]*grpc.ClientConn{}
	names := make([]string, 0, len(peers))
	for target := range peers {
		names = append(names, target)
	}
	slices.Sort(names)
	for _, target := range names {
		conn, err := client.Dial(a.Cfg.Peers[target], client.Options{Peer: target, Tokens: a.Tokens, Metrics: a.Clients, Retry: []string{strings.TrimPrefix(usersettingsv1.UserSettingsSyncService_SetUserTimezone_FullMethodName, "/"), strings.TrimPrefix(userdeletionv1.UserDeletionService_PurgeUserData_FullMethodName, "/")}, DefaultTimeout: 30 * time.Second})
		if err != nil {
			return fail(err)
		}
		closers = append(closers, func() { _ = conn.Close() })
		connections[target] = conn
	}
	for _, target := range a.Cfg.Outbox.TimezoneTargets {
		send, err := outboxlib.GRPCDelivery(connections[target], a.Tokens, usersettingsv1.UserSettingsSyncService_SetUserTimezone_FullMethodName, func() proto.Message { return &usersettingsv1.SetUserTimezoneRequest{} }, func() proto.Message { return &usersettingsv1.SetUserTimezoneResponse{} })
		if err != nil {
			return fail(err)
		}
		routes[events.TimezoneTopic][target] = send
	}
	for _, target := range a.Cfg.Outbox.DeletionTargets {
		send, err := outboxlib.GRPCDelivery(connections[target], a.Tokens, userdeletionv1.UserDeletionService_PurgeUserData_FullMethodName, func() proto.Message { return &userdeletionv1.PurgeUserDataRequest{} }, func() proto.Message { return &userdeletionv1.PurgeUserDataResponse{} })
		if err != nil {
			return fail(err)
		}
		routes[events.DeletionTopic][target] = send
	}
	dispatcher, err := outboxlib.NewDispatcher(outboxlib.Options{Pool: pool.Primary(), Routes: routes, Now: time.Now, Logger: a.Log, Registerer: reg, PollInterval: a.Cfg.Outbox.PollInterval})
	if err != nil {
		return fail(err)
	}
	return events.NewEvents(publisher), worker.Job{Name: "outbox", Run: dispatcher.Run}, closeAll, nil
}
