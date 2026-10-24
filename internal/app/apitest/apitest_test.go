package apitest

import (
	"errors"
	"testing"

	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// The template registers no service of its own, so the API test of the template is the
// assembly itself: it starts, is healthy and refuses what it does not serve.
func TestAPIServiceStartsAndServes(t *testing.T) {
	env := Start(t)
	resp, err := healthpb.NewHealthClient(env.Conn).Check(t.Context(), &healthpb.HealthCheckRequest{})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health: %v %v", resp, err)
	}
	err = env.Conn.Invoke(User(t.Context(), "u1"), "/finassist.nothing.v1.Nope/Get", &healthpb.HealthCheckRequest{}, &healthpb.HealthCheckResponse{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("unknown method: %v", err)
	}
}

func TestUnitHelpers(t *testing.T) {
	md, _ := metadata.FromOutgoingContext(User(t.Context(), "u1"))
	if md.Get("authorization")[0] != "Bearer dev:u1" || md.Get("x-service-authorization")[0] != "Bearer dev:bank-bff" {
		t.Fatalf("user md: %v", md)
	}
	md, _ = metadata.FromOutgoingContext(Caller(t.Context(), "assistant"))
	if len(md.Get("authorization")) != 0 || md.Get("x-service-authorization")[0] != "Bearer dev:assistant" {
		t.Fatalf("caller md: %v", md)
	}
	if Reason(finerr.NotFound()) != "NOT_FOUND" || Reason(errors.New("x")) != "" || Reason(nil) != "" {
		t.Fatal("Reason")
	}
}
