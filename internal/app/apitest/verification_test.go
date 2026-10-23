package apitest_test

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/user/internal/app"
	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/ratelimit"
	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/verification"
)

func phoneContext(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(apitest.Caller(ctx, "bank-bff"), "x-client-ip", "203.0.113.7")
}
func pendingPhoneAPI(t *testing.T, env *apitest.Env, raw, phone string, expires time.Time) int32 {
	t.Helper()
	h := token.Sum(raw)
	var id int32
	err := env.App.DB.Primary().QueryRow(t.Context(), `INSERT INTO phone_verification(phone,token_hash,provider,provider_call_id,created_at,expires_at) VALUES($1,$2,'test',$3,$4,$5) RETURNING id`, phone, h[:], "call:"+raw, expires.Add(-5*time.Minute), expires).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
func confirmPhoneAPI(t *testing.T, env *apitest.Env, raw, phone string) {
	t.Helper()
	changed, err := env.App.Verifications.Confirm(t.Context(), verification.Proof{Provider: "test", CallID: "call:" + raw, Phone: phone})
	if err != nil || !changed {
		t.Fatal("confirmation failed", changed, err)
	}
}
func phoneJob(t *testing.T, env *apitest.Env) (context.CancelFunc, <-chan error) {
	return phoneBackgroundJob(t, env, "phone-notifications")
}
func phoneBackgroundJob(t *testing.T, env *apitest.Env, name string) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	found := false
	for _, job := range env.App.Jobs {
		if job.Name == name {
			found = true
			go func() { done <- job.Run(ctx); close(done) }()
			break
		}
	}
	if !found {
		cancel()
		t.Fatal("phone notifications not wired")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("phone listener did not stop")
		}
	})
	return cancel, done
}
func phoneMetric(t *testing.T, env *apitest.Env, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := env.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.Metric {
			match := true
			for _, l := range m.Label {
				if l.GetName() != "reason" {
					t.Fatal("unexpected phone label", l.GetName())
				}
				if labels[l.GetName()] != l.GetValue() {
					match = false
				}
			}
			if match {
				if m.Gauge != nil {
					return m.Gauge.GetValue()
				}
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}
func secondPhoneInstance(t *testing.T, first *apitest.Env) *apitest.Env {
	t.Helper()
	reg := prometheus.NewRegistry()
	a, err := app.New(t.Context(), first.App.Cfg, slog.New(slog.DiscardHandler), reg)
	if err != nil {
		t.Fatal(err)
	}
	lis := bufconn.Listen(1 << 20)
	go func() { _ = a.GRPC.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///second", grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		a.GRPC.Shutdown(ctx, 0)
		a.Close()
	})
	return &apitest.Env{App: a, Conn: conn, DSN: first.DSN, Registry: reg}
}
func openPhoneWatch(t *testing.T, env *apitest.Env, ctx context.Context, raw string) grpc.ServerStreamingClient[userv1.WatchPhoneVerificationResponse] {
	t.Helper()
	stream, err := userv1.NewAuthServiceClient(env.Conn).WatchPhoneVerification(ctx, &userv1.WatchPhoneVerificationRequest{VerificationToken: raw})
	if err != nil {
		t.Fatal(err)
	}
	return stream
}
func watchReason(t *testing.T, env *apitest.Env, ctx context.Context, raw, want string) {
	t.Helper()
	stream := openPhoneWatch(t, env, ctx, raw)
	m, err := stream.Recv()
	if m != nil || apitest.Reason(err) != want {
		t.Fatal("unexpected initial refusal", m, err)
	}
}
func TestPhoneWatchNewUserRegisterAndReplay(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	phoneJob(t, env)
	id := pendingPhoneAPI(t, env, "new-phone", "+79001234567", time.Now().Add(time.Minute))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stream := openPhoneWatch(t, env, phoneContext(ctx), "new-phone")
	pending, err := stream.Recv()
	if err != nil || pending.GetStatus() != userv1.VerificationStatus_VERIFICATION_STATUS_PENDING || pending.IsNewUser != nil || pending.Tokens != nil || pending.RegistrationToken != nil {
		t.Fatal(pending, err)
	}
	if changed, err := env.App.Verifications.Confirm(t.Context(), verification.Proof{Provider: "test", CallID: "call:new-phone", Phone: "+79001234568"}); changed || !errors.Is(err, verification.ErrProof) {
		t.Fatal(changed, err)
	}
	confirmPhoneAPI(t, env, "new-phone", "+79001234567")
	verified, err := stream.Recv()
	if err != nil || verified.GetStatus() != userv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED || !verified.GetIsNewUser() || verified.Tokens != nil {
		t.Fatal(verified, err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(verified.GetRegistrationToken())
	if err != nil || len(raw) != 32 {
		t.Fatal("registration secret shape", err)
	}
	if _, err = stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatal("terminal stream left open", err)
	}
	replay := openPhoneWatch(t, env, phoneContext(ctx), "new-phone")
	again, err := replay.Recv()
	if err != nil || !proto.Equal(verified, again) {
		t.Fatal("new-user result changed", err)
	}
	if _, err = replay.Recv(); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	// The result remains valid independently of the request's five-minute expiry.
	if _, err = env.App.DB.Primary().Exec(ctx, `UPDATE phone_verification SET expires_at=$2,released_at=$3 WHERE id=$1`, id, time.Now().Add(-time.Second), time.Now()); err != nil {
		t.Fatal(err)
	}
	afterExpiry := openPhoneWatch(t, env, phoneContext(ctx), "new-phone")
	again, err = afterExpiry.Recv()
	if err != nil || !proto.Equal(verified, again) {
		t.Fatal("result ended at request expiry", err)
	}
	registration, err := userv1.NewAuthServiceClient(env.Conn).Register(authContext(ctx, authKey()), registrationRequest(verified.GetRegistrationToken()))
	if err != nil {
		t.Fatal(err)
	}
	checkAccessJWT(t, env, registration.GetTokens().GetAccessToken(), registration.GetUserId())
	watchReason(t, env, phoneContext(ctx), "new-phone", "INVALID_TOKEN")
	var sessions int
	var wiped bool
	err = env.App.DB.Primary().QueryRow(ctx, `SELECT (SELECT count(*) FROM auth_session),result_ciphertext IS NULL AND result_key_id IS NULL AND closed_at IS NOT NULL FROM phone_verification WHERE id=$1`, id).Scan(&sessions, &wiped)
	if err != nil || sessions != 1 || !wiped {
		t.Fatal("registration not atomic with result consumption", sessions, wiped, err)
	}
	eventually(t, func() bool { return phoneMetric(t, env, "finassist_phone_watch_active_streams", nil) == 0 })
}
func TestPhoneWatchExistingUserAcrossInstancesAndRefresh(t *testing.T) {
	first := apitest.Start(t)
	seedDocuments(t, first)
	user, _ := registerAPI(t, first, "existing-account", "+79001234567")
	second := secondPhoneInstance(t, first)
	phoneJob(t, first)
	phoneJob(t, second)
	pendingPhoneAPI(t, first, "existing-phone", "+79001234567", time.Now().Add(time.Minute))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stream := openPhoneWatch(t, second, phoneContext(ctx), "existing-phone")
	if m, err := stream.Recv(); err != nil || m.GetStatus() != userv1.VerificationStatus_VERIFICATION_STATUS_PENDING {
		t.Fatal(m, err)
	}
	confirmPhoneAPI(t, first, "existing-phone", "+79001234567")
	result, err := stream.Recv()
	if err != nil || result.IsNewUser == nil || result.GetIsNewUser() || result.GetTokens().GetRefreshToken() == "" || result.RegistrationToken != nil {
		t.Fatal(result, err)
	}
	checkAccessJWT(t, first, result.GetTokens().GetAccessToken(), user.GetUserId())
	if _, err = stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	replay := openPhoneWatch(t, first, phoneContext(ctx), "existing-phone")
	again, err := replay.Recv()
	if err != nil || !proto.Equal(result, again) {
		t.Fatal("existing-user result changed", err)
	}
	if changed, err := first.App.Verifications.Confirm(ctx, verification.Proof{Provider: "test", CallID: "call:existing-phone", Phone: "+79001234567"}); changed || err != nil {
		t.Fatal(changed, err)
	}
	var sessions int
	if err = first.App.DB.Primary().QueryRow(ctx, `SELECT count(*) FROM auth_session WHERE user_id=$1`, user.GetUserId()).Scan(&sessions); err != nil || sessions != 2 {
		t.Fatal("duplicate confirmation issued a session", sessions, err)
	}
	rotated, err := userv1.NewAuthServiceClient(first.Conn).RefreshTokens(authContext(ctx, authKey()), &userv1.RefreshTokensRequest{RefreshToken: result.GetTokens().GetRefreshToken()})
	if err != nil || rotated.GetTokens().GetRefreshToken() == result.GetTokens().GetRefreshToken() {
		t.Fatal(rotated, err)
	}
	watchReason(t, second, phoneContext(ctx), "existing-phone", "INVALID_TOKEN")
}
func TestPhoneWatchExpiryAndNoLateConfirmation(t *testing.T) {
	env := apitest.Start(t)
	phoneJob(t, env)
	pendingPhoneAPI(t, env, "expired-before", "+79001234567", time.Now().Add(-time.Second))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	watchReason(t, env, phoneContext(ctx), "expired-before", "INVALID_TOKEN")
	pendingPhoneAPI(t, env, "expires-open", "+79001234568", time.Now().Add(300*time.Millisecond))
	stream := openPhoneWatch(t, env, phoneContext(ctx), "expires-open")
	if m, err := stream.Recv(); err != nil || m.GetStatus() != userv1.VerificationStatus_VERIFICATION_STATUS_PENDING {
		t.Fatal(m, err)
	}
	if m, err := stream.Recv(); err != nil || m.GetStatus() != userv1.VerificationStatus_VERIFICATION_STATUS_EXPIRED || m.Tokens != nil || m.RegistrationToken != nil {
		t.Fatal(m, err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if changed, err := env.App.Verifications.Confirm(ctx, verification.Proof{Provider: "test", CallID: "call:expires-open", Phone: "+79001234568"}); changed || err != nil {
		t.Fatal(changed, err)
	}
	watchReason(t, env, phoneContext(ctx), "expires-open", "INVALID_TOKEN")
	releasedID := pendingPhoneAPI(t, env, "released-pending", "+79001234570", time.Now().Add(time.Minute))
	if _, err := env.App.DB.Primary().Exec(ctx, `UPDATE phone_verification SET released_at=$2 WHERE id=$1`, releasedID, time.Now()); err != nil {
		t.Fatal(err)
	}
	watchReason(t, env, phoneContext(ctx), "released-pending", "INVALID_TOKEN")
	if changed, err := env.App.Verifications.Confirm(ctx, verification.Proof{Provider: "test", CallID: "call:released-pending", Phone: "+79001234570"}); changed || err != nil {
		t.Fatal("released attempt revived", changed, err)
	}

	pendingPhoneAPI(t, env, "old-result", "+79001234569", time.Now().Add(time.Minute))
	confirmPhoneAPI(t, env, "old-result", "+79001234569")
	if _, err := env.App.DB.Primary().Exec(ctx, `UPDATE phone_verification SET verified_at=$1 WHERE provider_call_id='call:old-result'`, time.Now().Add(-verification.ResultTTL)); err != nil {
		t.Fatal(err)
	}
	watchReason(t, env, phoneContext(ctx), "old-result", "INVALID_TOKEN")
	if _, err := env.App.Verifications.Cleanup(ctx, 1000); err != nil {
		t.Fatal(err)
	}
	var wiped bool
	if err := env.App.DB.Primary().QueryRow(ctx, `SELECT result_ciphertext IS NULL AND result_key_id IS NULL FROM phone_verification WHERE provider_call_id='call:old-result'`).Scan(&wiped); err != nil || !wiped {
		t.Fatal("expired secret not wiped", err)
	}
}
func TestPhoneWatchAccessLimitsAndCapacity(t *testing.T) {
	t.Run("access and attempt order", func(t *testing.T) {
		env := apitest.Start(t, func(c *config.Config) { c.Limits[ratelimit.WatchIP] = ratelimit.Rule{Limit: 1, Window: time.Hour} })
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		for _, caller := range []context.Context{apitest.Caller(ctx, "assistant"), ctx} {
			stream := openPhoneWatch(t, env, caller, "foreign-secret")
			m, err := stream.Recv()
			if m != nil || (status.Code(err) != codes.PermissionDenied && status.Code(err) != codes.Unauthenticated) || apitest.Reason(err) != "" {
				t.Fatal(m, err)
			}
		}
		watchReason(t, env, phoneContext(ctx), "unknown-secret", "INVALID_TOKEN")
		watchReason(t, env, phoneContext(ctx), "still-unknown", "RATE_LIMITED")
		var hits int
		if err := env.App.DB.Primary().QueryRow(ctx, `SELECT sum(hits)::integer FROM rate_limit_window WHERE scope='watch_ip:3600'`).Scan(&hits); err != nil || hits != 1 {
			t.Fatal("denial consumed another attempt", hits, err)
		}
	})
	t.Run("capacity and cancellation", func(t *testing.T) {
		env := apitest.Start(t, func(c *config.Config) { c.Phone.MaxStreams = 1 })
		phoneJob(t, env)
		pendingPhoneAPI(t, env, "one", "+79001234567", time.Now().Add(time.Minute))
		pendingPhoneAPI(t, env, "two", "+79001234568", time.Now().Add(time.Minute))
		ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
		defer cancel()
		oneCtx, cancelOne := context.WithCancel(phoneContext(ctx))
		one := openPhoneWatch(t, env, oneCtx, "one")
		if _, err := one.Recv(); err != nil {
			t.Fatal(err)
		}
		two := openPhoneWatch(t, env, phoneContext(ctx), "two")
		if m, err := two.Recv(); m != nil || status.Code(err) != codes.Internal || apitest.Reason(err) != "" {
			t.Fatal(m, err)
		}
		if phoneMetric(t, env, "finassist_phone_watch_active_streams", nil) != 1 || phoneMetric(t, env, "finassist_phone_watch_max_streams", nil) != 1 || phoneMetric(t, env, "finassist_phone_watch_rejected_total", map[string]string{"reason": "capacity"}) != 1 {
			t.Fatal("capacity not observable")
		}
		cancelOne()
		if _, err := one.Recv(); status.Code(err) != codes.Canceled {
			t.Fatal(err)
		}
		eventually(t, func() bool { return phoneMetric(t, env, "finassist_phone_watch_active_streams", nil) == 0 })
		resumed := openPhoneWatch(t, env, phoneContext(ctx), "two")
		if m, err := resumed.Recv(); err != nil || m.GetStatus() != userv1.VerificationStatus_VERIFICATION_STATUS_PENDING {
			t.Fatal(m, err)
		}
	})
}
func TestPhoneConfirmationConcurrentAccountDeletion(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	user, _ := registerAPI(t, env, "delete-owner", "+79001234567")
	pendingPhoneAPI(t, env, "delete-pending", "+79001234567", time.Now().Add(time.Minute))
	hold, err := env.App.DB.Primary().Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(context.Background()) }()
	if _, err = hold.Exec(t.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,110))`, user.GetUserId()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	done := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Go(func() {
		_, err := env.App.Verifications.Confirm(ctx, verification.Proof{Provider: "test", CallID: "call:delete-pending", Phone: "+79001234567"})
		done <- err
	})
	wg.Go(func() {
		_, err := userv1.NewProfileServiceClient(env.Conn).DeleteAccount(apitest.User(ctx, user.GetUserId()), &userv1.DeleteAccountRequest{})
		done <- err
	})
	eventually(t, func() bool {
		var n int
		err := env.App.DB.Primary().QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND application_name='user' AND wait_event_type='Lock' AND query LIKE '%pg_advisory_xact_lock%'`).Scan(&n)
		return err == nil && n == 2
	})
	if err = hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(done)
	for err := range done {
		if err != nil {
			t.Fatal(err)
		}
	}
	var users, sessions, requests, tombstones int
	err = env.App.DB.Primary().QueryRow(ctx, `SELECT (SELECT count(*) FROM app_user),(SELECT count(*) FROM auth_session),(SELECT count(*) FROM phone_verification),(SELECT count(*) FROM tombstone_users)`).Scan(&users, &sessions, &requests, &tombstones)
	if err != nil || users+sessions+requests != 0 || tombstones != 1 {
		t.Fatal("confirmation resurrected deleted data", users, sessions, requests, tombstones, err)
	}
	watchReason(t, env, phoneContext(ctx), "delete-pending", "INVALID_TOKEN")
}

func TestPhoneWatchListenerFailureReconnectAndSafeReplay(t *testing.T) {
	env := apitest.Start(t)
	_, done := phoneJob(t, env)
	pendingPhoneAPI(t, env, "reconnect", "+79001234567", time.Now().Add(time.Minute))
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	stream := openPhoneWatch(t, env, phoneContext(ctx), "reconnect")
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	var pid int32
	if err := env.App.DB.Primary().QueryRow(ctx, `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND application_name='user-phone-listener'`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := env.App.DB.Primary().Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatal(err)
	}
	if m, err := stream.Recv(); m != nil || status.Code(err) != codes.Internal || apitest.Reason(err) != "" {
		t.Fatal("listener failure became terminal verification", m, err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("listener failure hidden")
		}
	case <-ctx.Done():
		t.Fatal("listener did not exit")
	}
	if phoneMetric(t, env, "finassist_phone_watch_active_streams", nil) != 0 || phoneMetric(t, env, "finassist_phone_watch_listener_ready", nil) != 0 {
		t.Fatal("disconnect metrics stale")
	}
	// A failed listener cannot keep new pending streams silently waiting.
	unavailable := openPhoneWatch(t, env, phoneContext(ctx), "reconnect")
	if m, err := unavailable.Recv(); m != nil || status.Code(err) != codes.Internal {
		t.Fatal(m, err)
	}
	if phoneMetric(t, env, "finassist_phone_watch_rejected_total", map[string]string{"reason": "not_ready"}) != 1 {
		t.Fatal("listener unavailability invisible")
	}
	phoneJob(t, env)
	eventually(t, func() bool { return phoneMetric(t, env, "finassist_phone_watch_listener_ready", nil) == 1 })
	resumed := openPhoneWatch(t, env, phoneContext(ctx), "reconnect")
	if m, err := resumed.Recv(); err != nil || m.GetStatus() != userv1.VerificationStatus_VERIFICATION_STATUS_PENDING {
		t.Fatal(m, err)
	}
	confirmPhoneAPI(t, env, "reconnect", "+79001234567")
	if m, err := resumed.Recv(); err != nil || m.GetStatus() != userv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED {
		t.Fatal("same secret did not resume", m, err)
	}
}
func TestPhoneConfirmationRollbackAndCorruptResultFailClosed(t *testing.T) {
	t.Run("failed session insert rolls confirmation back", func(t *testing.T) {
		env := apitest.Start(t)
		seedDocuments(t, env)
		_, _ = registerAPI(t, env, "existing-rollback", "+79001234567")
		id := pendingPhoneAPI(t, env, "session-failure", "+79001234567", time.Now().Add(time.Minute))
		if _, err := env.App.DB.Primary().Exec(t.Context(), `DROP TABLE refresh_token`); err != nil {
			t.Fatal(err)
		}
		changed, err := env.App.Verifications.Confirm(t.Context(), verification.Proof{Provider: "test", CallID: "call:session-failure", Phone: "+79001234567"})
		if changed || err == nil {
			t.Fatal(changed, err)
		}
		var clean bool
		var sessions int
		err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT verified_at IS NULL AND result_ciphertext IS NULL AND user_id IS NULL,(SELECT count(*) FROM auth_session) FROM phone_verification WHERE id=$1`, id).Scan(&clean, &sessions)
		if err != nil || !clean || sessions != 1 {
			t.Fatal("partial confirmation committed", clean, sessions, err)
		}
	})
	for _, kind := range []string{"ciphertext", "key-id"} {
		t.Run(kind, func(t *testing.T) {
			env := apitest.Start(t)
			id := pendingPhoneAPI(t, env, "corrupted", "+79001234567", time.Now().Add(time.Minute))
			confirmPhoneAPI(t, env, "corrupted", "+79001234567")
			var err error
			if kind == "ciphertext" {
				_, err = env.App.DB.Primary().Exec(t.Context(), `UPDATE phone_verification SET result_ciphertext=$2 WHERE id=$1`, id, make([]byte, 64))
			} else {
				_, err = env.App.DB.Primary().Exec(t.Context(), `UPDATE phone_verification SET result_key_id='unknown-key' WHERE id=$1`, id)
			}
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			stream := openPhoneWatch(t, env, phoneContext(ctx), "corrupted")
			if m, err := stream.Recv(); m != nil || status.Code(err) != codes.Internal || apitest.Reason(err) != "" {
				t.Fatal("bad result leaked", m, err)
			}
		})
	}
}
func TestPhoneWatchResyncSeesDeletionWithoutNotification(t *testing.T) {
	env := apitest.Start(t, func(c *config.Config) { c.Phone.ResyncInterval = 20 * time.Millisecond })
	seedDocuments(t, env)
	user, _ := registerAPI(t, env, "resync-owner", "+79001234567")
	phoneJob(t, env)
	pendingPhoneAPI(t, env, "resync-pending", "+79001234567", time.Now().Add(time.Minute))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	stream := openPhoneWatch(t, env, phoneContext(ctx), "resync-pending")
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	if _, err := userv1.NewProfileServiceClient(env.Conn).DeleteAccount(apitest.User(ctx, user.GetUserId()), &userv1.DeleteAccountRequest{}); err != nil {
		t.Fatal(err)
	}
	if m, err := stream.Recv(); m != nil || apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatal("deleted request kept streaming", m, err)
	}
	eventually(t, func() bool { return phoneMetric(t, env, "finassist_phone_watch_active_streams", nil) == 0 })
}

func TestPhoneCleanupWorkerBatchesAndStops(t *testing.T) {
	env := apitest.Start(t)
	_, err := env.App.DB.Primary().Exec(t.Context(), `INSERT INTO phone_verification(phone,token_hash,provider,created_at,expires_at) SELECT '+79'||lpad(g::text,9,'0'),sha256(int4send(g)),'test',$1,$2 FROM generate_series(1,2100) g`, time.Now().Add(-25*time.Hour), time.Now().Add(-25*time.Hour+5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	pendingPhoneAPI(t, env, "keep-active", "+78001234567", time.Now().Add(time.Minute))
	cancel, done := phoneBackgroundJob(t, env, "phone-cleanup")
	eventually(t, func() bool {
		var n int
		err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM phone_verification`).Scan(&n)
		return err == nil && n == 1
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
}
