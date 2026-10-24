package server

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
)

type unitListener struct{ closed bool }

func (l *unitListener) Accept() (net.Conn, error) { return nil, errors.New("listener unavailable") }
func (l *unitListener) Close() error              { l.closed = true; return nil }
func (l *unitListener) Addr() net.Addr            { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1} }

func TestUnitServerLifecyclePropagatesListenerFailureAndStopsHealth(t *testing.T) {
	e := unitServer(t)
	if err := e.srv.Check(); err != nil {
		t.Fatal(err)
	}
	listener := &unitListener{}
	if err := e.srv.Serve(listener); err == nil {
		t.Fatal("listener error hidden")
	}
	if !listener.closed {
		t.Fatal("failed listener not closed")
	}
	e.srv.SetServing(true)
	e.srv.Shutdown(t.Context(), 0)
	e.srv.SetServing(true)
	status, err := e.srv.health.Check(t.Context(), &healthpb.HealthCheckRequest{})
	if err != nil || status.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatal("shutdown reported healthy", status, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	e.srv.Shutdown(ctx, time.Hour) // An expired owner deadline must cut the configured delay.
}

func TestUnitServerPolicyCoversRegisteredMethodsAndMetricsStayExclusive(t *testing.T) {
	d := testDeps(t)
	reg := prometheus.NewRegistry()
	d.Metrics = reg
	s, err := New(Options{MaxRecvMsgSize: 1024}, d)
	if err != nil {
		t.Fatal(err)
	}
	defer s.GRPC().Stop()
	if _, err := New(Options{}, d); err == nil {
		t.Fatal("duplicate registry accepted")
	}
	testgrpc.RegisterTestServiceServer(s.GRPC(), &testService{})
	if err := s.Check(); err != nil {
		t.Fatal(err)
	}
	// A registered service must be fully covered by the table, not only one method.
	s.deps.Policy = Policy{emptyCall: {Callers: []string{"user"}, Model: ModelSystem}}
	if err := s.Check(); err == nil {
		t.Fatal("partial access table accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s.Shutdown(ctx, time.Hour)
	if err := (Policy{"/grpc.testing.TestService/*": {Callers: []string{"user"}, Model: ModelSystem}}).validate(grpc.NewServer()); err != nil {
		t.Fatal(err)
	}
}
