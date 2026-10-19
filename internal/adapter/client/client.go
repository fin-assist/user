// Package client dials other services (grpc-api.md 9): round_robin over every address the
// target resolves to, retries only on UNAVAILABLE for the methods that allow it, the
// service token and x-request-id on every call, a default deadline for calls without one.
//
// A generated client wraps the connection: statementdatav1.NewAssistantToolsServiceClient(conn).
// The usecase declares the interface it needs and an adapter here implements it.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/user/internal/reqctx"
)

// TokenSource gives the ServiceAccount token of this service.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// Options configures Dial.
type Options struct {
	Peer   string // name of the called service, the metric label
	Tokens TokenSource
	// Retry lists methods safe to retry on UNAVAILABLE: reads and methods idempotent by
	// construction (grpc-api.md 6). Each entry is "package.Service/Method" or
	// "package.Service" for every method. Methods with idempotency-key are retried by the
	// app, not here.
	Retry []string
	// DefaultTimeout bounds a call whose context has no deadline (workers, outbox):
	// 5 s if zero. A deadline already in the context is passed on as is.
	DefaultTimeout time.Duration
	Metrics        *Metrics
	// DialOptions are extra options, for tests (a bufconn dialer).
	DialOptions []grpc.DialOption
}

// Dial connects to target: "dns:///analytics.finassist.svc.cluster.local:9090" on k3s
// (a headless service: DNS returns every pod), "dns:///127.0.0.1:9093" on the stand.
// The connection is lazy; it does not fail when the peer is down.
func Dial(target string, o Options) (*grpc.ClientConn, error) {
	if o.Tokens == nil || o.Peer == "" {
		return nil, errors.New("client: Peer and Tokens are required")
	}
	if o.DefaultTimeout <= 0 {
		o.DefaultTimeout = 5 * time.Second
	}
	sc, err := serviceConfig(o.Retry)
	if err != nil {
		return nil, err
	}
	opts := append([]grpc.DialOption{
		// Plain HTTP/2 inside the cluster: WireGuard encrypts traffic between nodes,
		// identity is the ServiceAccount token (grpc-api.md 3.3, 9).
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultServiceConfig(sc),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, PermitWithoutStream: true}),
		grpc.WithChainUnaryInterceptor(o.unary),
		grpc.WithChainStreamInterceptor(o.stream),
	}, o.DialOptions...)
	return grpc.NewClient(target, opts...)
}

func serviceConfig(retry []string) (string, error) {
	type name struct {
		Service string `json:"service"`
		Method  string `json:"method,omitempty"`
	}
	cfg := map[string]any{"loadBalancingConfig": []any{map[string]any{"round_robin": map[string]any{}}}}
	if len(retry) > 0 {
		var names []name
		for _, r := range retry {
			svc, method, _ := strings.Cut(r, "/")
			if svc == "" || strings.Contains(method, "/") {
				return "", fmt.Errorf("client: retry entry %q is not package.Service or package.Service/Method", r)
			}
			names = append(names, name{Service: svc, Method: method})
		}
		cfg["methodConfig"] = []any{map[string]any{
			"name": names,
			// grpc-go adds jitter to the backoff; hedging is not used (grpc-api.md 9).
			"retryPolicy": map[string]any{
				"maxAttempts":          3,
				"initialBackoff":       "0.1s",
				"maxBackoff":           "1s",
				"backoffMultiplier":    2,
				"retryableStatusCodes": []string{"UNAVAILABLE"},
			},
		}}
	}
	b, err := json.Marshal(cfg)
	return string(b), err
}

// outgoing adds the service token and x-request-id. It never forwards the user's
// authorization: a call from this service is a system call with user_id in the request
// (grpc-api.md 3.2). Client hints (x-client-ip, ...) stay with bank-bff and are not passed on.
func (o Options) outgoing(ctx context.Context) (context.Context, error) {
	token, err := o.Tokens.Token(ctx)
	if err != nil {
		return ctx, fmt.Errorf("service token: %w", err)
	}
	kv := []string{"x-service-authorization", "Bearer " + token}
	if id := reqctx.RequestID(ctx); id != "" {
		kv = append(kv, "x-request-id", id)
	}
	// A fresh outgoing context: nothing from an earlier outgoing metadata leaks through.
	return metadata.NewOutgoingContext(ctx, metadata.Pairs(kv...)), nil
}

func (o Options) unary(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	ctx, cancel := o.deadline(ctx)
	defer cancel()
	ctx, err := o.outgoing(ctx)
	if err != nil {
		return err
	}
	start := time.Now()
	err = invoker(ctx, method, req, reply, cc, opts...)
	o.Metrics.observe(o.Peer, method, err, start)
	return err
}

func (o Options) stream(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	// Streams have their own deadlines (grpc-api.md 9): no default here.
	ctx, err := o.outgoing(ctx)
	if err != nil {
		return nil, err
	}
	return streamer(ctx, desc, cc, method, opts...)
}

func (o Options) deadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, o.DefaultTimeout)
}

// Metrics are the RED metrics of outgoing calls, shared by all clients of the process.
type Metrics struct {
	handled  *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

// NewMetrics registers the client metrics.
func NewMetrics(reg prometheus.Registerer) (*Metrics, error) {
	m := &Metrics{
		handled: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "grpc_client_handled_total",
			Help: "Completed outgoing gRPC calls by peer, method and status code.",
		}, []string{"peer", "method", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "grpc_client_handling_seconds",
			Help:    "Duration of outgoing gRPC calls by peer and method.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		}, []string{"peer", "method"}),
	}
	for _, c := range []prometheus.Collector{m.handled, m.duration} {
		if err := reg.Register(c); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func (m *Metrics) observe(peer, method string, err error, start time.Time) {
	if m == nil {
		return
	}
	m.handled.WithLabelValues(peer, method, status.Code(err).String()).Inc()
	m.duration.WithLabelValues(peer, method).Observe(time.Since(start).Seconds())
}
