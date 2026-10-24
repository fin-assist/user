package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	idempotency "github.com/fin-assist/go-idempotency"
	"google.golang.org/grpc/metadata"

	"github.com/fin-assist/user/internal/verification"
)

type startFunc func(context.Context, idempotency.Request, string, string) (verification.Started, error)

func (f startFunc) Start(ctx context.Context, r idempotency.Request, phone, ip string) (verification.Started, error) {
	return f(ctx, r, phone, ip)
}
func TestUnitStartMappingRecalculatesRemainingTime(t *testing.T) {
	now := time.Date(2026, 10, 23, 20, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		delta time.Duration
		want  int32
	}{{299*time.Second + 900*time.Millisecond, 299}, {500 * time.Millisecond, 0}, {-time.Minute, 0}} {
		h, _, _, limits, _ := handlerFixture(t)
		h.now = func() time.Time { return now }
		h.start = startFunc(func(_ context.Context, r idempotency.Request, phone, ip string) (verification.Started, error) {
			want, err := idempotency.HashProto(&userv1.StartPhoneVerificationRequest{Phone: phone})
			if err != nil || r.Hash != want || r.Endpoint != userv1.AuthService_StartPhoneVerification_FullMethodName || phone != "+79001234567" || ip != "203.0.113.7" {
				t.Fatal("wrong start input", r, phone, ip, err)
			}
			return verification.Started{Token: "opaque", CallbackNumber: "+74951234567", ExpiresAt: now.Add(tt.delta)}, nil
		})
		out, err := h.StartPhoneVerification(handlerContext(), &userv1.StartPhoneVerificationRequest{Phone: "+79001234567"})
		if err != nil || out.GetVerificationToken() != "opaque" || out.GetExpiresIn() != tt.want || !out.GetExpiresAt().AsTime().Equal(now.Add(tt.delta)) || limits.calls != 0 {
			t.Fatal(out, err)
		}
	}
}
func TestUnitStartExtractionAndFailure(t *testing.T) {
	h, _, _, _, _ := handlerFixture(t)
	h.now = time.Now
	calls := 0
	h.start = startFunc(func(context.Context, idempotency.Request, string, string) (verification.Started, error) {
		calls++
		return verification.Started{}, errDependency
	})
	if _, err := h.StartPhoneVerification(context.Background(), &userv1.StartPhoneVerificationRequest{}); grpcReason(err) != "VALIDATION_ERROR" || calls != 0 {
		t.Fatal(err, calls)
	}
	if _, err := h.StartPhoneVerification(handlerContext(), &userv1.StartPhoneVerificationRequest{}); !errors.Is(err, errDependency) || calls != 1 {
		t.Fatal(err, calls)
	}
	if _, err := h.StartPhoneVerification(handlerContext(), &userv1.StartPhoneVerificationRequest{Phone: string([]byte{255})}); err == nil || calls != 1 {
		t.Fatal("invalid protobuf string", err, calls)
	}
	h.start = nil
	if _, err := h.StartPhoneVerification(handlerContext(), &userv1.StartPhoneVerificationRequest{}); err == nil {
		t.Fatal("missing start usecase")
	}
	ctx := metadata.NewIncomingContext(t.Context(), metadata.Pairs("idempotency-key", "bad"))
	if _, err := h.StartPhoneVerification(ctx, &userv1.StartPhoneVerificationRequest{}); grpcReason(err) != "VALIDATION_ERROR" {
		t.Fatal(err)
	}
}
