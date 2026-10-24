package client

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/test/bufconn"
)

func TestIntegrationProjectedTokenRotationReachesPeerWithoutReconnecting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projected-token")
	if err := os.WriteFile(path, []byte("first-service-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := newPeer()
	lis := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	testgrpc.RegisterTestServiceServer(server, p)
	go func() { _ = server.Serve(lis) }()
	t.Cleanup(server.Stop)
	metrics, err := NewMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := Dial("passthrough:///rotation", Options{Peer: "analytics", Tokens: &FileToken{Path: path}, Metrics: metrics, DialOptions: []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) })}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	c := testgrpc.NewTestServiceClient(conn)
	for _, token := range []string{"first-service-token", "rotated-longer-service-token"} {
		if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if _, err := c.UnaryCall(t.Context(), &testgrpc.SimpleRequest{}); err != nil {
				t.Fatal(err)
			}
			md := <-p.md
			<-p.hasDeadline
			if got := md.Get("x-service-authorization"); len(got) != 1 || got[0] != "Bearer "+token {
				t.Fatal(got)
			}
		}
	}
	before := p.calls.Load()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UnaryCall(t.Context(), &testgrpc.SimpleRequest{}); err == nil {
		t.Fatal("missing projected token reused")
	}
	if p.calls.Load() != before {
		t.Fatal("unauthenticated request reached peer")
	}
}
