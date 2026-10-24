package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/user/internal/logx"
	"github.com/fin-assist/user/internal/reqctx"
)

// unitServer constructs interceptors and their collaborators without a listener or transport.
func unitServer(t *testing.T) *env {
	t.Helper()
	logs := &bytes.Buffer{}
	reg := prometheus.NewRegistry()
	s, err := New(Options{Reflection: true}, Deps{
		Logger: logx.New(logs, "json", slog.LevelDebug, "test"), Metrics: reg, Policy: policy(),
		Services: DevServiceVerifier{}, Users: DevUserVerifier{}, Tombstones: deleted{"gone": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.GRPC().Stop)
	return &env{srv: s, logs: logs, reg: reg}
}

func incoming(caller, user string) context.Context {
	md := metadata.MD{}
	if caller != "" {
		md.Set(mdServiceAuthorization, "Bearer "+caller)
	}
	if user != "" {
		md.Set(mdAuthorization, "Bearer "+user)
	}
	return metadata.NewIncomingContext(context.Background(), md)
}

func TestUnitAuthorizationRefusesBeforeHandlerAndPreservesIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, method, caller, user string
		code                       codes.Code
		reason, wantUser           string
	}{
		{"missing service", emptyCall, "", "dev:u1", codes.Unauthenticated, "", ""},
		{"bad service", emptyCall, "bad", "dev:u1", codes.Unauthenticated, "", ""},
		{"foreign caller", emptyCall, "dev:analytics", "dev:u1", codes.PermissionDenied, "", ""},
		{"missing user", emptyCall, "dev:bank-bff", "", codes.Unauthenticated, "INVALID_TOKEN", ""},
		{"bad user", emptyCall, "dev:bank-bff", "bad", codes.Unauthenticated, "INVALID_TOKEN", ""},
		{"deleted", emptyCall, "dev:bank-bff", "dev:gone", codes.Unauthenticated, "TOKEN_REVOKED", ""},
		{"tombstone unavailable", emptyCall, "dev:bank-bff", "dev:broken", codes.Unavailable, "", ""},
		{"user", emptyCall, "dev:bank-bff", "dev:u1", codes.OK, "", "u1"},
		{"mixed system", unaryCall, "dev:assistant", "", codes.OK, "", ""},
		{"system ignores user", streamOut, "dev:user", "bad", codes.OK, "", ""},
		{"health open", "/grpc.health.v1.Health/Check", "", "", codes.OK, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := unitServer(t)
			called := false
			_, err := e.srv.unary(incoming(tc.caller, tc.user), nil, &grpc.UnaryServerInfo{FullMethod: tc.method}, func(ctx context.Context, _ any) (any, error) {
				called = true
				id, _ := reqctx.UserID(ctx)
				if id != tc.wantUser {
					t.Fatalf("identity %q, want %q", id, tc.wantUser)
				}
				if reqctx.RequestID(ctx) == "" {
					t.Fatal("missing generated request ID")
				}
				return "accepted", nil
			})
			if status.Code(err) != tc.code || Reason(err) != tc.reason || called != (tc.code == codes.OK) {
				t.Fatalf("called=%v error=%v", called, err)
			}
		})
	}
}

func TestUnitInterceptorsValidateNormalizeAndRecoverWithoutTransport(t *testing.T) {
	e := unitServer(t)
	ctx := metadata.NewIncomingContext(incoming("dev:bank-bff", "dev:u1"), metadata.Pairs(mdServiceAuthorization, "Bearer dev:bank-bff", mdAuthorization, "Bearer dev:u1", mdRequestID, "request-1", mdClientIP, "192.0.2.1", mdClientVersion, "0.2", mdReadPrimary, "1"))
	called := false
	_, err := e.srv.unary(ctx, validationMessage(t), &grpc.UnaryServerInfo{FullMethod: emptyCall}, func(context.Context, any) (any, error) { called = true; return nil, nil })
	if called || Reason(err) != "VALIDATION_ERROR" {
		t.Fatalf("invalid input reached handler: %v %v", called, err)
	}
	for _, cause := range []error{errors.New("private backend failure"), finerr.NotFound(), context.Canceled} {
		_, err = e.srv.unary(ctx, nil, &grpc.UnaryServerInfo{FullMethod: emptyCall}, func(ctx context.Context, _ any) (any, error) {
			if reqctx.ClientIP(ctx) != "192.0.2.1" || reqctx.ClientVersion(ctx) != "0.2" || !reqctx.ReadPrimary(ctx) || reqctx.RequestID(ctx) != "request-1" {
				t.Fatal("trusted hints lost")
			}
			return nil, cause
		})
		if status.Code(err) != status.Code(normalize(cause)) {
			t.Fatalf("normalization: %v", err)
		}
	}
	_, err = e.srv.unary(ctx, nil, &grpc.UnaryServerInfo{FullMethod: emptyCall}, func(context.Context, any) (any, error) { panic("handler panic") })
	if status.Code(err) != codes.Internal || status.Convert(err).Message() != "Internal" {
		t.Fatalf("panic leaked: %v", err)
	}
}

type unitStream struct {
	grpc.ServerStream
	ctx        context.Context
	receiveErr error
}

func (s unitStream) Context() context.Context { return s.ctx }
func (s unitStream) RecvMsg(any) error        { return s.receiveErr }

func TestUnitStreamValidatesEveryMessageAndRefusesMissingIdentity(t *testing.T) {
	e := unitServer(t)
	for _, receiveErr := range []error{nil, errors.New("transport closed")} {
		err := e.srv.stream(nil, unitStream{ctx: incoming("dev:user", ""), receiveErr: receiveErr}, &grpc.StreamServerInfo{FullMethod: streamOut}, func(_ any, ss grpc.ServerStream) error {
			if reqctx.Caller(ss.Context()) != "user" || reqctx.RequestID(ss.Context()) == "" {
				t.Fatal("stream context missing")
			}
			return ss.RecvMsg(validationMessage(t))
		})
		if receiveErr == nil && Reason(err) != "VALIDATION_ERROR" || receiveErr != nil && status.Code(err) != codes.Internal {
			t.Fatalf("stream returned %v", err)
		}
	}
	called := false
	err := e.srv.stream(nil, unitStream{ctx: incoming("", "")}, &grpc.StreamServerInfo{FullMethod: streamOut}, func(any, grpc.ServerStream) error { called = true; return nil })
	if called || status.Code(err) != codes.Unauthenticated {
		t.Fatalf("unauthorized stream reached handler: %v %v", called, err)
	}
}
