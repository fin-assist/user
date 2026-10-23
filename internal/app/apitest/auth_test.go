package apitest_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	identity "github.com/fin-assist/go-identity"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/app"
	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/ratelimit"
	"github.com/fin-assist/user/internal/token"
)

var authKeys atomic.Uint64

func authKey() string { return fmt.Sprintf("019c7714-3b77-74d1-9866-%012x", authKeys.Add(1)) }
func authContext(ctx context.Context, key string) context.Context {
	return metadata.AppendToOutgoingContext(apitest.Caller(ctx, "bank-bff"), "x-client-ip", "203.0.113.7", "idempotency-key", key)
}
func seedRegistration(t *testing.T, env *apitest.Env, raw, phone string, verified time.Time) int32 {
	t.Helper()
	h := token.Sum(raw)
	v := token.Sum("verification:" + raw)
	var id int32
	err := env.App.DB.Primary().QueryRow(t.Context(), `INSERT INTO phone_verification(phone,token_hash,provider,provider_call_id,created_at,expires_at,verified_at,is_new_user,registration_token_hash,result_ciphertext,result_key_id,released_at) VALUES($1,$2,'test',$3,$4,$5,$6,true,$7,$8,'test',$9) RETURNING id`, phone, v[:], "call:"+raw, verified.Add(-time.Minute), verified.Add(4*time.Minute), verified, h[:], []byte("test-ciphertext"), time.Now().UTC()).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func registrationRequest(raw string) *userv1.RegisterRequest {
	return &userv1.RegisterRequest{RegistrationToken: raw, PdConsentAccepted: true, PdConsentVersion: "v1"}
}
func registerAPI(t *testing.T, env *apitest.Env, raw, phone string) (*userv1.RegisterResponse, string) {
	t.Helper()
	seedRegistration(t, env, raw, phone, time.Now().UTC().Add(-6*time.Minute))
	key := authKey()
	r, err := userv1.NewAuthServiceClient(env.Conn).Register(authContext(t.Context(), key), registrationRequest(raw))
	if err != nil {
		t.Fatal(err)
	}
	return r, key
}
func checkAccessJWT(t *testing.T, env *apitest.Env, raw, id string) {
	t.Helper()
	cache, err := identity.NewKeyCache(env.App.Signing, time.Now, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	v, err := identity.NewUserVerifier(cache, identity.VerifyOptions{Issuer: "apitest-user", Audience: "apitest-client", Clock: time.Now})
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.VerifyUser(t.Context(), raw)
	if err != nil || got != id {
		t.Fatal("access JWT identity", got, err)
	}
}

func TestAuthAPIRegisterAtomicReplayAndInitialTimezone(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	c := userv1.NewAuthServiceClient(env.Conn)
	raw := "reg-new"
	id := seedRegistration(t, env, raw, "+79001234567", time.Now().UTC().Add(-6*time.Minute))
	key := authKey()
	req := registrationRequest(raw)
	req.Timezone = stringPtr("Asia/Tokyo")
	first, err := c.Register(metadata.AppendToOutgoingContext(authContext(t.Context(), key), "authorization", "Bearer dev:"+profileUserB), req)
	if err != nil {
		t.Fatal(err)
	}
	checkAccessJWT(t, env, first.GetTokens().GetAccessToken(), first.GetUserId())
	b, err := tokenDecode(first.GetTokens().GetRefreshToken())
	if err != nil || len(b) != 32 {
		t.Fatal("refresh entropy", err)
	}
	second, err := c.Register(authContext(t.Context(), key), req)
	if err != nil || !proto.Equal(first, second) {
		t.Fatal("registration replay", err)
	}
	changed := proto.Clone(req).(*userv1.RegisterRequest)
	changed.Timezone = stringPtr("UTC")
	if _, err := c.Register(authContext(t.Context(), key), changed); apitest.Reason(err) != "IDEMPOTENCY_CONFLICT" {
		t.Fatal(err)
	}
	if _, err := c.Register(authContext(t.Context(), authKey()), req); apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatal("consumed registration token", err)
	}
	var users, consents, sessions, events, version, hits int
	var zone string
	var closed, wiped bool
	err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM app_user),(SELECT count(*) FROM user_consent WHERE type='personal_data'),(SELECT count(*) FROM auth_session),(SELECT count(*) FROM outbox_events),settings_version,timezone,(SELECT closed_at IS NOT NULL AND user_id=app_user.id FROM phone_verification WHERE id=$2),(SELECT result_ciphertext IS NULL AND result_key_id IS NULL FROM phone_verification WHERE id=$2),(SELECT hits FROM rate_limit_window WHERE scope='register_ip:60') FROM app_user WHERE id=$1`, first.GetUserId(), id).Scan(&users, &consents, &sessions, &events, &version, &zone, &closed, &wiped, &hits)
	if err != nil || users != 1 || consents != 1 || sessions != 1 || events != 1 || version != 1 || zone != "Asia/Tokyo" || !closed || !wiped || hits != 2 {
		t.Fatal("atomic registration", users, consents, sessions, events, version, zone, closed, wiped, hits, err)
	}
	var payload []byte
	var encrypted bool
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT payload,encrypted FROM idempotency_keys WHERE endpoint=$1 AND key=$2`, userv1.AuthService_Register_FullMethodName, key).Scan(&payload, &encrypted); err != nil || !encrypted || bytes.Contains(payload, []byte(first.GetTokens().GetRefreshToken())) {
		t.Fatal("plaintext replay", err)
	}
	var lifetime time.Duration
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT extract(epoch FROM expires_at-issued_at)::bigint FROM refresh_token`).Scan(&lifetime); err != nil || lifetime*time.Second != 60*24*time.Hour {
		t.Fatal(lifetime, err)
	}
}

func TestAuthAPIRegisterErrorsDoNotConsumeTheResult(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	c := userv1.NewAuthServiceClient(env.Conn)
	seedRegistration(t, env, "valid", "+79001234567", time.Now().UTC().Add(-6*time.Minute))
	seedRegistration(t, env, "expired", "+79001234568", time.Now().UTC().Add(-16*time.Minute))
	for _, tc := range []struct {
		name string
		req  *userv1.RegisterRequest
		want string
	}{
		{"bad-token", registrationRequest("unknown"), "INVALID_TOKEN"},
		{"expired", registrationRequest("expired"), "TOKEN_EXPIRED"},
		{"no-consent", &userv1.RegisterRequest{RegistrationToken: "valid", PdConsentVersion: "v1"}, "CONSENT_REQUIRED"},
		{"old-document", &userv1.RegisterRequest{RegistrationToken: "valid", PdConsentAccepted: true, PdConsentVersion: "old"}, "CONSENT_OUTDATED"},
		{"timezone", &userv1.RegisterRequest{RegistrationToken: "valid", PdConsentAccepted: true, PdConsentVersion: "v1", Timezone: stringPtr("Local")}, "VALIDATION_ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.Register(authContext(t.Context(), authKey()), tc.req)
			if apitest.Reason(err) != tc.want {
				t.Fatal(err)
			}
			if tc.want == "VALIDATION_ERROR" {
				found := false
				for _, d := range status.Convert(err).Details() {
					if b, ok := d.(*errdetails.BadRequest); ok {
						for _, v := range b.GetFieldViolations() {
							found = found || v.GetField() == "timezone"
						}
					}
				}
				if !found {
					t.Fatal("field detail absent")
				}
			}
		})
	}
	var n int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM app_user`).Scan(&n); err != nil || n != 0 {
		t.Fatal("failed registration wrote user", n, err)
	}
	if _, err := c.Register(authContext(t.Context(), authKey()), registrationRequest("valid")); err != nil {
		t.Fatal("error consumed token", err)
	}
}

func TestAuthAPIConcurrentRegisterSameKeyIsOneEffect(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	seedRegistration(t, env, "reg", "+79001234567", time.Now().UTC().Add(-6*time.Minute))
	c := userv1.NewAuthServiceClient(env.Conn)
	key := authKey()
	start := make(chan struct{})
	results := make(chan *userv1.RegisterResponse, 2)
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			r, err := c.Register(authContext(t.Context(), key), registrationRequest("reg"))
			results <- r
			errs <- err
		}()
	}
	close(start)
	a, b := <-results, <-results
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(a, b) {
		t.Fatal("different cached registration results")
	}
	var sessions, hits int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM auth_session),(SELECT hits FROM rate_limit_window WHERE scope='register_ip:60')`).Scan(&sessions, &hits); err != nil || sessions != 1 || hits != 1 {
		t.Fatal("double registration effect", sessions, hits, err)
	}
}

func TestAuthAPIConcurrentRegisterDifferentKeysConsumesTokenOnce(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	seedRegistration(t, env, "reg", "+79001234567", time.Now().UTC().Add(-6*time.Minute))
	c := userv1.NewAuthServiceClient(env.Conn)
	blocker, err := env.App.DB.Primary().Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	if _, err := blocker.Exec(t.Context(), `SELECT version FROM consent_document WHERE type='personal_data' AND is_current FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	type result struct {
		response *userv1.RegisterResponse
		err      error
	}
	done := make(chan result, 2)
	for range 2 {
		key := authKey()
		go func() {
			r, err := c.Register(authContext(t.Context(), key), registrationRequest("reg"))
			done <- result{r, err}
		}()
	}
	// Both requests have already read the same open verification, and now wait
	// at the document read before either can create an account or consume it.
	eventually(t, func() bool {
		var n int
		err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND application_name='user-auth' AND wait_event_type='Lock' AND query LIKE 'SELECT version FROM consent_document%'`).Scan(&n)
		return err == nil && n == 2
	})
	if err := blocker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	successes, refusals := 0, 0
	var user string
	for range 2 {
		r := <-done
		if r.err == nil {
			successes++
			user = r.response.GetUserId()
		} else if apitest.Reason(r.err) == "INVALID_TOKEN" {
			refusals++
		} else {
			t.Fatal(r.err)
		}
	}
	if successes != 1 || refusals != 1 || user == "" {
		t.Fatal("registration token consumed more than once", successes, refusals)
	}
	var users, sessions, events, consents, closed, hits int
	err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM app_user),(SELECT count(*) FROM auth_session),(SELECT count(*) FROM outbox_events),(SELECT count(*) FROM user_consent WHERE type='personal_data'),(SELECT count(*) FROM phone_verification WHERE closed_at IS NOT NULL AND user_id=$1),(SELECT hits FROM rate_limit_window WHERE scope='register_ip:60')`, user).Scan(&users, &sessions, &events, &consents, &closed, &hits)
	if err != nil || users != 1 || sessions != 1 || events != 1 || consents != 1 || closed != 1 || hits != 2 {
		t.Fatal("duplicate registration effects", users, sessions, events, consents, closed, hits, err)
	}
}

func TestAuthAPICleanupDrainsBatchesAndKeepsLiveState(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	registered, _ := registerAPI(t, env, "reg", "+79001234567")
	if _, err := env.App.DB.Primary().Exec(t.Context(), `INSERT INTO idempotency_keys(scope,endpoint,key,request_hash,expires_at) SELECT '00000000-0000-0000-0000-000000000000',$1,md5(n::text)::uuid,$2,$3 FROM generate_series(1,1001) n`, userv1.AuthService_RefreshTokens_FullMethodName, make([]byte, 32), time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `INSERT INTO auth_replay_owner(endpoint,key,user_id) SELECT endpoint,key,$1 FROM idempotency_keys WHERE endpoint=$2`, registered.GetUserId(), userv1.AuthService_RefreshTokens_FullMethodName); err != nil {
		t.Fatal(err)
	}
	var run func(context.Context) error
	for _, job := range env.App.Jobs {
		if job.Name == "auth-cleanup" {
			run = job.Run
		}
	}
	if run == nil {
		t.Fatal("cleanup missing")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	eventually(t, func() bool {
		var keys, owners int
		err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM idempotency_keys),(SELECT count(*) FROM auth_replay_owner)`).Scan(&keys, &owners)
		return err == nil && keys == 1 && owners == 1
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cleanup did not stop")
	}
	if _, err := userv1.NewAuthServiceClient(env.Conn).RefreshTokens(authContext(t.Context(), authKey()), &userv1.RefreshTokensRequest{RefreshToken: registered.GetTokens().GetRefreshToken()}); err != nil {
		t.Fatal("cleanup removed live session", err)
	}
}

func TestAuthAPIRequestInProgressDoesNotConsumeSecondAttempt(t *testing.T) {
	for _, method := range []string{"register", "refresh"} {
		t.Run(method, func(t *testing.T) {
			env := apitest.Start(t)
			seedDocuments(t, env)
			registered, _ := registerAPI(t, env, "initial", "+79001234567")
			c := userv1.NewAuthServiceClient(env.Conn)
			blocker, err := env.App.DB.Primary().Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = blocker.Rollback(context.Background()) }()
			var call func(context.Context) error
			scope := "refresh_ip:60"
			expected := 1
			query := `SELECT id FROM auth_session FOR UPDATE`
			if method == "register" {
				seedRegistration(t, env, "next", "+79001234567", time.Now().UTC().Add(-6*time.Minute))
				scope = "register_ip:60"
				expected = 2
				query = `SELECT id FROM app_user FOR UPDATE`
				call = func(ctx context.Context) error { _, err := c.Register(ctx, registrationRequest("next")); return err }
			} else {
				call = func(ctx context.Context) error {
					_, err := c.RefreshTokens(ctx, &userv1.RefreshTokensRequest{RefreshToken: registered.GetTokens().GetRefreshToken()})
					return err
				}
			}
			if _, err := blocker.Exec(t.Context(), query); err != nil {
				t.Fatal(err)
			}
			key := authKey()
			done := make(chan error, 1)
			go func() { done <- call(authContext(t.Context(), key)) }()
			eventually(t, func() bool {
				var hits int
				err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT hits FROM rate_limit_window WHERE scope=$1`, scope).Scan(&hits)
				return err == nil && hits == expected
			})
			if err := call(authContext(t.Context(), key)); apitest.Reason(err) != "REQUEST_IN_PROGRESS" {
				t.Fatal("busy key", err)
			}
			if err := blocker.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if err := call(authContext(t.Context(), key)); err != nil {
				t.Fatal("completed key not replayed", err)
			}
			var hits int
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT hits FROM rate_limit_window WHERE scope=$1`, scope).Scan(&hits); err != nil || hits != expected {
				t.Fatal("busy/replay consumed attempts", hits, err)
			}
		})
	}
}

type authFailureLog struct{ errors chan string }

func (h authFailureLog) Enabled(context.Context, slog.Level) bool { return true }
func (h authFailureLog) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		select {
		case h.errors <- r.Message:
		default:
		}
	}
	return nil
}
func (h authFailureLog) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h authFailureLog) WithGroup(string) slog.Handler      { return h }

func TestAuthAPICleanupFailureLogsAndStops(t *testing.T) {
	for _, table := range []string{"idempotency_keys", "auth_replay_owner", "refresh_token"} {
		t.Run(table, func(t *testing.T) {
			env := apitest.Start(t)
			log := authFailureLog{errors: make(chan string, 1)}
			a, err := app.New(t.Context(), env.App.Cfg, slog.New(log), prometheus.NewRegistry())
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			// Table is from this test's fixed list, never from request input.
			if _, err := env.App.DB.Primary().Exec(t.Context(), `DROP TABLE `+table); err != nil {
				t.Fatal(err)
			}
			var run func(context.Context) error
			for _, job := range a.Jobs {
				if job.Name == "auth-cleanup" {
					run = job.Run
				}
			}
			if run == nil {
				t.Fatal("cleanup absent")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- run(ctx) }()
			select {
			case msg := <-log.errors:
				if msg != "job iteration failed" {
					t.Fatal(msg)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cleanup failure not logged")
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("failed cleanup did not stop")
			}
		})
	}
}

func TestAuthAPIConcurrentRegistrationsKeepOneAccountAndSettings(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	c := userv1.NewAuthServiceClient(env.Conn)
	for _, raw := range []string{"first", "second"} {
		seedRegistration(t, env, raw, "+79001234567", time.Now().UTC().Add(-6*time.Minute))
	}
	start := make(chan struct{})
	results := make(chan *userv1.RegisterResponse, 2)
	errs := make(chan error, 2)
	for _, raw := range []string{"first", "second"} {
		go func() {
			<-start
			r, err := c.Register(authContext(t.Context(), authKey()), registrationRequest(raw))
			results <- r
			errs <- err
		}()
	}
	close(start)
	a, b := <-results, <-results
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if a.GetUserId() != b.GetUserId() || a.GetTokens().GetRefreshToken() == b.GetTokens().GetRefreshToken() {
		t.Fatal("two accounts or shared session")
	}
	var users, sessions, consents, events int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM app_user),(SELECT count(*) FROM auth_session),(SELECT count(*) FROM user_consent),(SELECT count(*) FROM outbox_events)`).Scan(&users, &sessions, &consents, &events); err != nil || users != 1 || sessions != 2 || consents != 1 || events != 1 {
		t.Fatal(users, sessions, consents, events, err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE app_user SET timezone='UTC',theme='dark' WHERE id=$1`, a.GetUserId()); err != nil {
		t.Fatal(err)
	}
	seedRegistration(t, env, "third", "+79001234567", time.Now().UTC().Add(-6*time.Minute))
	req := registrationRequest("third")
	req.Timezone = stringPtr("Asia/Tokyo")
	third, err := c.Register(authContext(t.Context(), authKey()), req)
	if err != nil || third.GetUserId() != a.GetUserId() {
		t.Fatal(err)
	}
	var zone, theme string
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT timezone,theme FROM app_user WHERE id=$1`, a.GetUserId()).Scan(&zone, &theme); err != nil || zone != "UTC" || theme != "dark" {
		t.Fatal("existing settings changed", zone, theme, err)
	}
}

func TestAuthAPIRefreshReplayReuseAndLogout(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	c := userv1.NewAuthServiceClient(env.Conn)
	registered, _ := registerAPI(t, env, "reg", "+79001234567")
	old := registered.GetTokens().GetRefreshToken()
	key := authKey()
	req := &userv1.RefreshTokensRequest{RefreshToken: old}
	first, err := c.RefreshTokens(authContext(t.Context(), key), req)
	if err != nil || first.GetTokens().GetRefreshToken() == old {
		t.Fatal(err)
	}
	checkAccessJWT(t, env, first.GetTokens().GetAccessToken(), registered.GetUserId())
	replay, err := c.RefreshTokens(authContext(t.Context(), key), req)
	if err != nil || !proto.Equal(first, replay) {
		t.Fatal("refresh replay", err)
	}
	if _, err := c.RefreshTokens(authContext(t.Context(), key), &userv1.RefreshTokensRequest{RefreshToken: "other"}); apitest.Reason(err) != "IDEMPOTENCY_CONFLICT" {
		t.Fatal(err)
	}
	var hits, links int
	var revoked bool
	var encrypted bool
	var seconds float64
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT hits FROM rate_limit_window WHERE scope='refresh_ip:60'),(SELECT count(*) FROM refresh_token),(SELECT revoked_at IS NOT NULL FROM auth_session),(SELECT encrypted FROM idempotency_keys WHERE endpoint=$1 AND key=$2),(SELECT extract(epoch FROM expires_at-now()) FROM idempotency_keys WHERE endpoint=$1 AND key=$2)`, userv1.AuthService_RefreshTokens_FullMethodName, key).Scan(&hits, &links, &revoked, &encrypted, &seconds); err != nil || hits != 1 || links != 2 || revoked || !encrypted || seconds > 60 || seconds < 30 {
		t.Fatal(hits, links, revoked, encrypted, seconds, err)
	}
	if _, err := c.RefreshTokens(authContext(t.Context(), authKey()), req); apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatal("old secret did not revoke", err)
	}
	if _, err := c.RefreshTokens(authContext(t.Context(), authKey()), &userv1.RefreshTokensRequest{RefreshToken: first.GetTokens().GetRefreshToken()}); apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatal("successor still works", err)
	}
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT revoked_at IS NOT NULL FROM auth_session`).Scan(&revoked); err != nil || !revoked {
		t.Fatal("401 rolled back revocation", err)
	}
	for range 2 {
		if _, err := c.Logout(apitest.Caller(t.Context(), "bank-bff"), &userv1.LogoutRequest{RefreshToken: old}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.RefreshTokens(authContext(t.Context(), authKey()), req); apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatal(err)
	}
	checkAccessJWT(t, env, first.GetTokens().GetAccessToken(), registered.GetUserId())
}

func TestAuthAPIBadSecretsHitLimitsBeforeTokensAndKeysBeforeLimits(t *testing.T) {
	env := apitest.Start(t, func(c *config.Config) {
		c.Limits[ratelimit.RefreshIP] = ratelimit.Rule{Limit: 2, Window: time.Hour}
		c.Limits[ratelimit.RegisterIP] = ratelimit.Rule{Limit: 1, Window: time.Hour}
	})
	c := userv1.NewAuthServiceClient(env.Conn)
	req := &userv1.RefreshTokensRequest{RefreshToken: "bad"}
	for range 2 {
		if _, err := c.RefreshTokens(authContext(t.Context(), authKey()), req); apitest.Reason(err) != "INVALID_TOKEN" {
			t.Fatal(err)
		}
	}
	if _, err := c.RefreshTokens(authContext(t.Context(), authKey()), req); apitest.Reason(err) != "RATE_LIMITED" {
		t.Fatal(err)
	}
	if _, err := c.Register(authContext(t.Context(), authKey()), registrationRequest("bad")); apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatal(err)
	}
	if _, err := c.Register(authContext(t.Context(), authKey()), registrationRequest("bad")); apitest.Reason(err) != "RATE_LIMITED" {
		t.Fatal(err)
	}
	for _, key := range []string{"", "not-uuid"} {
		if _, err := c.RefreshTokens(authContext(t.Context(), key), req); apitest.Reason(err) != "VALIDATION_ERROR" {
			t.Fatal("key must precede limit", err)
		}
	}
	var hits int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT hits FROM rate_limit_window WHERE scope='refresh_ip:3600'`).Scan(&hits); err != nil || hits != 2 {
		t.Fatal(hits, err)
	}
	if got := counterValue(t, env, "finassist_rate_limit_consumed_total", "scope", "refresh_ip"); got != 2 {
		t.Fatal("committed metrics", got)
	}
}

func TestAuthAPIConcurrentSameAndDifferentRefreshKeys(t *testing.T) {
	for _, sameKey := range []bool{true, false} {
		t.Run(fmt.Sprint(sameKey), func(t *testing.T) {
			env := apitest.Start(t)
			seedDocuments(t, env)
			r, _ := registerAPI(t, env, "reg", "+79001234567")
			c := userv1.NewAuthServiceClient(env.Conn)
			key := authKey()
			start := make(chan struct{})
			results := make(chan *userv1.RefreshTokensResponse, 2)
			errs := make(chan error, 2)
			for range 2 {
				useKey := key
				if !sameKey {
					useKey = authKey()
				}
				go func() {
					<-start
					out, err := c.RefreshTokens(authContext(t.Context(), useKey), &userv1.RefreshTokensRequest{RefreshToken: r.GetTokens().GetRefreshToken()})
					results <- out
					errs <- err
				}()
			}
			close(start)
			a, b := <-results, <-results
			ea, eb := <-errs, <-errs
			if sameKey {
				if ea != nil || eb != nil || !proto.Equal(a, b) {
					t.Fatal(ea, eb)
				}
			} else {
				successes, revocations := 0, 0
				for _, err := range []error{ea, eb} {
					if err == nil {
						successes++
					} else if apitest.Reason(err) == "TOKEN_REVOKED" {
						revocations++
					} else {
						t.Fatal(err)
					}
				}
				if successes != 1 || revocations != 1 {
					t.Fatal("unexpected concurrent outcomes", ea, eb)
				}
			}
			var revoked bool
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT revoked_at IS NOT NULL FROM auth_session`).Scan(&revoked); err != nil || revoked == sameKey {
				t.Fatal(revoked, err)
			}
		})
	}
}

func TestAuthAPIAccessTableErrorsAndDeletedReplay(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	r, key := registerAPI(t, env, "reg", "+79001234567")
	c := userv1.NewAuthServiceClient(env.Conn)
	refreshKey := authKey()
	refreshRequest := &userv1.RefreshTokensRequest{RefreshToken: r.GetTokens().GetRefreshToken()}
	if _, err := c.RefreshTokens(authContext(t.Context(), refreshKey), refreshRequest); err != nil {
		t.Fatal(err)
	}
	for _, ctx := range []context.Context{t.Context(), apitest.Caller(t.Context(), "assistant")} {
		if _, err := c.Register(ctx, registrationRequest("reg")); status.Code(err) != codes.Unauthenticated && status.Code(err) != codes.PermissionDenied {
			t.Fatal(err)
		}
		if _, err := c.RefreshTokens(ctx, &userv1.RefreshTokensRequest{}); status.Code(err) != codes.Unauthenticated && status.Code(err) != codes.PermissionDenied {
			t.Fatal(err)
		}
		if _, err := c.Logout(ctx, &userv1.LogoutRequest{}); status.Code(err) != codes.Unauthenticated && status.Code(err) != codes.PermissionDenied {
			t.Fatal(err)
		}
	}
	if err := env.App.DB.Run(t.Context(), func(ctx context.Context) error {
		return env.App.Tombstones.Purge(ctx, r.GetUserId(), func(context.Context, string) error { return nil })
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Register(authContext(t.Context(), key), registrationRequest("reg")); apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatal("deleted cached result leaked", err)
	}
	if _, err := c.RefreshTokens(authContext(t.Context(), refreshKey), refreshRequest); apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatal("deleted cached refresh leaked", err)
	}
	if _, err := c.RefreshTokens(authContext(t.Context(), authKey()), &userv1.RefreshTokensRequest{RefreshToken: r.GetTokens().GetRefreshToken()}); apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(authContext(t.Context(), authKey()))
	cancel()
	if _, err := c.RefreshTokens(ctx, &userv1.RefreshTokensRequest{}); status.Code(err) != codes.Canceled {
		t.Fatal(err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `DROP TABLE refresh_token`); err != nil {
		t.Fatal(err)
	}
	_, err := c.RefreshTokens(authContext(t.Context(), authKey()), &userv1.RefreshTokensRequest{RefreshToken: "test"})
	if status.Code(err) != codes.Internal || apitest.Reason(err) != "" || strings.Contains(status.Convert(err).Message(), "refresh_token") {
		t.Fatal("SQL error exposed", err)
	}
}

func TestAuthAPICancelAfterEffectStillCommitsAndReplays(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	r, _ := registerAPI(t, env, "reg", "+79001234567")
	c := userv1.NewAuthServiceClient(env.Conn)
	blocker, err := env.App.DB.Primary().Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	if _, err := blocker.Exec(t.Context(), `SELECT id FROM auth_session FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	key := authKey()
	req := &userv1.RefreshTokensRequest{RefreshToken: r.GetTokens().GetRefreshToken()}
	ctx, cancel := context.WithCancel(authContext(t.Context(), key))
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := c.RefreshTokens(ctx, req); done <- err }()
	eventually(t, func() bool {
		var n int
		err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM rate_limit_window WHERE scope='refresh_ip:60' AND hits=1`).Scan(&n)
		return err == nil && n == 1
	})
	cancel()
	if err := <-done; status.Code(err) != codes.Canceled {
		t.Fatal(err)
	}
	if err := blocker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		var n int
		err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM idempotency_keys WHERE key=$1 AND done`, key).Scan(&n)
		return err == nil && n == 1
	})
	got, err := c.RefreshTokens(authContext(t.Context(), key), req)
	if err != nil || got.GetTokens().GetRefreshToken() == req.GetRefreshToken() {
		t.Fatal("cancelled effect not replayed", err)
	}
	var links int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM refresh_token`).Scan(&links); err != nil || links != 2 {
		t.Fatal("double rotation", links, err)
	}
}

func TestAuthAPIFailedReplayBindingRollsBackEveryRegistrationEffect(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	c := userv1.NewAuthServiceClient(env.Conn)
	seedRegistration(t, env, "reg", "+79001234567", time.Now().UTC().Add(-6*time.Minute))
	if _, err := env.App.DB.Primary().Exec(t.Context(), `DROP TABLE auth_replay_owner`); err != nil {
		t.Fatal(err)
	}
	_, err := c.Register(authContext(t.Context(), authKey()), registrationRequest("reg"))
	if status.Code(err) != codes.Internal || apitest.Reason(err) != "" {
		t.Fatal(err)
	}
	var users, sessions, events, closed, hits int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM app_user),(SELECT count(*) FROM auth_session),(SELECT count(*) FROM outbox_events),(SELECT count(*) FROM phone_verification WHERE closed_at IS NOT NULL),(SELECT hits FROM rate_limit_window WHERE scope='register_ip:60')`).Scan(&users, &sessions, &events, &closed, &hits); err != nil || users != 0 || sessions != 0 || events != 0 || closed != 0 || hits != 1 {
		t.Fatal("partial registration", users, sessions, events, closed, hits, err)
	}
}

func TestAuthAPIExpiredRefreshReplayAndLiveKeyBeforeLimit(t *testing.T) {
	env := apitest.Start(t, func(c *config.Config) { c.Limits[ratelimit.RefreshIP] = ratelimit.Rule{Limit: 2, Window: time.Hour} })
	seedDocuments(t, env)
	r, _ := registerAPI(t, env, "reg", "+79001234567")
	c := userv1.NewAuthServiceClient(env.Conn)
	key := authKey()
	req := &userv1.RefreshTokensRequest{RefreshToken: r.GetTokens().GetRefreshToken()}
	first, err := c.RefreshTokens(authContext(t.Context(), key), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.RefreshTokens(authContext(t.Context(), authKey()), &userv1.RefreshTokensRequest{RefreshToken: "bad"}); apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatal(err)
	}
	if _, err := c.RefreshTokens(authContext(t.Context(), authKey()), &userv1.RefreshTokensRequest{RefreshToken: "bad"}); apitest.Reason(err) != "RATE_LIMITED" {
		t.Fatal(err)
	}
	if replay, err := c.RefreshTokens(authContext(t.Context(), key), req); err != nil || !proto.Equal(first, replay) {
		t.Fatal("limit overrode replay", err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE idempotency_keys SET expires_at=$1 WHERE key=$2`, time.Now().Add(-time.Second), key); err != nil {
		t.Fatal(err)
	}
	// After the cache TTL, this key executes again; clear just this test's limit.
	if _, err := env.App.DB.Primary().Exec(t.Context(), `DELETE FROM rate_limit_window WHERE scope='refresh_ip:3600'`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RefreshTokens(authContext(t.Context(), key), req); apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatal("expired key masked reuse", err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE refresh_token SET expires_at=$1`, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RefreshTokens(authContext(t.Context(), authKey()), &userv1.RefreshTokensRequest{RefreshToken: first.GetTokens().GetRefreshToken()}); apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatal("expired refresh accepted", err)
	}
}

func tokenDecode(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

func counterValue(t *testing.T, env *apitest.Env, name, label, value string) float64 {
	t.Helper()
	metrics, err := env.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range metrics {
		if family.GetName() != name {
			continue
		}
		for _, m := range family.Metric {
			for _, l := range m.Label {
				if l.GetName() == label && l.GetValue() == value {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatal("metric absent", name)
	return 0
}
