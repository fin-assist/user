package apitest_test

import (
	"context"
	"testing"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"github.com/fin-assist/go-grpc/reqctx"
	"google.golang.org/grpc/metadata"

	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/verification"
)

type capturedRequest struct {
	id, caller, ip, version, user string
	readPrimary                   bool
}

type contextCreationProvider struct {
	creationProvider
	requests chan capturedRequest
}

func (p *contextCreationProvider) Create(ctx context.Context, a verification.Attempt) (verification.Call, error) {
	user, _ := reqctx.UserID(ctx)
	p.requests <- capturedRequest{reqctx.RequestID(ctx), reqctx.Caller(ctx), reqctx.ClientIP(ctx), reqctx.ClientVersion(ctx), user, reqctx.ReadPrimary(ctx)}
	return p.creationProvider.Create(ctx, a)
}

func TestAPISharedContextReachesPhoneProviderWithoutInventingUserIdentity(t *testing.T) {
	p := &contextCreationProvider{requests: make(chan capturedRequest, 1)}
	env := apitest.StartWithPhone(t, p, creationSelector{})
	p.env = env
	seedDocuments(t, env)
	const requestID = "019c7714-3b77-74d1-9866-e1f484aae2ab"
	ctx := metadata.AppendToOutgoingContext(authContext(t.Context(), authKey()),
		"x-request-id", requestID, "x-read-primary", "1", "x-client-version", "0.2.12")
	reply, err := userv1.NewAuthServiceClient(env.Conn).StartPhoneVerification(ctx, &userv1.StartPhoneVerificationRequest{Phone: "+79001234567", PdConsentAccepted: true, PdConsentVersion: "v1", AgeConfirmed: true})
	if err != nil || reply.GetVerificationToken() == "" || reply.GetCallbackNumber() != "+74957654321" {
		t.Fatalf("phone start: %v %v", reply, err)
	}
	select {
	case got := <-p.requests:
		if got.id != requestID || got.caller != "bank-bff" || got.ip == "" || got.version != "0.2.12" || got.user != "" || !got.readPrimary {
			t.Fatalf("lost or fabricated request identity: %+v", got)
		}
	default:
		t.Fatal("provider was not called")
	}
}
