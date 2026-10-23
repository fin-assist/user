// Package signing serves the authenticated internal public-key RPC.
package signing

import (
	"context"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/grpc"
)

// Keys supplies only a public JWKS snapshot.
type Keys interface {
	JWKS(context.Context) ([]byte, error)
}

type handler struct {
	userv1.UnimplementedUserInternalServiceServer
	keys Keys
}

// Register installs the internal service. Consent status is added with the profile stage.
func Register(s grpc.ServiceRegistrar, keys Keys) {
	userv1.RegisterUserInternalServiceServer(s, &handler{keys: keys})
}

func (h *handler) GetSigningKeys(ctx context.Context, _ *userv1.GetSigningKeysRequest) (*userv1.GetSigningKeysResponse, error) {
	b, err := h.keys.JWKS(ctx)
	if err != nil {
		return nil, err
	}
	return &userv1.GetSigningKeysResponse{JwksJson: string(b)}, nil
}
