package profile

import (
	"context"
	"errors"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"

	"github.com/fin-assist/go-grpc/reqctx"

	"github.com/fin-assist/user/internal/profile"
)

type service struct {
	gotID   string
	patch   profile.Patch
	version string
	err     error
}

func (s *service) Get(_ context.Context, id string) (profile.Record, error) {
	s.gotID = id
	return record(), s.err
}
func (s *service) Update(_ context.Context, id string, p profile.Patch) (profile.Record, error) {
	s.gotID, s.patch = id, p
	return record(), s.err
}
func (s *service) Grant(_ context.Context, id, v string) (profile.Record, error) {
	s.gotID, s.version = id, v
	return record(), s.err
}
func (s *service) Revoke(_ context.Context, id string) error { s.gotID = id; return s.err }
func record() profile.Record {
	v := "v1"
	return profile.Record{ID: "user", Phone: "+79001234567", Theme: "dark", Timezone: "UTC", CreatedAt: time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC), Consent: profile.Consent{Granted: true, Version: &v, CurrentVersion: "v1"}}
}

func TestUnitHandlersIdentityMappingAndErrors(t *testing.T) {
	s := &service{}
	h := &handler{service: s}
	ctx := reqctx.WithUserID(t.Context(), "verified-sub")
	get, err := h.GetProfile(ctx, &userv1.GetProfileRequest{})
	if err != nil || get.GetProfile().GetPhone() != record().Phone || s.gotID != "verified-sub" {
		t.Fatalf("%v %v", get, err)
	}
	zone := "Asia/Tokyo"
	theme := userv1.Theme_THEME_LIGHT
	updated, err := h.UpdateProfile(ctx, &userv1.UpdateProfileRequest{Theme: &theme, Timezone: &zone})
	if err != nil || updated.GetProfile().GetTheme() != userv1.Theme_THEME_DARK || s.patch.Theme == nil || *s.patch.Theme != "light" || s.patch.Timezone != &zone {
		t.Fatalf("%+v %v", s.patch, err)
	}
	if _, err := h.UpdateProfile(ctx, &userv1.UpdateProfileRequest{}); err != nil || s.patch.Theme != nil || s.patch.Timezone != nil {
		t.Fatal("presence lost")
	}
	granted, err := h.GrantAssistantConsent(ctx, &userv1.GrantAssistantConsentRequest{Version: "v2"})
	if err != nil || s.version != "v2" || !granted.GetProfile().GetAssistantConsent().GetGranted() {
		t.Fatal(err)
	}
	if _, err := h.RevokeAssistantConsent(ctx, &userv1.RevokeAssistantConsentRequest{}); err != nil {
		t.Fatal(err)
	}
	calls := []func(context.Context) error{
		func(ctx context.Context) error { _, err := h.GetProfile(ctx, &userv1.GetProfileRequest{}); return err },
		func(ctx context.Context) error {
			_, err := h.UpdateProfile(ctx, &userv1.UpdateProfileRequest{})
			return err
		},
		func(ctx context.Context) error {
			_, err := h.GrantAssistantConsent(ctx, &userv1.GrantAssistantConsentRequest{})
			return err
		},
		func(ctx context.Context) error {
			_, err := h.RevokeAssistantConsent(ctx, &userv1.RevokeAssistantConsentRequest{})
			return err
		},
	}
	for _, call := range calls {
		if err := call(t.Context()); err == nil {
			t.Fatal("missing user reached usecase")
		}
		s.err = errors.New("usecase failed")
		if err := call(ctx); !errors.Is(err, s.err) {
			t.Fatal(err)
		}
		s.err = nil
	}
}

func TestUnitThemeAndConsentMapping(t *testing.T) {
	for theme, want := range map[userv1.Theme]string{userv1.Theme_THEME_SYSTEM: "system", userv1.Theme_THEME_LIGHT: "light", userv1.Theme_THEME_DARK: "dark", userv1.Theme_THEME_UNSPECIFIED: "", userv1.Theme(100): ""} {
		if got := themeString(theme); got != want {
			t.Fatalf("%v: %q", theme, got)
		}
	}
	for theme, want := range map[string]userv1.Theme{"system": userv1.Theme_THEME_SYSTEM, "light": userv1.Theme_THEME_LIGHT, "dark": userv1.Theme_THEME_DARK} {
		p := record()
		p.Theme = theme
		m := Message(p)
		if m.GetTheme() != want || !m.GetCreatedAt().AsTime().Equal(p.CreatedAt) || m.GetUserId() != p.ID || m.GetTimezone() != p.Timezone {
			t.Fatal(m)
		}
	}
	if ConsentMessage(profile.Consent{CurrentVersion: "v2"}).Version != nil {
		t.Fatal("absent version acquired presence")
	}
}
