// Package signing serves the authenticated internal public-key RPC.
package signing

import (
	"context"

	"github.com/fin-assist/user/internal/adapter/grpc/retryinfo"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/grpc"

	"github.com/fin-assist/user/internal/profile"
)

// Keys supplies only a public JWKS snapshot.
type Keys interface {
	JWKS(context.Context) ([]byte, error)
}

// Consents checks effective assistant consent on primary.
type Consents interface {
	Status(context.Context, string) (profile.Consent, error)
}

type handler struct {
	userv1.UnimplementedUserInternalServiceServer
	keys     Keys
	consents Consents
}

// Register installs public keys and the assistant-only system consent check.
func Register(s grpc.ServiceRegistrar, keys Keys, consents Consents) {
	userv1.RegisterUserInternalServiceServer(s, &handler{keys: keys, consents: consents})
}

func (h *handler) GetAssistantConsentStatus(ctx context.Context, in *userv1.GetAssistantConsentStatusRequest) (*userv1.GetAssistantConsentStatusResponse, error) {
	c, err := h.consents.Status(ctx, in.GetUserId())
	if err != nil {
		return nil, retryinfo.Apply(err)
	}
	return &userv1.GetAssistantConsentStatusResponse{Consent: &userv1.AssistantConsent{Granted: c.Granted, Version: c.Version, CurrentVersion: c.CurrentVersion}}, nil
}

func (h *handler) GetSigningKeys(ctx context.Context, _ *userv1.GetSigningKeysRequest) (*userv1.GetSigningKeysResponse, error) {
	b, err := h.keys.JWKS(ctx)
	if err != nil {
		return nil, retryinfo.Apply(err)
	}
	return &userv1.GetSigningKeysResponse{JwksJson: string(b)}, nil
}
