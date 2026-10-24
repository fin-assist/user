package apitest_test

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	"github.com/fin-assist/user/internal/app"
	"github.com/fin-assist/user/internal/app/apitest"
)

func secondStartInstance(t *testing.T, first *apitest.Env, provider *creationProvider) *apitest.Env {
	t.Helper()
	reg := prometheus.NewRegistry()
	a, err := app.New(t.Context(), first.App.Cfg, slog.New(slog.DiscardHandler), reg, app.WithPhoneBackend(provider, creationSelector{}))
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	go func() { _ = a.GRPC.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///second", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		a.GRPC.Shutdown(ctx, 0)
		a.Close()
	})
	return &apitest.Env{App: a, Conn: conn, DSN: first.DSN, Registry: reg}
}
