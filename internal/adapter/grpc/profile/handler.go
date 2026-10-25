// Package profile adapts the profile RPCs to their usecases.
package profile

import (
	"context"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/go-grpc/reqctx"

	"github.com/fin-assist/user/internal/profile"
)

// Usecases is the profile handler's consumer-owned application interface.
type Usecases interface {
	Get(context.Context, string) (profile.Record, error)
	Update(context.Context, string, profile.Patch) (profile.Record, error)
	Grant(context.Context, string, string) (profile.Record, error)
	Revoke(context.Context, string) error
}

// Deletions is separate from profile settings and consents.
type Deletions interface {
	Delete(context.Context, string) error
}

type handler struct {
	userv1.UnimplementedProfileServiceServer
	service   Usecases
	deletions Deletions
}

// Register installs profile RPCs and the atomic account-deletion usecase.
func Register(s grpc.ServiceRegistrar, service Usecases, deletions Deletions) {
	userv1.RegisterProfileServiceServer(s, &handler{service: service, deletions: deletions})
}

func (h *handler) GetProfile(ctx context.Context, _ *userv1.GetProfileRequest) (*userv1.GetProfileResponse, error) {
	id, ok := reqctx.UserID(ctx)
	if !ok {
		return nil, finerr.InvalidToken()
	}
	p, err := h.service.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &userv1.GetProfileResponse{Profile: Message(p)}, nil
}
func (h *handler) UpdateProfile(ctx context.Context, in *userv1.UpdateProfileRequest) (*userv1.UpdateProfileResponse, error) {
	id, ok := reqctx.UserID(ctx)
	if !ok {
		return nil, finerr.InvalidToken()
	}
	patch := profile.Patch{Timezone: in.Timezone}
	if in.Theme != nil {
		theme := themeString(in.GetTheme())
		patch.Theme = &theme
	}
	p, err := h.service.Update(ctx, id, patch)
	if err != nil {
		return nil, err
	}
	return &userv1.UpdateProfileResponse{Profile: Message(p)}, nil
}
func (h *handler) GrantAssistantConsent(ctx context.Context, in *userv1.GrantAssistantConsentRequest) (*userv1.GrantAssistantConsentResponse, error) {
	id, ok := reqctx.UserID(ctx)
	if !ok {
		return nil, finerr.InvalidToken()
	}
	p, err := h.service.Grant(ctx, id, in.GetVersion())
	if err != nil {
		return nil, err
	}
	return &userv1.GrantAssistantConsentResponse{Profile: Message(p)}, nil
}
func (h *handler) RevokeAssistantConsent(ctx context.Context, _ *userv1.RevokeAssistantConsentRequest) (*userv1.RevokeAssistantConsentResponse, error) {
	id, ok := reqctx.UserID(ctx)
	if !ok {
		return nil, finerr.InvalidToken()
	}
	if err := h.service.Revoke(ctx, id); err != nil {
		return nil, err
	}
	return &userv1.RevokeAssistantConsentResponse{}, nil
}

func (h *handler) DeleteAccount(ctx context.Context, _ *userv1.DeleteAccountRequest) (*userv1.DeleteAccountResponse, error) {
	id, ok := reqctx.UserID(ctx)
	if !ok {
		return nil, finerr.InvalidToken()
	}
	if err := h.deletions.Delete(ctx, id); err != nil {
		return nil, err
	}
	return &userv1.DeleteAccountResponse{}, nil
}

// Message maps a validated profile snapshot to its generated API representation.
func Message(p profile.Record) *userv1.Profile {
	theme := map[string]userv1.Theme{"system": userv1.Theme_THEME_SYSTEM, "light": userv1.Theme_THEME_LIGHT, "dark": userv1.Theme_THEME_DARK}[p.Theme]
	return &userv1.Profile{UserId: p.ID, Phone: p.Phone, Theme: theme, Timezone: p.Timezone, CreatedAt: timestamppb.New(p.CreatedAt), AssistantConsent: ConsentMessage(p.Consent)}
}

// ConsentMessage maps the effective consent while preserving optional version presence.
func ConsentMessage(c profile.Consent) *userv1.AssistantConsent {
	return &userv1.AssistantConsent{Granted: c.Granted, Version: c.Version, CurrentVersion: c.CurrentVersion}
}

func themeString(theme userv1.Theme) string {
	switch theme {
	case userv1.Theme_THEME_SYSTEM:
		return "system"
	case userv1.Theme_THEME_LIGHT:
		return "light"
	case userv1.Theme_THEME_DARK:
		return "dark"
	default:
		return ""
	}
}
