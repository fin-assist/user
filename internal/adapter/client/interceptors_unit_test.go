package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/user/internal/reqctx"
)

type unitTokenSource struct{ err error }

func (s unitTokenSource) Token(context.Context) (string, error) { return "system-token", s.err }

func TestUnitClientInterceptorsReplaceSensitiveMetadataAndPreserveDeadlines(t *testing.T) {
	metrics, err := NewMetrics(prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	options := Options{Peer: "assistant", Tokens: unitTokenSource{}, Metrics: metrics, DefaultTimeout: time.Minute}
	inherited := metadata.NewOutgoingContext(reqctx.WithRequestID(t.Context(), "request-1"), metadata.Pairs("authorization", "Bearer private-user-token", "x-client-ip", "192.0.2.1"))
	for _, withDeadline := range []bool{false, true} {
		ctx := inherited
		var cancel context.CancelFunc
		var wantDeadline time.Time
		if withDeadline {
			ctx, cancel = context.WithTimeout(ctx, time.Second)
			defer cancel()
			wantDeadline, _ = ctx.Deadline()
		}
		called := false
		err := options.unary(ctx, "/service/Read", nil, nil, nil, func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
			called = true
			md, _ := metadata.FromOutgoingContext(ctx)
			if md.Get("x-service-authorization")[0] != "Bearer system-token" || md.Get("x-request-id")[0] != "request-1" || len(md.Get("authorization")) != 0 || len(md.Get("x-client-ip")) != 0 {
				t.Fatalf("metadata leaked or lost: %v", md)
			}
			deadline, ok := ctx.Deadline()
			if !ok || withDeadline && !deadline.Equal(wantDeadline) {
				t.Fatal("deadline changed", deadline, wantDeadline)
			}
			return status.Error(codes.Unavailable, "peer unavailable")
		})
		if !called || status.Code(err) != codes.Unavailable {
			t.Fatal("invoker result lost", called, err)
		}
	}
	if got := testutil.ToFloat64(metrics.handled.WithLabelValues("assistant", "/service/Read", "Unavailable")); got != 2 {
		t.Fatal("completion metric", got)
	}
	called := false
	stream, err := options.stream(inherited, &grpc.StreamDesc{}, nil, "/service/Watch", func(ctx context.Context, _ *grpc.StreamDesc, _ *grpc.ClientConn, _ string, _ ...grpc.CallOption) (grpc.ClientStream, error) {
		called = true
		if _, ok := ctx.Deadline(); ok {
			t.Fatal("default deadline applied to stream")
		}
		return nil, errors.New("stream rejected")
	})
	if !called || stream != nil || err == nil {
		t.Fatal(stream, called, err)
	}
	options.Tokens = unitTokenSource{err: errors.New("key unavailable")}
	called = false
	if err := options.unary(t.Context(), "/service/Read", nil, nil, nil, func(context.Context, string, any, any, *grpc.ClientConn, ...grpc.CallOption) error {
		called = true
		return nil
	}); err == nil || called {
		t.Fatal("missing token invoked peer", err)
	}
	if _, err := options.stream(t.Context(), nil, nil, "/service/Watch", func(context.Context, *grpc.StreamDesc, *grpc.ClientConn, string, ...grpc.CallOption) (grpc.ClientStream, error) {
		called = true
		return nil, nil
	}); err == nil || called {
		t.Fatal("missing token opened stream", err)
	}
}

func TestUnitLazyClientConfigurationAndMetricRegistrationFailures(t *testing.T) {
	conn, err := Dial("passthrough:///127.0.0.1:1", Options{Peer: "assistant", Tokens: unitTokenSource{}, Retry: []string{"service.Read/Fetch", "service.Watch"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close() // grpc.NewClient is lazy: this does not dial.
	if _, err := serviceConfig([]string{"bad/too/many"}); err == nil {
		t.Fatal("invalid retry rule accepted")
	}
	for _, name := range []string{"grpc_client_handled_total", "grpc_client_handling_seconds"} {
		reg := prometheus.NewRegistry()
		reg.MustRegister(prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: "Existing metric."}))
		if _, err := NewMetrics(reg); err == nil {
			t.Fatal("duplicate metrics accepted")
		}
	}
	var metrics *Metrics
	metrics.observe("assistant", "/service/Read", nil, time.Now())
}
