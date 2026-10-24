package apitest_test

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/user/internal/adapter/fakephone"
	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/ratelimit"
)

func fakePhoneEnv(t *testing.T) *apitest.Env {
	t.Helper()
	return apitest.Start(t, func(c *config.Config) { c.Zvonok.Provider = "fake" })
}

func TestFakePhoneAPINewExistingReplayDeletionAndNotify(t *testing.T) {
	env := fakePhoneEnv(t)
	seedDocuments(t, env)
	if env.App.PhoneCallback != nil {
		t.Fatal("fake opened a real ingress")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// Independent LISTEN proves the common transactional pg_notify is executed,
	// even though the first Watch can already see the committed verified result.
	listener, err := pgx.Connect(ctx, env.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close(context.WithoutCancel(ctx)) }()
	if _, err = listener.Exec(ctx, "LISTEN phone_verification"); err != nil {
		t.Fatal(err)
	}
	phone, key := "+70000001234", authKey()
	started, err := startCall(t, env, key, phone)
	if err != nil || started.GetExpiresIn() < 298 || started.GetExpiresIn() > 300 {
		t.Fatal(started, err)
	}
	var id int32
	var pool []string
	var selected string
	var verified bool
	if err = env.App.DB.Primary().QueryRow(ctx, `SELECT id,allowed_phones_for_call,callback_number,verified_at IS NOT NULL FROM phone_verification WHERE phone=$1`, phone).Scan(&id, &pool, &selected, &verified); err != nil ||
		!verified || !slices.Equal(pool, []string{fakephone.CallbackNumberOne, fakephone.CallbackNumberTwo}) ||
		!slices.Contains(pool, selected) || selected != started.GetCallbackNumber() {
		t.Fatal("common selector/confirmation missing", pool, selected, verified, err)
	}
	notice, err := listener.WaitForNotification(ctx)
	if err != nil || notice.Payload != strconv.FormatInt(int64(id), 10) {
		t.Fatal(notice, err)
	}
	first, err := openPhoneWatch(t, env, phoneContext(ctx), started.GetVerificationToken()).Recv()
	if err != nil || first.GetStatus() != userv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED || !first.GetIsNewUser() || first.GetRegistrationToken() == "" {
		t.Fatal(first, err)
	}
	again, err := startCall(t, env, key, phone)
	if err != nil || again.GetVerificationToken() != started.GetVerificationToken() || again.GetCallbackNumber() != selected {
		t.Fatal(again, err)
	}
	replayed, err := openPhoneWatch(t, env, phoneContext(ctx), again.GetVerificationToken()).Recv()
	if err != nil || replayed.GetRegistrationToken() != first.GetRegistrationToken() || startHits(t, env) != 1 {
		t.Fatal(replayed, err)
	}
	client := userv1.NewAuthServiceClient(env.Conn)
	registered, err := client.Register(authContext(ctx, authKey()), registrationRequest(first.GetRegistrationToken()))
	if err != nil {
		t.Fatal(err)
	}
	checkAccessJWT(t, env, registered.GetTokens().GetAccessToken(), registered.GetUserId())
	// Let the old claim expire without sleeping; confirmation results keep their TTL.
	if _, err = env.App.DB.Primary().Exec(ctx, `UPDATE phone_verification SET released_at=now(),expires_at=now()-interval '1 second' WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	second, err := startCall(t, env, authKey(), phone)
	if err != nil {
		t.Fatal(err)
	}
	logged, err := openPhoneWatch(t, env, phoneContext(ctx), second.GetVerificationToken()).Recv()
	if err != nil || logged.GetIsNewUser() || logged.GetTokens().GetRefreshToken() == "" {
		t.Fatal(logged, err)
	}
	checkAccessJWT(t, env, logged.GetTokens().GetAccessToken(), registered.GetUserId())
	if _, err = userv1.NewProfileServiceClient(env.Conn).DeleteAccount(apitest.User(ctx, registered.GetUserId()), &userv1.DeleteAccountRequest{}); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err = env.App.DB.Primary().QueryRow(ctx, `SELECT count(*) FROM phone_verification WHERE phone=$1`, phone).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatal(remaining, err)
	}
	watchReason(t, env, phoneContext(ctx), second.GetVerificationToken(), "INVALID_TOKEN")
}

func TestFakePhoneAPIConfirmationRollbackRecoversOwnAttempt(t *testing.T) {
	env := fakePhoneEnv(t)
	// Force common Confirm to fail after creation and pool selection, without mocks.
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE phone_verification RENAME COLUMN registration_token_hash TO temporarily_unavailable`); err != nil {
		t.Fatal(err)
	}
	key, phone := authKey(), "+70000004321"
	if out, err := startCall(t, env, key, phone); err == nil || out != nil {
		t.Fatal(out, err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE phone_verification RENAME COLUMN temporarily_unavailable TO registration_token_hash`); err != nil {
		t.Fatal(err)
	}
	var id int32
	var created time.Time
	var callID *string
	var selected *string
	var verified *time.Time
	var pool []string
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT id,created_at,provider_call_id,callback_number,allowed_phones_for_call,verified_at FROM phone_verification WHERE phone=$1`, phone).Scan(&id, &created, &callID, &selected, &pool, &verified); err != nil ||
		callID != nil || selected != nil || len(pool) != 0 || verified != nil {
		t.Fatal(id, callID, selected, pool, verified, err)
	}
	if startHits(t, env) != 1 {
		t.Fatal("reservation lost")
	}
	// Reassembly creates a fresh provider and idempotency engine on the same DB.
	env = secondPhoneInstance(t, env)
	out, err := startCall(t, env, key, phone)
	if err != nil || !out.GetExpiresAt().AsTime().Equal(created.Add(5*time.Minute)) {
		t.Fatal(out, err)
	}
	var recovered string
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT provider_call_id FROM phone_verification WHERE id=$1 AND verified_at IS NOT NULL`, id).Scan(&recovered); err != nil || recovered != "fake:"+strconv.FormatInt(int64(id), 10) || startHits(t, env) != 1 {
		t.Fatal(recovered, err)
	}
}

func TestFakePhoneAPINamespaceRefusalAndSharedLimits(t *testing.T) {
	env := apitest.Start(t, func(c *config.Config) {
		c.Zvonok.Provider = "fake"
		c.Limits[ratelimit.WatchIP] = ratelimit.Rule{Limit: 1, Window: time.Hour}
	})
	if out, err := startCall(t, env, authKey(), "+79990001234"); err == nil || out != nil || startHits(t, env) != 0 {
		t.Fatal(out, err)
	}
	var claimed int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM phone_verification WHERE released_at IS NULL`).Scan(&claimed); err != nil || claimed != 0 {
		t.Fatal(claimed, err)
	}
	phone := "+70000005678"
	for range 3 {
		out, err := startCall(t, env, authKey(), phone)
		if err != nil {
			t.Fatal(out, err)
		}
		if _, err = env.App.DB.Primary().Exec(t.Context(), `UPDATE phone_verification SET released_at=now(),expires_at=now()-interval '1 second' WHERE phone=$1`, phone); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := startCall(t, env, authKey(), phone); apitest.Reason(err) != "RATE_LIMITED" || startHits(t, env) != 3 {
		t.Fatal(err)
	}
	// Watch limits run before secret authentication, as with the real provider.
	var watchCalls int
	for range 2 {
		stream := openPhoneWatch(t, env, phoneContext(t.Context()), "invalid-test-secret")
		_, err := stream.Recv()
		if apitest.Reason(err) == "RATE_LIMITED" {
			watchCalls++
			break
		}
	}
	if watchCalls == 0 {
		t.Fatal("fake bypasses Watch IP limits")
	}
}
