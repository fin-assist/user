package client

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/fin-assist/user/internal/reqctx"
)

type peer struct {
	testgrpc.UnimplementedTestServiceServer
	failures    atomic.Int32 // UNAVAILABLE answers before success
	calls       atomic.Int32
	md          chan metadata.MD
	hasDeadline chan bool
}

func (p *peer) handle(ctx context.Context) error {
	p.calls.Add(1)
	md, _ := metadata.FromIncomingContext(ctx)
	_, ok := ctx.Deadline()
	select {
	case p.md <- md:
		p.hasDeadline <- ok
	default:
	}
	if p.failures.Add(-1) >= 0 {
		return status.Error(codes.Unavailable, "pod is restarting")
	}
	return nil
}

func (p *peer) EmptyCall(ctx context.Context, _ *testgrpc.Empty) (*testgrpc.Empty, error) {
	return &testgrpc.Empty{}, p.handle(ctx)
}

func (p *peer) UnaryCall(ctx context.Context, _ *testgrpc.SimpleRequest) (*testgrpc.SimpleResponse, error) {
	return &testgrpc.SimpleResponse{}, p.handle(ctx)
}

func dial(t *testing.T, p *peer) (testgrpc.TestServiceClient, *Metrics) {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	testgrpc.RegisterTestServiceServer(srv, p)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	m, err := NewMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := Dial("passthrough:///peer", Options{
		Peer:    "analytics",
		Tokens:  DevToken("assistant"),
		Retry:   []string{"grpc.testing.TestService/EmptyCall"},
		Metrics: m,
		DialOptions: []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return testgrpc.NewTestServiceClient(conn), m
}

func newPeer() *peer { return &peer{md: make(chan metadata.MD, 1), hasDeadline: make(chan bool, 1)} }

func TestRetriesOnlyListedMethodsOnUnavailable(t *testing.T) {
	p := newPeer()
	c, _ := dial(t, p)

	p.failures.Store(2)
	if _, err := c.EmptyCall(t.Context(), &testgrpc.Empty{}); err != nil {
		t.Fatalf("listed method must be retried: %v", err)
	}
	if n := p.calls.Load(); n != 3 {
		t.Fatalf("%d attempts, want 3", n)
	}

	p.calls.Store(0)
	p.failures.Store(1)
	if _, err := c.UnaryCall(t.Context(), &testgrpc.SimpleRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("unlisted method must not be retried: %v", err)
	}
	if n := p.calls.Load(); n != 1 {
		t.Fatalf("%d attempts, want 1", n)
	}
}

func TestMetadataAndDefaultDeadline(t *testing.T) {
	p := newPeer()
	c, m := dial(t, p)

	// An incoming user call: its authorization and outgoing metadata must not travel on.
	ctx := reqctx.WithRequestID(context.Background(), "req-1")
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer user-token")
	if _, err := c.UnaryCall(ctx, &testgrpc.SimpleRequest{}); err != nil {
		t.Fatal(err)
	}
	md := <-p.md
	if got := md.Get("x-service-authorization"); len(got) != 1 || got[0] != "Bearer dev:assistant" {
		t.Fatalf("service token: %v", got)
	}
	if got := md.Get("x-request-id"); len(got) != 1 || got[0] != "req-1" {
		t.Fatalf("request id: %v", got)
	}
	if len(md.Get("authorization")) != 0 {
		t.Fatal("the user token must not be forwarded")
	}
	if !<-p.hasDeadline {
		t.Fatal("a call without a deadline must get the default one")
	}
	if n := testutil.ToFloat64(m.handled.WithLabelValues("analytics", "/grpc.testing.TestService/UnaryCall", "OK")); n != 1 {
		t.Fatalf("metric %v", n)
	}
}

func TestServiceConfigRejectsBadRetryEntries(t *testing.T) {
	for _, bad := range []string{"", "/Method", "a.B/C/D"} {
		if _, err := serviceConfig([]string{bad}); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
	}
}

func TestFileTokenRereadsAfterRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &FileToken{Path: path}
	if tok, err := f.Token(t.Context()); err != nil || tok != "one" {
		t.Fatalf("%q %v", tok, err)
	}
	// kubelet replaces the file; mtime and usually size change.
	if err := os.WriteFile(path, []byte("rotated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(path, time.Now().Add(time.Minute), time.Now().Add(time.Minute))
	if tok, err := f.Token(t.Context()); err != nil || tok != "rotated" {
		t.Fatalf("%q %v", tok, err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(path, time.Now().Add(2*time.Minute), time.Now().Add(2*time.Minute))
	if _, err := f.Token(t.Context()); err == nil {
		t.Fatal("an empty token file must be an error")
	}
}
