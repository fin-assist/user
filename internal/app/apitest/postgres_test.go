package apitest_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/token"
)

func TestAPIRegistrationConstraintRefusalsRollBackEveryEffect(t *testing.T) {
	for _, timing := range []struct{ name, clause string }{
		{"immediate", "NOT DEFERRABLE"},
		{"deferred", "DEFERRABLE INITIALLY DEFERRED"},
	} {
		for _, constraint := range []string{"user_consent_document_fkey", "unexpected_document_fkey"} {
			t.Run(timing.name+"/"+constraint, func(t *testing.T) {
				env := apitest.Start(t)
				seedDocuments(t, env)
				// Test-only fault injection: keep the visible current document, while the
				// FK checks another catalog. Immediate checks fail inside engine.Do; deferred
				// checks fail at outer COMMIT. The production schema/migrations are unchanged.
				_, err := env.App.DB.Primary().Exec(t.Context(), `CREATE TABLE test_document(type text,version text,PRIMARY KEY(type,version));
 ALTER TABLE user_consent DROP CONSTRAINT user_consent_document_fkey;`)
				if err != nil {
					t.Fatal(err)
				}
				sql := `ALTER TABLE user_consent ADD CONSTRAINT ` + constraint + ` FOREIGN KEY(type,version) REFERENCES test_document(type,version) ` + timing.clause
				if _, err = env.App.DB.Primary().Exec(t.Context(), sql); err != nil {
					t.Fatal(err)
				}
				raw, key := timing.name+"-registration", authKey()
				id := seedRegistration(t, env, raw, "+79008887766", time.Now().UTC().Add(-time.Minute))
				client := userv1.NewAuthServiceClient(env.Conn)
				_, err = client.Register(authContext(t.Context(), key), registrationRequest(raw))
				want, wantCode := "CONSENT_OUTDATED", codes.FailedPrecondition
				if constraint != "user_consent_document_fkey" {
					want, wantCode = "", codes.Internal
				}
				if err == nil || status.Code(err) != wantCode || apitest.Reason(err) != want || (want == "" && (status.Code(err) != codes.Internal || status.Convert(err).Message() != codes.Internal.String())) || strings.Contains(err.Error(), "79008887766") || strings.Contains(err.Error(), "test_document") {
					t.Fatal("unsafe/wrong constraint refusal", err)
				}
				for _, table := range []string{"app_user", "auth_session", "refresh_token", "user_consent", "outbox_events", "idempotency_keys"} {
					var n int
					if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&n); err != nil || n != 0 {
						t.Fatal("partial transaction", table, n, err)
					}
				}
				var closed bool
				if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT closed_at IS NOT NULL FROM phone_verification WHERE id=$1`, id).Scan(&closed); err != nil || closed {
					t.Fatal("registration result consumed", closed, err)
				}
				// The same idempotency key can succeed once the missing reference is repaired.
				if _, err = env.App.DB.Primary().Exec(t.Context(), `INSERT INTO test_document VALUES('personal_data','v1')`); err != nil {
					t.Fatal(err)
				}
				out, err := client.Register(authContext(t.Context(), key), registrationRequest(raw))
				if err != nil || out.GetUserId() == "" {
					t.Fatal("retry after rollback", out, err)
				}
			})
		}
	}
}

func TestAPIDedicatedAuthTransactionKeepsConfirmationEventsAndMetrics(t *testing.T) {
	env := fakePhoneEnv(t)
	seedDocuments(t, env)
	phone, key := "+70000001955", authKey()
	start, err := startCall(t, env, key, phone)
	if err != nil {
		t.Fatal(err)
	}
	watch := openPhoneWatch(t, env, phoneContext(t.Context()), start.GetVerificationToken())
	verified, err := watch.Recv()
	if err != nil {
		t.Fatal(err)
	}
	client := userv1.NewAuthServiceClient(env.Conn)
	registered, err := client.Register(authContext(t.Context(), authKey()), registrationRequest(verified.GetRegistrationToken()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = client.RefreshTokens(authContext(t.Context(), authKey()), &userv1.RefreshTokensRequest{RefreshToken: registered.GetTokens().GetRefreshToken()}); err != nil {
		t.Fatal(err)
	}
	// Expire only the old phone claim; its confirmed result remains valid. This
	// exercises a later login without bypassing the production busy-phone policy.
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE phone_verification SET released_at=now(),expires_at=now()-interval '1 second' WHERE phone=$1`, phone); err != nil {
		t.Fatal(err)
	}
	second, err := startCall(t, env, authKey(), phone)
	if err != nil {
		t.Fatal(err)
	}
	existing, err := openPhoneWatch(t, env, phoneContext(t.Context()), second.GetVerificationToken()).Recv()
	if err != nil || existing.GetIsNewUser() || existing.GetTokens() == nil {
		t.Fatal("existing-user confirmation lost auth transaction", existing, err)
	}
	var events, sessions, cached int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM outbox_events),(SELECT count(*) FROM auth_session),(SELECT count(*) FROM idempotency_keys WHERE done)`).Scan(&events, &sessions, &cached); err != nil || events != 1 || sessions != 2 || cached < 4 {
		t.Fatal("idempotency/outbox/session effects", events, sessions, cached, err)
	}
	// The report remains an independent ordinary-pool job after selection moved
	// to the dedicated auth transaction. No one-minute scheduler wait is needed.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	found := false
	for _, job := range env.App.Jobs {
		if job.Name == "phone-callback-outcomes" {
			found = true
			go func() { done <- job.Run(ctx) }()
			break
		}
	}
	if !found {
		t.Fatal("callback outcomes job missing")
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		total := zvonokMetric(t, env, "finassist_phone_callback_outcomes_24h", map[string]string{"callback_number": start.GetCallbackNumber(), "outcome": "confirmed"})
		if second.GetCallbackNumber() != start.GetCallbackNumber() {
			total += zvonokMetric(t, env, "finassist_phone_callback_outcomes_24h", map[string]string{"callback_number": second.GetCallbackNumber(), "outcome": "confirmed"})
		}
		if total == 2 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("callback report lost writes", ctx.Err())
		case <-ticker.C:
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err = userv1.NewProfileServiceClient(env.Conn).DeleteAccount(apitest.User(t.Context(), registered.GetUserId()), &userv1.DeleteAccountRequest{}); err != nil {
		t.Fatal("tombstone/delete after auth effects", err)
	}
}

func TestAPIGrantConsentDeferredCommitPreservesProfileAndHistory(t *testing.T) {
	for _, constraint := range []string{"user_consent_document_fkey", "unexpected_grant_document_fkey"} {
		t.Run(constraint, func(t *testing.T) {
			env := apitest.Start(t)
			seedProfiles(t, env)
			client := userv1.NewProfileServiceClient(env.Conn)
			ctx := apitest.User(t.Context(), profileUserA)
			_, err := env.App.DB.Primary().Exec(t.Context(), `INSERT INTO consent_document(type,version,title,url,is_current) VALUES('assistant','v1','Old consent','https://example.test/old',false)`)
			if err != nil {
				t.Fatal(err)
			}
			_, err = env.App.DB.Primary().Exec(t.Context(), `INSERT INTO user_consent(user_id,type,version,granted_at) VALUES($1,'assistant','v1',now()-interval '1 day')`, profileUserA)
			if err != nil {
				t.Fatal(err)
			}
			before, err := client.GetProfile(ctx, &userv1.GetProfileRequest{})
			if err != nil {
				t.Fatal(err)
			}
			counts := apiTransactionCounts(t, env)
			// The visible current document remains v2. Only the test FK catalog omits it.
			_, err = env.App.DB.Primary().Exec(t.Context(), `CREATE TABLE test_document(type text,version text,PRIMARY KEY(type,version));
 INSERT INTO test_document VALUES('assistant','v1');
 ALTER TABLE user_consent DROP CONSTRAINT user_consent_document_fkey`)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE user_consent ADD CONSTRAINT `+constraint+` FOREIGN KEY(type,version) REFERENCES test_document(type,version) DEFERRABLE INITIALLY DEFERRED`); err != nil {
				t.Fatal(err)
			}
			_, err = client.GrantAssistantConsent(ctx, &userv1.GrantAssistantConsentRequest{Version: "v2"})
			wantCode, wantReason := codes.FailedPrecondition, "CONSENT_OUTDATED"
			if constraint != "user_consent_document_fkey" {
				wantCode, wantReason = codes.Internal, ""
			}
			requireAPIConstraintRefusal(t, err, wantCode, wantReason, profileUserA, "test_document")
			after, err := client.GetProfile(ctx, &userv1.GetProfileRequest{})
			if err != nil || !proto.Equal(before, after) {
				t.Fatal("failed grant changed profile", before, after, err)
			}
			if got := apiTransactionCounts(t, env); !reflect.DeepEqual(got, counts) {
				t.Fatal("failed grant left effects", counts, got)
			}
			var version string
			var revoked bool
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT version,revoked_at IS NOT NULL FROM user_consent WHERE user_id=$1`, profileUserA).Scan(&version, &revoked); err != nil || version != "v1" || revoked {
				t.Fatal("old consent revocation was not rolled back", version, revoked, err)
			}
			if _, err := env.App.DB.Primary().Exec(t.Context(), `INSERT INTO test_document VALUES('assistant','v2')`); err != nil {
				t.Fatal(err)
			}
			granted, err := client.GrantAssistantConsent(ctx, &userv1.GrantAssistantConsentRequest{Version: "v2"})
			if err != nil || !granted.GetProfile().GetAssistantConsent().GetGranted() || granted.GetProfile().GetAssistantConsent().GetVersion() != "v2" {
				t.Fatal("grant retry after repair", granted, err)
			}
		})
	}
}

func TestAPIRefreshDeferredCommitPreservesChainAndIdempotency(t *testing.T) {
	for _, constraint := range []string{"phone_verification_user_fkey", "unexpected_refresh_user_fkey"} {
		t.Run(constraint, func(t *testing.T) {
			env := apitest.Start(t)
			seedDocuments(t, env)
			registered, _ := registerAPI(t, env, "refresh-commit-registration", "+79008887777")
			var verification int32
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT phone_verification_id FROM auth_session WHERE user_id=$1`, registered.GetUserId()).Scan(&verification); err != nil {
				t.Fatal(err)
			}
			// Restore a verification result that has not yet been consumed.
			// Refresh's actual CloseVerification statement assigns user_id and queues the FK.
			_, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE phone_verification SET user_id=NULL,closed_at=NULL WHERE id=$1`, verification)
			if err != nil {
				t.Fatal(err)
			}
			_, err = env.App.DB.Primary().Exec(t.Context(), `CREATE TABLE test_verification_user(id uuid PRIMARY KEY);
 ALTER TABLE phone_verification DROP CONSTRAINT phone_verification_user_fkey`)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE phone_verification ADD CONSTRAINT `+constraint+` FOREIGN KEY(user_id) REFERENCES test_verification_user(id) DEFERRABLE INITIALLY DEFERRED`); err != nil {
				t.Fatal(err)
			}
			before := apiTransactionCounts(t, env)
			client := userv1.NewAuthServiceClient(env.Conn)
			original := registered.GetTokens().GetRefreshToken()
			key := authKey()
			request := &userv1.RefreshTokensRequest{RefreshToken: original}
			_, err = client.RefreshTokens(authContext(t.Context(), key), request)
			wantCode, wantReason := codes.Unauthenticated, "INVALID_TOKEN"
			if constraint != "phone_verification_user_fkey" {
				wantCode, wantReason = codes.Internal, ""
			}
			requireAPIConstraintRefusal(t, err, wantCode, wantReason, original, registered.GetUserId(), "test_verification_user")
			if got := apiTransactionCounts(t, env); !reflect.DeepEqual(got, before) {
				t.Fatal("failed refresh left effects", before, got)
			}
			hash := token.Sum(original)
			var generation int32
			var rotated bool
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT generation,rotated_at IS NOT NULL FROM refresh_token WHERE token_hash=$1`, hash[:]).Scan(&generation, &rotated); err != nil || generation != 1 || rotated {
				t.Fatal("old refresh link consumed", generation, rotated, err)
			}
			var owner *string
			var closed bool
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT user_id::text,closed_at IS NOT NULL FROM phone_verification WHERE id=$1`, verification).Scan(&owner, &closed); err != nil || owner != nil || closed {
				t.Fatal("verification consumption was not rolled back", owner, closed, err)
			}
			if _, err := env.App.DB.Primary().Exec(t.Context(), `INSERT INTO test_verification_user VALUES($1)`, registered.GetUserId()); err != nil {
				t.Fatal(err)
			}
			refreshed, err := client.RefreshTokens(authContext(t.Context(), key), request)
			if err != nil || refreshed.GetTokens().GetRefreshToken() == "" || refreshed.GetTokens().GetRefreshToken() == original {
				t.Fatal("refresh retry after repair", refreshed, err)
			}
		})
	}
}

func apiTransactionCounts(t *testing.T, env *apitest.Env) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, table := range []string{"app_user", "auth_session", "refresh_token", "user_consent", "outbox_events", "idempotency_keys"} {
		var n int
		if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
			t.Fatal(table, err)
		}
		counts[table] = n
	}
	return counts
}

func requireAPIConstraintRefusal(t *testing.T, err error, code codes.Code, reason string, private ...string) {
	t.Helper()
	if err == nil || status.Code(err) != code || apitest.Reason(err) != reason {
		t.Fatal("wrong constraint refusal", code, reason, err)
	}
	if code == codes.Internal && (status.Convert(err).Message() != codes.Internal.String() || len(status.Convert(err).Details()) != 0) {
		t.Fatal("internal refusal is not bare", err)
	}
	for _, value := range private {
		if strings.Contains(err.Error(), value) {
			t.Fatal("constraint refusal leaked private state", err)
		}
	}
}
