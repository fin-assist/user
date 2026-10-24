package apitest_test

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	userdeletionv1 "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	usersettingsv1 "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/user/internal/app"
	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/config"
)

type deletionPeer struct {
	userdeletionv1.UnimplementedUserDeletionServiceServer
	t           *testing.T
	unavailable atomic.Bool
	calls       atomic.Int32
	block       <-chan struct{}
	started     chan struct{}
	applied     chan string
	mu          sync.Mutex
	deleted     map[string]bool
}

func (p *deletionPeer) PurgeUserData(ctx context.Context, in *userdeletionv1.PurgeUserDataRequest) (*userdeletionv1.PurgeUserDataResponse, error) {
	p.calls.Add(1)
	md, _ := metadata.FromIncomingContext(ctx)
	if got := md.Get("x-service-authorization"); len(got) != 1 || got[0] != "Bearer dev:user" {
		p.t.Error("missing system identity")
	}
	for _, k := range []string{"authorization", "idempotency-key", "x-client-ip", "x-client-version"} {
		if len(md.Get(k)) != 0 {
			p.t.Errorf("forwarded %s", k)
		}
	}
	if d, ok := ctx.Deadline(); !ok || time.Until(d) > 31*time.Second {
		p.t.Error("unbounded purge")
	}
	select {
	case p.started <- struct{}{}:
	default:
	}
	if p.unavailable.Load() {
		return nil, status.Error(codes.Unavailable, "temporarily unavailable")
	}
	if p.block != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-p.block:
		}
	}
	p.mu.Lock()
	first := !p.deleted[in.GetUserId()]
	p.deleted[in.GetUserId()] = true
	p.mu.Unlock()
	if first {
		p.applied <- in.GetUserId()
	}
	return &userdeletionv1.PurgeUserDataResponse{}, nil
}
func serveDeletionPeer(t *testing.T, p *deletionPeer, timezone ...*timezonePeer) string {
	t.Helper()
	p.t = t
	p.started = make(chan struct{}, 32)
	p.applied = make(chan string, 16)
	p.deleted = map[string]bool{}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	userdeletionv1.RegisterUserDeletionServiceServer(s, p)
	for _, settings := range timezone {
		usersettingsv1.RegisterUserSettingsSyncServiceServer(s, settings)
	}
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(func() { s.Stop(); _ = lis.Close() })
	return "passthrough:///" + lis.Addr().String()
}
func receiveDeletion(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case id := <-ch:
		return id
	case <-time.After(8 * time.Second):
		t.Fatal("purge not delivered")
		return ""
	}
}
func TestAPIDeletionOutboxIndependentTargetsAndProcessRestart(t *testing.T) {
	blocked := make(chan struct{})
	fast, flaky, slow := &deletionPeer{}, &deletionPeer{}, &deletionPeer{block: blocked}
	flaky.unavailable.Store(true)
	peers := map[string]string{"bank-statement-data": serveDeletionPeer(t, fast), "analytics": serveDeletionPeer(t, flaky), "assistant": serveDeletionPeer(t, slow)}
	env := apitest.Start(t, func(c *config.Config) { c.Peers = peers })
	seedDocuments(t, env)
	r, _ := registerAPI(t, env, "outbox-delete", "+79001234567")
	ctx := metadata.AppendToOutgoingContext(apitest.User(t.Context(), r.GetUserId()), "x-client-ip", "203.0.113.3", "x-client-version", "test", "idempotency-key", "private")
	if _, err := userv1.NewProfileServiceClient(env.Conn).DeleteAccount(ctx, &userv1.DeleteAccountRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE outbox_events SET created_at=$1`, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	stop := runOutbox(t, env)
	if got := receiveDeletion(t, fast.applied); got != r.GetUserId() {
		t.Fatal(got)
	}
	select {
	case <-slow.started:
	case <-time.After(5 * time.Second):
		t.Fatal("slow target never started")
	}
	eventually(t, func() bool {
		var pending, fastRows, failures int
		err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*),count(*) FILTER(WHERE target='bank-statement-data'),count(*) FILTER(WHERE target='analytics' AND attempts>0) FROM outbox_events`).Scan(&pending, &fastRows, &failures)
		return err == nil && pending == 2 && fastRows == 0 && failures == 1
	})
	eventually(t, func() bool {
		families, err := env.Registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, family := range families {
			if family.GetName() != "finassist_outbox_oldest_age_seconds" {
				continue
			}
			for _, m := range family.Metric {
				for _, label := range m.Label {
					if label.GetName() != "target" {
						t.Fatal("unbounded outbox label")
					}
					if label.GetValue() == "analytics" && m.GetGauge().GetValue() > 3600 {
						return true
					}
				}
			}
		}
		return false
	})
	fastCalls := fast.calls.Load()
	stop()
	cfg := env.App.Cfg
	env.App.Close()
	flaky.unavailable.Store(false)
	close(blocked)
	// Rebuild pools, clients and dispatcher from durable rows, as a new process.
	restarted, err := app.New(t.Context(), cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	stopAgain := runOutbox(t, &apitest.Env{App: restarted})
	defer stopAgain()
	for _, p := range []*deletionPeer{flaky, slow} {
		if got := receiveDeletion(t, p.applied); got != r.GetUserId() {
			t.Fatal(got)
		}
	}
	eventually(t, func() bool {
		var n int
		err := restarted.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM outbox_events`).Scan(&n)
		return err == nil && n == 0
	})
	if fast.calls.Load() != fastCalls {
		t.Fatal("acknowledged recipient redelivered after restart")
	}
}

func TestAPIDeleteAccountDuringInflightTimezoneDelivery(t *testing.T) {
	blocked := make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(blocked) }) }
	defer unblock()
	settings := &timezonePeer{t: t, block: blocked, started: make(chan *usersettingsv1.SetUserTimezoneRequest, 16), applied: make(chan *usersettingsv1.SetUserTimezoneRequest, 16)}
	bank, analytics, assistant := &deletionPeer{}, &deletionPeer{}, &deletionPeer{}
	peers := map[string]string{"bank-statement-data": serveDeletionPeer(t, bank), "analytics": serveDeletionPeer(t, analytics), "assistant": serveDeletionPeer(t, assistant, settings)}
	env := apitest.Start(t, func(c *config.Config) { c.Peers = peers; c.Outbox.TimezoneTargets = []string{"assistant"} })
	seedDocuments(t, env)
	r, _ := registerAPI(t, env, "inflight-timezone", "+79001234567")
	stop := runOutbox(t, env)
	defer stop()
	started := receiveTimezone(t, settings.started)
	if started.GetUserId() != r.GetUserId() {
		t.Fatal(started)
	}
	client := userv1.NewProfileServiceClient(env.Conn)
	// The shared dispatcher releases row locks before the external RPC, retaining
	// only its own session advisory lock. Deletion must finish while it is blocked.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := client.DeleteAccount(apitest.User(ctx, r.GetUserId()), &userv1.DeleteAccountRequest{}); err != nil {
		t.Fatal("slow timezone delayed deletion", err)
	}
	var users, keys, verifications, sessions, consents, tombstones, timezone int
	err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM app_user),(SELECT count(*) FROM idempotency_keys WHERE owner_id=$1),(SELECT count(*) FROM phone_verification),(SELECT count(*) FROM auth_session),(SELECT count(*) FROM user_consent),(SELECT count(*) FROM tombstone_users),(SELECT count(*) FROM outbox_events WHERE topic='user.timezone_changed')`, r.GetUserId()).Scan(&users, &keys, &verifications, &sessions, &consents, &tombstones, &timezone)
	if err != nil || users+keys+verifications+sessions+consents+timezone != 0 || tombstones != 1 {
		t.Fatal("partial deletion", users, keys, verifications, sessions, consents, tombstones, timezone, err)
	}
	for _, peer := range []*deletionPeer{bank, analytics} {
		if got := receiveDeletion(t, peer.applied); got != r.GetUserId() {
			t.Fatal(got)
		}
	}
	select {
	case <-settings.applied:
		t.Fatal("timezone was not held during deletion")
	default:
	}
	if assistant.calls.Load() != 0 {
		t.Fatal("blocked target unexpectedly acknowledged purge")
	}
	// The old in-flight call may finish after local deletion. Its final ack must
	// not recreate the removed row or consume the separately queued deletion event.
	unblock()
	late := receiveTimezone(t, settings.applied)
	if late.GetUserId() != r.GetUserId() {
		t.Fatal(late)
	}
	if got := receiveDeletion(t, assistant.applied); got != r.GetUserId() {
		t.Fatal(got)
	}
	eventually(t, func() bool {
		var n int
		err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM outbox_events`).Scan(&n)
		return err == nil && n == 0
	})
	for _, peer := range []*deletionPeer{bank, analytics, assistant} {
		if peer.calls.Load() != 1 {
			t.Fatal("duplicate or lost deletion", peer.calls.Load())
		}
	}
}
