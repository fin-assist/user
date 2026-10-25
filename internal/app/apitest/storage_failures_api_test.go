package apitest_test

import (
	"strings"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/user/internal/app/apitest"
)

// SQL fault injection preserves real repositories, transactions and transport.
// Each failure is repaired before retry; the caller must never observe a partial
// account/session, consumed registration token or leaked storage diagnostics.
func TestAPIRegistrationStorageFailuresAreAtomicAndRetryable(t *testing.T) {
	for _, tc := range []struct{ name, breakSQL, restoreSQL string }{
		{"account-insert", `ALTER TABLE app_user RENAME COLUMN updated_at TO unavailable`, `ALTER TABLE app_user RENAME COLUMN unavailable TO updated_at`},
		{"consent-insert", `ALTER TABLE user_consent ADD COLUMN unavailable text NOT NULL`, `ALTER TABLE user_consent DROP COLUMN unavailable`},
		{"timezone-event", `ALTER TABLE outbox_events RENAME TO unavailable_events`, `ALTER TABLE unavailable_events RENAME TO outbox_events`},
		{"session-insert", `ALTER TABLE auth_session RENAME COLUMN created_at TO unavailable`, `ALTER TABLE auth_session RENAME COLUMN unavailable TO created_at`},
		{"refresh-insert", `ALTER TABLE refresh_token RENAME COLUMN generation TO unavailable`, `ALTER TABLE refresh_token RENAME COLUMN unavailable TO generation`},
		{"consume-result", `ALTER TABLE phone_verification RENAME COLUMN result_ciphertext TO unavailable`, `ALTER TABLE phone_verification RENAME COLUMN unavailable TO result_ciphertext`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := apitest.Start(t)
			seedDocuments(t, env)
			seedRegistration(t, env, "registration-storage-token", "+79001234567", time.Now().Add(-time.Minute))
			if _, err := env.App.DB.Primary().Exec(t.Context(), tc.breakSQL); err != nil {
				t.Fatal(err)
			}
			client := userv1.NewAuthServiceClient(env.Conn)
			in := registrationRequest("registration-storage-token")
			response, err := client.Register(authContext(t.Context(), authKey()), in)
			if response != nil || status.Code(err) != codes.Internal || apitest.Reason(err) != "" || strings.Contains(status.Convert(err).Message(), "unavailable") {
				t.Fatal(response, err)
			}
			if _, err := env.App.DB.Primary().Exec(t.Context(), tc.restoreSQL); err != nil {
				t.Fatal(err)
			}
			var users, sessions int
			var consumed bool
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM app_user),(SELECT count(*) FROM auth_session),(SELECT closed_at IS NOT NULL FROM phone_verification LIMIT 1)`).Scan(&users, &sessions, &consumed); err != nil || users != 0 || sessions != 0 || consumed {
				t.Fatal(users, sessions, consumed, err)
			}
			registered, err := client.Register(authContext(t.Context(), authKey()), in)
			if err != nil || registered.GetTokens().GetRefreshToken() == "" {
				t.Fatal("retry after repair", registered, err)
			}
		})
	}
}

func TestAPIRefreshRejectsCorruptedGenerationAndRollsBackUnavailableStorage(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	seedRegistration(t, env, "refresh-storage-registration", "+79001234567", time.Now().Add(-time.Minute))
	client := userv1.NewAuthServiceClient(env.Conn)
	registered, err := client.Register(authContext(t.Context(), authKey()), registrationRequest("refresh-storage-registration"))
	if err != nil {
		t.Fatal(err)
	}
	raw := registered.GetTokens().GetRefreshToken()
	for _, generation := range []int{0, 2147483647} {
		if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE refresh_token SET generation=$1`, generation); err != nil {
			t.Fatal(err)
		}
		if got, err := client.RefreshTokens(authContext(t.Context(), authKey()), &userv1.RefreshTokensRequest{RefreshToken: raw}); got != nil || status.Code(err) != codes.Internal {
			t.Fatal(got, err)
		}
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE refresh_token SET generation=1,issued_at=now()+interval '1 minute'`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE refresh_token ADD COLUMN unavailable text NOT NULL DEFAULT 'existing';ALTER TABLE refresh_token ALTER COLUMN unavailable DROP DEFAULT`); err != nil {
		t.Fatal(err)
	}
	if got, err := client.RefreshTokens(authContext(t.Context(), authKey()), &userv1.RefreshTokensRequest{RefreshToken: raw}); got != nil || status.Code(err) != codes.Internal {
		t.Fatal(got, err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE refresh_token DROP COLUMN unavailable`); err != nil {
		t.Fatal(err)
	}
	refreshed, err := client.RefreshTokens(authContext(t.Context(), authKey()), &userv1.RefreshTokensRequest{RefreshToken: raw})
	if err != nil || refreshed.GetTokens().GetRefreshToken() == raw {
		t.Fatal(refreshed, err)
	}
	var validWindow bool
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT bool_and(expires_at>issued_at) FROM refresh_token`).Scan(&validWindow); err != nil || !validWindow {
		t.Fatal(validWindow, err)
	}
	// Failed logout must not report success or erase the session.
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE refresh_token RENAME COLUMN session_id TO unavailable`); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Logout(apitest.User(t.Context(), registered.GetUserId()), &userv1.LogoutRequest{RefreshToken: refreshed.GetTokens().GetRefreshToken()}); status.Code(err) != codes.Internal {
		t.Fatal(err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE refresh_token RENAME COLUMN unavailable TO session_id`); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Logout(apitest.User(t.Context(), registered.GetUserId()), &userv1.LogoutRequest{RefreshToken: refreshed.GetTokens().GetRefreshToken()}); err != nil {
		t.Fatal(err)
	}
}

func TestAPIProfileWritesDoNotExposePartialSettingsOrConsentAfterStorageFailure(t *testing.T) {
	for _, tc := range []struct{ name, breakSQL, restoreSQL string }{
		{"settings-update", `ALTER TABLE app_user RENAME COLUMN updated_at TO unavailable`, `ALTER TABLE app_user RENAME COLUMN unavailable TO updated_at`},
		{"timezone-event", `ALTER TABLE outbox_events RENAME TO unavailable_events`, `ALTER TABLE unavailable_events RENAME TO outbox_events`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := apitest.Start(t)
			seedProfiles(t, env)
			c := userv1.NewProfileServiceClient(env.Conn)
			ctx := apitest.User(t.Context(), profileUserA)
			if _, err := env.App.DB.Primary().Exec(t.Context(), tc.breakSQL); err != nil {
				t.Fatal(err)
			}
			zone := "Asia/Tokyo"
			if got, err := c.UpdateProfile(ctx, &userv1.UpdateProfileRequest{Timezone: &zone}); got != nil || status.Code(err) != codes.Internal || apitest.Reason(err) != "" {
				t.Fatal(got, err)
			}
			if _, err := env.App.DB.Primary().Exec(t.Context(), tc.restoreSQL); err != nil {
				t.Fatal(err)
			}
			before, err := c.GetProfile(ctx, &userv1.GetProfileRequest{})
			if err != nil || before.GetProfile().GetTimezone() != "Europe/Moscow" {
				t.Fatal(before, err)
			}
			if got, err := c.UpdateProfile(ctx, &userv1.UpdateProfileRequest{Timezone: &zone}); err != nil || got.GetProfile().GetTimezone() != zone {
				t.Fatal(got, err)
			}
		})
	}
	env := apitest.Start(t)
	seedProfiles(t, env)
	c := userv1.NewProfileServiceClient(env.Conn)
	ctx := apitest.User(t.Context(), profileUserA)
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE user_consent ADD COLUMN unavailable text NOT NULL`); err != nil {
		t.Fatal(err)
	}
	if got, err := c.GrantAssistantConsent(ctx, &userv1.GrantAssistantConsentRequest{Version: "v2"}); got != nil || status.Code(err) != codes.Internal {
		t.Fatal(got, err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE user_consent DROP COLUMN unavailable`); err != nil {
		t.Fatal(err)
	}
	before, err := c.GetProfile(ctx, &userv1.GetProfileRequest{})
	if err != nil || before.GetProfile().GetAssistantConsent().GetGranted() {
		t.Fatal(before, err)
	}
	if _, err := c.GrantAssistantConsent(ctx, &userv1.GrantAssistantConsentRequest{Version: "v2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE user_consent SET granted_at=now()+interval '1 hour' WHERE user_id=$1`, profileUserA); err != nil {
		t.Fatal(err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE user_consent ALTER COLUMN revoked_at TYPE bigint USING NULL`); err != nil {
		t.Fatal(err)
	}
	if got, err := c.RevokeAssistantConsent(ctx, &userv1.RevokeAssistantConsentRequest{}); got != nil || status.Code(err) != codes.Internal {
		t.Fatal(got, err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE user_consent ALTER COLUMN revoked_at TYPE timestamptz USING NULL`); err != nil {
		t.Fatal(err)
	}
	before, err = c.GetProfile(ctx, &userv1.GetProfileRequest{})
	if err != nil || !before.GetProfile().GetAssistantConsent().GetGranted() {
		t.Fatal(before, err)
	}
	if _, err := c.RevokeAssistantConsent(ctx, &userv1.RevokeAssistantConsentRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE app_user SET settings_version=2147483647 WHERE id=$1`, profileUserA); err != nil {
		t.Fatal(err)
	}
	zone := "Asia/Tokyo"
	if got, err := c.UpdateProfile(ctx, &userv1.UpdateProfileRequest{Timezone: &zone}); got != nil || status.Code(err) != codes.Internal {
		t.Fatal(got, err)
	}
}

func TestAPIRegistrationRejectsUnpublishedConsentAndFutureProofBeforeAccountCreation(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	seedRegistration(t, env, "guarded-public-registration", "+79001234567", time.Now().Add(time.Minute))
	c := userv1.NewAuthServiceClient(env.Conn)
	in := registrationRequest("guarded-public-registration")
	if got, err := c.Register(authContext(t.Context(), authKey()), in); got != nil || status.Code(err) != codes.Internal {
		t.Fatal(got, err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE phone_verification SET verified_at=now();UPDATE consent_document SET is_current=false WHERE type='personal_data'`); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Register(authContext(t.Context(), authKey()), in); got != nil || status.Code(err) != codes.Internal {
		t.Fatal(got, err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE consent_document SET is_current=true WHERE type='personal_data';ALTER TABLE consent_document RENAME COLUMN is_current TO unavailable`); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Register(authContext(t.Context(), authKey()), in); got != nil || status.Code(err) != codes.Internal {
		t.Fatal(got, err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE consent_document RENAME COLUMN unavailable TO is_current`); err != nil {
		t.Fatal(err)
	}
	registered, err := c.Register(authContext(t.Context(), authKey()), in)
	if err != nil {
		t.Fatal("repair must preserve one-time proof", err)
	}
	old := registered.GetTokens().GetRefreshToken()
	first, err := c.RefreshTokens(authContext(t.Context(), authKey()), &userv1.RefreshTokensRequest{RefreshToken: old})
	if err != nil {
		t.Fatal(err)
	}
	// Reuse revocation remains monotonic after a backwards wall-clock correction.
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE refresh_token SET rotated_at=now()+interval '1 minute' WHERE token_hash=$1`, tokenHash(old)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RefreshTokens(authContext(t.Context(), authKey()), &userv1.RefreshTokensRequest{RefreshToken: old}); apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatal(err)
	}
	if _, err := c.RefreshTokens(authContext(t.Context(), authKey()), &userv1.RefreshTokensRequest{RefreshToken: first.GetTokens().GetRefreshToken()}); apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatal(err)
	}
}

func TestAPIConditionalConsentAuthenticationFailsClosedWhenDeletionStoreIsUnavailable(t *testing.T) {
	env := apitest.Start(t)
	seedProfiles(t, env)
	c := userv1.NewConsentDocumentServiceClient(env.Conn)
	request := &userv1.GetConsentDocumentRequest{Type: userv1.ConsentType_CONSENT_TYPE_ASSISTANT}
	for _, token := range []string{"Bearer", "Basic invalid", "Bearer invalid-jwt"} {
		ctx := metadata.AppendToOutgoingContext(apitest.Caller(t.Context(), "bank-bff"), "authorization", token)
		if got, err := c.GetConsentDocument(ctx, request); got != nil || apitest.Reason(err) != "INVALID_TOKEN" {
			t.Fatal(got, err)
		}
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE tombstone_users RENAME TO unavailable_tombstones`); err != nil {
		t.Fatal(err)
	}
	ctx := apitest.User(t.Context(), profileUserA)
	if got, err := c.GetConsentDocument(ctx, request); got != nil || status.Code(err) != codes.Internal || apitest.Reason(err) != "" {
		t.Fatal(got, err)
	}
	if got, err := userv1.NewProfileServiceClient(env.Conn).GetProfile(ctx, &userv1.GetProfileRequest{}); got != nil || status.Code(err) != codes.Unavailable || apitest.Reason(err) != "" {
		t.Fatal(got, err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE unavailable_tombstones RENAME TO tombstone_users`); err != nil {
		t.Fatal(err)
	}
	if got, err := c.GetConsentDocument(ctx, request); err != nil || got.GetDocument().GetVersion() != "v2" {
		t.Fatal(got, err)
	}
}
