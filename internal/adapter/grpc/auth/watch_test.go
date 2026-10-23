package auth

import (
	"context"
	"errors"
	"testing"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/grpc/metadata"

	"github.com/fin-assist/user/internal/auth"
	"github.com/fin-assist/user/internal/verification"
)

type watchFunc func(context.Context, string, func(verification.Outcome) error) error

func (f watchFunc) Watch(ctx context.Context, s string, send func(verification.Outcome) error) error {
	return f(ctx, s, send)
}

type watchStream struct {
	ctx      context.Context
	messages []*userv1.WatchPhoneVerificationResponse
	err      error
}

func (s *watchStream) Context() context.Context { return s.ctx }
func (s *watchStream) Send(m *userv1.WatchPhoneVerificationResponse) error {
	s.messages = append(s.messages, m)
	return s.err
}
func (*watchStream) SetHeader(metadata.MD) error  { return nil }
func (*watchStream) SendHeader(metadata.MD) error { return nil }
func (*watchStream) SetTrailer(metadata.MD)       {}
func (*watchStream) SendMsg(any) error            { return nil }
func (*watchStream) RecvMsg(any) error            { return nil }
func TestWatchLimitsBeforeSecretAndMessageMapping(t *testing.T) {
	for _, o := range []verification.Outcome{{State: verification.Pending}, {State: verification.Expired}, {State: verification.Verified, IsNewUser: true, RegistrationToken: "reg"}, {State: verification.Verified, Pair: auth.Pair{Access: "a", Refresh: "r"}}} {
		h, _, _, limits, _ := handlerFixture(t)
		calls := 0
		h.phone = watchFunc(func(_ context.Context, raw string, send func(verification.Outcome) error) error {
			calls++
			if limits.calls != calls || raw != "secret" {
				t.Fatal("token checked before limit")
			}
			return send(o)
		})
		stream := &watchStream{ctx: handlerContext()}
		if err := h.WatchPhoneVerification(&userv1.WatchPhoneVerificationRequest{VerificationToken: "secret"}, stream); err != nil || calls != 1 || len(stream.messages) != 1 {
			t.Fatal(err, calls)
		}
		m := stream.messages[0]
		if m.GetStatus() != userv1.VerificationStatus(o.State) {
			t.Fatal("wrong state", m)
		}
		if o.State == verification.Verified {
			if m.IsNewUser == nil || m.GetIsNewUser() != o.IsNewUser {
				t.Fatal(m)
			}
			if o.IsNewUser {
				if m.GetRegistrationToken() != "reg" || m.GetTokens() != nil {
					t.Fatal(m)
				}
			} else if m.GetTokens().GetRefreshToken() != "r" || m.RegistrationToken != nil {
				t.Fatal(m)
			}
		} else if m.IsNewUser != nil || m.GetTokens() != nil || m.RegistrationToken != nil {
			t.Fatal(m)
		}
		stream.err = errDependency
		if err := h.WatchPhoneVerification(&userv1.WatchPhoneVerificationRequest{VerificationToken: "secret"}, stream); !errors.Is(err, errDependency) {
			t.Fatal(err)
		}
	}
	h, _, _, limits, _ := handlerFixture(t)
	calls := 0
	h.phone = watchFunc(func(context.Context, string, func(verification.Outcome) error) error { calls++; return errDependency })
	limits.allowed = false
	if err := h.WatchPhoneVerification(&userv1.WatchPhoneVerificationRequest{}, &watchStream{ctx: handlerContext()}); grpcReason(err) != "RATE_LIMITED" || calls != 0 {
		t.Fatal(err, calls)
	}
	limits.allowed = true
	if err := h.WatchPhoneVerification(&userv1.WatchPhoneVerificationRequest{}, &watchStream{ctx: handlerContext()}); !errors.Is(err, errDependency) || calls != 1 {
		t.Fatal(err, calls)
	}
}
