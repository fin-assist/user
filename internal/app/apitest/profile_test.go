package apitest_test

import (
	"context"
	"strings"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/user/internal/app/apitest"
)

const profileUserA = "019c7714-3b77-74d1-9866-e1f484aae2ab"
const profileUserB = "019c7714-3b77-74d1-9866-e1f484aae2ac"

func stringPtr(v string) *string { return &v }

func seedProfiles(t *testing.T, env *apitest.Env) {
	t.Helper()
	seedDocuments(t, env)
	_, err := env.App.DB.Primary().Exec(t.Context(), `INSERT INTO app_user(id,phone,created_at) VALUES
	('019c7714-3b77-74d1-9866-e1f484aae2ab','+79001234567','2026-10-01T00:00:00Z'),
	('019c7714-3b77-74d1-9866-e1f484aae2ac','+79001234568','2026-10-02T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestProfileAPISettingsAndIsolation(t *testing.T) {
	env := apitest.Start(t)
	seedProfiles(t, env)
	c := userv1.NewProfileServiceClient(env.Conn)
	ctx := apitest.User(t.Context(), profileUserA)
	got, err := c.GetProfile(ctx, &userv1.GetProfileRequest{})
	if err != nil {
		t.Fatal(err)
	}
	p := got.GetProfile()
	if p.GetUserId() != profileUserA || p.GetPhone() != "+79001234567" || p.GetTheme() != userv1.Theme_THEME_SYSTEM || p.GetTimezone() != "Europe/Moscow" || p.GetAssistantConsent().GetGranted() || p.GetAssistantConsent().GetCurrentVersion() != "v2" || p.GetAssistantConsent().Version != nil || !p.GetCreatedAt().AsTime().Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal(p)
	}
	empty, err := c.UpdateProfile(ctx, &userv1.UpdateProfileRequest{})
	if err != nil || !proto.Equal(p, empty.GetProfile()) {
		t.Fatalf("empty patch: %v %v", empty, err)
	}
	dark := userv1.Theme_THEME_DARK
	changed, err := c.UpdateProfile(ctx, &userv1.UpdateProfileRequest{Theme: &dark})
	if err != nil || changed.GetProfile().GetTheme() != dark {
		t.Fatalf("theme: %v %v", changed, err)
	}
	zone := "Asia/Tokyo"
	changed, err = c.UpdateProfile(ctx, &userv1.UpdateProfileRequest{Timezone: &zone})
	if err != nil || changed.GetProfile().GetTimezone() != zone || changed.GetProfile().GetTheme() != dark {
		t.Fatalf("zone: %v %v", changed, err)
	}
	if _, err := c.UpdateProfile(ctx, &userv1.UpdateProfileRequest{Timezone: &zone}); err != nil {
		t.Fatal(err)
	}
	var count, version int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT settings_version,(SELECT count(*) FROM outbox_events) FROM app_user WHERE id=$1`, profileUserA).Scan(&version, &count); err != nil || version != 2 || count != 1 {
		t.Fatalf("version=%d events=%d err=%v", version, count, err)
	}
	other, err := c.GetProfile(apitest.User(t.Context(), profileUserB), &userv1.GetProfileRequest{})
	if err != nil || other.GetProfile().GetPhone() != "+79001234568" || other.GetProfile().GetTheme() != userv1.Theme_THEME_SYSTEM || other.GetProfile().GetTimezone() != "Europe/Moscow" {
		t.Fatalf("foreign profile: %v %v", other, err)
	}
	for _, request := range []*userv1.UpdateProfileRequest{{Theme: userv1.Theme_THEME_UNSPECIFIED.Enum()}, {Theme: userv1.Theme(999).Enum()}, {Timezone: stringPtr("Local")}, {Timezone: stringPtr("Unknown/Timezone")}, {Timezone: stringPtr(strings.Repeat("x", 65))}} {
		_, err := c.UpdateProfile(ctx, request)
		if apitest.Reason(err) != "VALIDATION_ERROR" {
			t.Fatalf("invalid patch: %v", err)
		}
		field := "timezone"
		if request.Theme != nil {
			field = "theme"
		}
		found := false
		for _, d := range status.Convert(err).Details() {
			if b, ok := d.(*errdetails.BadRequest); ok {
				for _, v := range b.GetFieldViolations() {
					if v.GetField() == field {
						found = true
					}
				}
			}
		}
		if !found {
			t.Fatalf("missing field %s: %v", field, err)
		}
	}
}

func TestProfileAPIConsentAndSystemCheck(t *testing.T) {
	env := apitest.Start(t)
	seedProfiles(t, env)
	c := userv1.NewProfileServiceClient(env.Conn)
	internal := userv1.NewUserInternalServiceClient(env.Conn)
	ctx := apitest.User(t.Context(), profileUserA)
	for range 2 {
		got, err := c.GrantAssistantConsent(ctx, &userv1.GrantAssistantConsentRequest{Version: "v2"})
		if err != nil || !got.GetProfile().GetAssistantConsent().GetGranted() || got.GetProfile().GetAssistantConsent().GetVersion() != "v2" {
			t.Fatalf("grant: %v %v", got, err)
		}
	}
	var count int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM user_consent WHERE user_id=$1`, profileUserA).Scan(&count); err != nil || count != 1 {
		t.Fatalf("repeated grant: %d %v", count, err)
	}
	request := &userv1.GetAssistantConsentStatusRequest{UserId: stringPtr(profileUserA)}
	// The table permits only assistant's system model; a supplied user token cannot replace user_id.
	system := metadata.AppendToOutgoingContext(apitest.Caller(t.Context(), "assistant"), "authorization", "Bearer dev:"+profileUserB, "x-read-primary", "0")
	r, err := internal.GetAssistantConsentStatus(system, request)
	if err != nil || !r.GetConsent().GetGranted() {
		t.Fatalf("system consent: %v %v", r, err)
	}
	for range 2 {
		if _, err := c.RevokeAssistantConsent(ctx, &userv1.RevokeAssistantConsentRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	r, err = internal.GetAssistantConsentStatus(system, request)
	if err != nil || r.GetConsent().GetGranted() || r.GetConsent().Version != nil {
		t.Fatalf("revoke not visible: %v %v", r, err)
	}
	if _, err := c.GrantAssistantConsent(ctx, &userv1.GrantAssistantConsentRequest{Version: "v1"}); apitest.Reason(err) != "CONSENT_OUTDATED" {
		t.Fatal(err)
	}
	if _, err := c.GrantAssistantConsent(ctx, &userv1.GrantAssistantConsentRequest{Version: "v2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE consent_document SET is_current=false WHERE type='assistant';INSERT INTO consent_document(type,version,title,url,is_current) VALUES('assistant','v3','New TEST consent','https://example.test/assistant/v3',true)`); err != nil {
		t.Fatal(err)
	}
	r, err = internal.GetAssistantConsentStatus(system, request)
	if err != nil || r.GetConsent().GetGranted() || r.GetConsent().GetVersion() != "v2" || r.GetConsent().GetCurrentVersion() != "v3" {
		t.Fatalf("outdated grant: %v %v", r, err)
	}
	other, err := c.GetProfile(apitest.User(t.Context(), profileUserB), &userv1.GetProfileRequest{})
	if err != nil || other.GetProfile().GetAssistantConsent().GetGranted() {
		t.Fatal("foreign consent changed")
	}
	for _, id := range []*string{nil, stringPtr("not-a-uuid")} {
		if _, err := internal.GetAssistantConsentStatus(apitest.Caller(t.Context(), "assistant"), &userv1.GetAssistantConsentStatusRequest{UserId: id}); apitest.Reason(err) != "VALIDATION_ERROR" {
			t.Fatalf("system user id: %v", err)
		}
	}
	if _, err := internal.GetAssistantConsentStatus(apitest.Caller(t.Context(), "bank-bff"), request); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign status caller: %v", err)
	}
	if _, err := internal.GetAssistantConsentStatus(apitest.Caller(t.Context(), "assistant"), &userv1.GetAssistantConsentStatusRequest{UserId: stringPtr("019c7714-3b77-74d1-9866-e1f484aae2ad")}); apitest.Reason(err) != "NOT_FOUND" {
		t.Fatal(err)
	}
}

func TestProfileAPIAccessDeletionAndFailures(t *testing.T) {
	env := apitest.Start(t)
	seedProfiles(t, env)
	c := userv1.NewProfileServiceClient(env.Conn)
	calls := []func(context.Context) error{
		func(ctx context.Context) error { _, err := c.GetProfile(ctx, &userv1.GetProfileRequest{}); return err },
		func(ctx context.Context) error {
			_, err := c.UpdateProfile(ctx, &userv1.UpdateProfileRequest{})
			return err
		},
		func(ctx context.Context) error {
			_, err := c.GrantAssistantConsent(ctx, &userv1.GrantAssistantConsentRequest{Version: "v2"})
			return err
		},
		func(ctx context.Context) error {
			_, err := c.RevokeAssistantConsent(ctx, &userv1.RevokeAssistantConsentRequest{})
			return err
		},
	}
	for _, call := range calls {
		if err := call(apitest.Caller(t.Context(), "bank-bff")); apitest.Reason(err) != "INVALID_TOKEN" {
			t.Fatalf("no user: %v", err)
		}
		if err := call(apitest.Caller(t.Context(), "assistant")); status.Code(err) != codes.PermissionDenied || apitest.Reason(err) != "" {
			t.Fatalf("foreign caller: %v", err)
		}
		if err := call(t.Context()); status.Code(err) != codes.Unauthenticated || apitest.Reason(err) != "" {
			t.Fatalf("no service: %v", err)
		}
		if err := call(apitest.User(t.Context(), "019c7714-3b77-74d1-9866-e1f484aae2ad")); apitest.Reason(err) != "NOT_FOUND" {
			t.Fatalf("missing user: %v", err)
		}
	}
	if err := env.App.DB.Run(t.Context(), func(ctx context.Context) error {
		return env.App.Tombstones.Purge(ctx, profileUserA, func(ctx context.Context, id string) error {
			_, err := env.App.DB.Writer(ctx).Exec(ctx, `DELETE FROM app_user WHERE id=$1`, id)
			return err
		})
	}); err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		if err := call(apitest.User(t.Context(), profileUserA)); apitest.Reason(err) != "TOKEN_REVOKED" {
			t.Fatalf("deleted user: %v", err)
		}
	}
	internal := userv1.NewUserInternalServiceClient(env.Conn)
	if _, err := internal.GetAssistantConsentStatus(apitest.Caller(t.Context(), "assistant"), &userv1.GetAssistantConsentStatusRequest{UserId: stringPtr(profileUserA)}); apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(apitest.User(t.Context(), profileUserB))
	cancel()
	if err := calls[0](ctx); status.Code(err) != codes.Canceled {
		t.Fatal(err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `DROP TABLE consent_document CASCADE`); err != nil {
		t.Fatal(err)
	}
	for _, call := range calls {
		err := call(apitest.User(t.Context(), profileUserB))
		if status.Code(err) != codes.Internal || apitest.Reason(err) != "" || strings.Contains(status.Convert(err).Message(), "consent_document") {
			t.Fatalf("storage error leaked: %v", err)
		}
	}
	if _, err := internal.GetAssistantConsentStatus(apitest.Caller(t.Context(), "assistant"), &userv1.GetAssistantConsentStatusRequest{UserId: stringPtr(profileUserB)}); status.Code(err) != codes.Internal {
		t.Fatal(err)
	}
}
