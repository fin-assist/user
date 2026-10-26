// Package consent adapts the consent-document RPC to its usecase.
package consent

import (
	"context"
	"strings"

	"github.com/fin-assist/user/internal/adapter/grpc/retryinfo"
	"github.com/fin-assist/user/internal/domain"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/fin-assist/user/internal/consent"
)

// Documents is the document usecase consumed by this handler.
type Documents interface {
	Current(context.Context, string) (consent.Document, error)
}

// UserVerifier verifies the conditional access token of an assistant document.
type UserVerifier interface {
	VerifyUser(context.Context, string) (string, error)
}

// DeletedUsers checks primary-backed tombstones after user identity verification.
type DeletedUsers interface {
	Deleted(context.Context, string) (bool, error)
}

type handler struct {
	userv1.UnimplementedConsentDocumentServiceServer
	documents Documents
	users     UserVerifier
	deleted   DeletedUsers
}

// Register installs the handler; its service caller is checked by the server ACL.
func Register(s grpc.ServiceRegistrar, documents Documents, users UserVerifier, deleted DeletedUsers) {
	userv1.RegisterConsentDocumentServiceServer(s, &handler{documents: documents, users: users, deleted: deleted})
}

func (h *handler) GetConsentDocument(ctx context.Context, req *userv1.GetConsentDocumentRequest) (*userv1.GetConsentDocumentResponse, error) {
	var kind string
	switch req.GetType() {
	case userv1.ConsentType_CONSENT_TYPE_PERSONAL_DATA:
		kind = "personal_data"
	case userv1.ConsentType_CONSENT_TYPE_ASSISTANT:
		kind = "assistant"
		if err := h.authorize(ctx); err != nil {
			return nil, retryinfo.Apply(err)
		}
	default:
		return nil, retryinfo.Apply(domain.WithRequestRetry(finerr.NotFound(), false))
	}
	d, err := h.documents.Current(ctx, kind)
	if err != nil {
		return nil, retryinfo.Apply(err)
	}
	return &userv1.GetConsentDocumentResponse{Document: &userv1.ConsentDocument{Type: req.GetType(), Version: d.Version, Title: d.Title, Url: d.URL}}, nil
}

func (h *handler) authorize(ctx context.Context) error {
	values := metadata.ValueFromIncomingContext(ctx, "authorization")
	if len(values) != 1 {
		return retryinfo.Apply(domain.WithRequestRetry(finerr.InvalidToken(), false))
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return retryinfo.Apply(domain.WithRequestRetry(finerr.InvalidToken(), false))
	}
	id, err := h.users.VerifyUser(ctx, token)
	if err != nil {
		return err
	}
	if id == "" {
		return retryinfo.Apply(domain.WithRequestRetry(finerr.InvalidToken(), false))
	}
	deleted, err := h.deleted.Deleted(ctx, id)
	if err != nil {
		return err
	}
	if deleted {
		return retryinfo.Apply(domain.WithRequestRetry(finerr.TokenRevoked(), false))
	}
	return nil
}
