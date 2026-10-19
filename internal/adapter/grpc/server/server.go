// Package server is the gRPC server of the service with the interceptors every call goes
// through (docs/server.md): request ID, logs with masking, RED metrics, panic recovery,
// service and user identity, the access table, tombstone, protovalidate. Handlers live in
// internal/adapter/grpc/<feature> and are registered on Server.GRPC().
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"buf.build/go/protovalidate"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
)

// Options configures the server.
type Options struct {
	MaxRecvMsgSize int  // bytes; 4 MiB by default, 16 MiB where SearchOperations is served
	Reflection     bool // dev only
}

// Deps are the collaborators of the interceptors. All are required: a data service passes
// the tombstone library (FIN-110), a service without user data passes NoTombstones.
type Deps struct {
	Logger     *slog.Logger
	Metrics    prometheus.Registerer
	Policy     Policy
	Services   ServiceVerifier
	Users      UserVerifier
	Tombstones Tombstones
}

// Server wraps grpc.Server with health and the interceptors.
type Server struct {
	grpc      *grpc.Server
	health    *health.Server
	deps      Deps
	log       *slog.Logger
	validator protovalidate.Validator

	handled  *prometheus.CounterVec
	duration *prometheus.HistogramVec
	denied   *prometheus.CounterVec
}

// Keepalive and connection age of grpc-api.md 9: MaxConnectionAge makes clients reconnect
// so new pods get connections; the grace is longer than the 3-minute answer stream.
const (
	maxConnectionAge      = 5 * time.Minute
	maxConnectionAgeGrace = 4 * time.Minute
	minClientPingInterval = 20 * time.Second
)

// New builds the server. Register handlers on GRPC() before Serve.
func New(o Options, d Deps) (*Server, error) {
	if d.Logger == nil || d.Metrics == nil || d.Policy == nil || d.Services == nil || d.Users == nil || d.Tombstones == nil {
		return nil, errors.New("grpc server: Logger, Metrics, Policy, Services, Users and Tombstones are required")
	}
	v, err := protovalidate.New()
	if err != nil {
		return nil, fmt.Errorf("protovalidate: %w", err)
	}
	s := &Server{deps: d, log: d.Logger, validator: v, health: health.NewServer()}
	if s.handled, s.duration, s.denied, err = newMetrics(d.Metrics); err != nil {
		return nil, fmt.Errorf("grpc metrics: %w", err)
	}
	if o.MaxRecvMsgSize <= 0 {
		o.MaxRecvMsgSize = 4 << 20
	}
	s.grpc = grpc.NewServer(
		grpc.ChainUnaryInterceptor(s.unary),
		grpc.ChainStreamInterceptor(s.stream),
		grpc.MaxRecvMsgSize(o.MaxRecvMsgSize),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionAge:      maxConnectionAge,
			MaxConnectionAgeGrace: maxConnectionAgeGrace,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             minClientPingInterval,
			PermitWithoutStream: true,
		}),
	)
	healthpb.RegisterHealthServer(s.grpc, s.health)
	if o.Reflection {
		reflection.Register(s.grpc)
	}
	// Not serving until Serve checked the access table and readiness says so.
	s.health.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	return s, nil
}

// GRPC is the server to register handlers on.
func (s *Server) GRPC() *grpc.Server { return s.grpc }

// SetServing sets the overall health status (service ""), which probes and the stand's
// checks read; readiness calls it from the database ping.
func (s *Server) SetServing(ok bool) {
	st := healthpb.HealthCheckResponse_NOT_SERVING
	if ok {
		st = healthpb.HealthCheckResponse_SERVING
	}
	s.health.SetServingStatus("", st)
}

// Check reports whether the access table is valid and covers every registered method.
// Serve calls it; tests call it to fail with the reason instead of a server that never
// starts.
func (s *Server) Check() error { return s.deps.Policy.validate(s.grpc) }

// Serve checks the access table (Check) and serves lis until Shutdown.
func (s *Server) Serve(lis net.Listener) error {
	if err := s.Check(); err != nil {
		return err
	}
	return s.grpc.Serve(lis)
}

// Shutdown reports NOT_SERVING, waits delay so balancers stop sending new calls, then
// stops gracefully; calls still running when ctx ends are cut. Answer streams survive
// that: their state is in the database, clients reconnect (grpc-api.md 5.2).
func (s *Server) Shutdown(ctx context.Context, delay time.Duration) {
	s.health.Shutdown()
	select {
	case <-time.After(delay):
	case <-ctx.Done():
	}
	done := make(chan struct{})
	go func() {
		s.grpc.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.grpc.Stop()
		<-done
	}
}
