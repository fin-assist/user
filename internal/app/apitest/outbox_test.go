package apitest_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	usersettingsv1 "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/config"
)

type timezonePeer struct {
	usersettingsv1.UnimplementedUserSettingsSyncServiceServer
	t                *testing.T
	block            <-chan struct{}
	started, applied chan *usersettingsv1.SetUserTimezoneRequest
	calls            atomic.Int32
	unavailable      atomic.Bool
}

func (p *timezonePeer) SetUserTimezone(ctx context.Context, in *usersettingsv1.SetUserTimezoneRequest) (*usersettingsv1.SetUserTimezoneResponse, error) {
	p.calls.Add(1)
	md, _ := metadata.FromIncomingContext(ctx)
	if got := md.Get("x-service-authorization"); len(got) != 1 || got[0] != "Bearer dev:user" {
		p.t.Error("missing system identity")
	}
	for _, key := range []string{"authorization", "x-client-ip", "x-client-version", "idempotency-key"} {
		if len(md.Get(key)) != 0 {
			p.t.Errorf("forwarded %s", key)
		}
	}
	if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 31*time.Second {
		p.t.Error("unbounded delivery")
	}
	select {
	case p.started <- proto.Clone(in).(*usersettingsv1.SetUserTimezoneRequest):
	case <-ctx.Done():
		return nil, ctx.Err()
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
	select {
	case p.applied <- proto.Clone(in).(*usersettingsv1.SetUserTimezoneRequest):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &usersettingsv1.SetUserTimezoneResponse{}, nil
}

func TestAPITimezoneOutboxRetriesUnavailableRecipient(t *testing.T) {
	fast := &timezonePeer{t: t, started: make(chan *usersettingsv1.SetUserTimezoneRequest, 16), applied: make(chan *usersettingsv1.SetUserTimezoneRequest, 16)}
	flaky := &timezonePeer{t: t, started: make(chan *usersettingsv1.SetUserTimezoneRequest, 16), applied: make(chan *usersettingsv1.SetUserTimezoneRequest, 16)}
	flaky.unavailable.Store(true)
	fastAddr, flakyAddr := serveTimezonePeer(t, fast), serveTimezonePeer(t, flaky)
	env := apitest.Start(t, func(c *config.Config) {
		c.Peers = map[string]string{"analytics": "passthrough:///127.0.0.1:1", "bank-statement-data": fastAddr, "assistant": flakyAddr}
	})
	seedProfiles(t, env)
	client := userv1.NewProfileServiceClient(env.Conn)
	if _, err := client.UpdateProfile(apitest.User(t.Context(), profileUserA), &userv1.UpdateProfileRequest{Timezone: stringPtr("UTC")}); err != nil {
		t.Fatal(err)
	}
	stop := runOutbox(t, env)
	defer stop()
	_ = receiveTimezone(t, fast.applied)
	eventually(t, func() bool {
		var n int
		if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM outbox_events WHERE target='assistant' AND attempts>0`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	})
	flaky.unavailable.Store(false)
	m := receiveTimezone(t, flaky.applied)
	if m.GetTimezone() != "UTC" || m.GetSettingsVersion() != 2 || m.GetUserId() != profileUserA {
		t.Fatal(m)
	}
	eventually(t, func() bool {
		var n int
		if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM outbox_events`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 0
	})
}

func serveTimezonePeer(t *testing.T, p *timezonePeer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := grpc.NewServer()
	usersettingsv1.RegisterUserSettingsSyncServiceServer(s, p)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(func() { s.Stop(); _ = lis.Close() })
	return "passthrough:///" + lis.Addr().String()
}

func runOutbox(t *testing.T, env *apitest.Env) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	found := false
	for _, job := range env.App.Jobs {
		if job.Name == "outbox" {
			found = true
			go func() { done <- job.Run(ctx) }()
			break
		}
	}
	if !found {
		cancel()
		t.Fatal("outbox job missing")
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("outbox did not stop")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !check() {
		select {
		case <-timer.C:
			t.Fatal("condition not reached")
		case <-tick.C:
		}
	}
}

func receiveTimezone(t *testing.T, ch <-chan *usersettingsv1.SetUserTimezoneRequest) *usersettingsv1.SetUserTimezoneRequest {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(8 * time.Second):
		t.Fatal("timezone was not delivered")
		return nil
	}
}

func TestAPITimezoneOutboxIndependentRecipientsRestartAndMetadata(t *testing.T) {
	blocked := make(chan struct{})
	fast := &timezonePeer{t: t, started: make(chan *usersettingsv1.SetUserTimezoneRequest, 16), applied: make(chan *usersettingsv1.SetUserTimezoneRequest, 16)}
	slow := &timezonePeer{t: t, block: blocked, started: make(chan *usersettingsv1.SetUserTimezoneRequest, 16), applied: make(chan *usersettingsv1.SetUserTimezoneRequest, 16)}
	fastAddr, slowAddr := serveTimezonePeer(t, fast), serveTimezonePeer(t, slow)
	env := apitest.Start(t, func(c *config.Config) {
		c.Peers = map[string]string{"analytics": "passthrough:///127.0.0.1:1", "bank-statement-data": fastAddr, "assistant": slowAddr}
		c.Outbox.PollInterval = 20 * time.Millisecond
	})
	seedProfiles(t, env)
	client := userv1.NewProfileServiceClient(env.Conn)
	ctx := metadata.AppendToOutgoingContext(apitest.User(t.Context(), profileUserA), "x-client-ip", "192.0.2.123", "x-client-version", "test/1.0.0", "idempotency-key", "private-request-key")
	if _, err := client.UpdateProfile(ctx, &userv1.UpdateProfileRequest{Timezone: stringPtr("Asia/Tokyo")}); err != nil {
		t.Fatal(err)
	}
	stop := runOutbox(t, env)
	_ = receiveTimezone(t, slow.started)
	first := receiveTimezone(t, fast.applied)
	if first.GetUserId() != profileUserA || first.GetTimezone() != "Asia/Tokyo" || first.GetSettingsVersion() != 2 {
		t.Fatal(first)
	}
	// A blocked assistant cannot occupy producer connections or delay another target.
	if _, err := client.UpdateProfile(ctx, &userv1.UpdateProfileRequest{Timezone: stringPtr("UTC")}); err != nil {
		t.Fatal(err)
	}
	second := receiveTimezone(t, fast.applied)
	if second.GetTimezone() != "UTC" || second.GetSettingsVersion() != 3 {
		t.Fatal(second)
	}
	eventually(t, func() bool {
		var n int
		err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM outbox_events WHERE target='bank-statement-data'`).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		return n == 0
	})
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE outbox_events SET created_at=$1 WHERE target='assistant'`, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		families, err := env.Registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range families {
			if f.GetName() == "finassist_outbox_oldest_age_seconds" {
				for _, m := range f.Metric {
					if m.Label[0].GetValue() == "assistant" && m.Gauge.GetValue() > 3600 {
						return true
					}
				}
			}
		}
		return false
	})
	stop()
	var remaining int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM outbox_events WHERE target='assistant'`).Scan(&remaining); err != nil || remaining != 2 {
		t.Fatalf("lost pending delivery: %d %v", remaining, err)
	}
	close(blocked)
	stopRestarted := runOutbox(t, env)
	defer stopRestarted()
	versions := map[int32]bool{}
	for len(versions) < 2 {
		m := receiveTimezone(t, slow.applied)
		if m.GetUserId() != profileUserA {
			t.Fatal(m)
		}
		versions[m.GetSettingsVersion()] = true
	}
	if !versions[2] || !versions[3] {
		t.Fatalf("missing versions: %v", versions)
	}
	eventually(t, func() bool {
		var n int
		if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM outbox_events`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 0
	})
	if fast.calls.Load() != 2 {
		t.Fatalf("acknowledged target redelivered on restart: %d", fast.calls.Load())
	}
}
