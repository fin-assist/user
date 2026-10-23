package apitest_test

import (
	"context"
	"sync"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/token"
)

func TestDeleteAccountPurgesPhoneSecretsAndRepeats(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	original, regKey := registerAPI(t, env, "delete-reg", "+79001234567")
	other, _ := registerAPI(t, env, "other-reg", "+79001234568")
	auth := userv1.NewAuthServiceClient(env.Conn)
	profiles := userv1.NewProfileServiceClient(env.Conn)
	refreshKey := authKey()
	rotation, err := auth.RefreshTokens(authContext(t.Context(), refreshKey), &userv1.RefreshTokensRequest{RefreshToken: original.GetTokens().GetRefreshToken()})
	if err != nil {
		t.Fatal(err)
	}
	// Same phone, no user_id: neither the pending request nor unused new-user
	// registration result is covered by the app_user foreign-key cascade.
	seedRegistration(t, env, "unused-result", "+79001234567", time.Now().UTC())
	verification := token.Sum("pending-deletion")
	if _, err = env.App.DB.Primary().Exec(t.Context(), `INSERT INTO phone_verification(phone,token_hash,provider,created_at,expires_at) VALUES('+79001234567',$1,'test',$2,$3)`, verification[:], time.Now(), time.Now().Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err = profiles.DeleteAccount(apitest.Caller(t.Context(), "assistant"), &userv1.DeleteAccountRequest{}); status.Code(err) != codes.PermissionDenied || apitest.Reason(err) != "" {
		t.Fatal(err)
	}
	if _, err = profiles.DeleteAccount(apitest.Caller(t.Context(), "bank-bff"), &userv1.DeleteAccountRequest{}); apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatal(err)
	}
	ctx := apitest.User(t.Context(), original.GetUserId())
	if _, err = profiles.DeleteAccount(ctx, &userv1.DeleteAccountRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err = profiles.DeleteAccount(ctx, &userv1.DeleteAccountRequest{}); err != nil {
		t.Fatal("repeat blocked by tombstone", err)
	}
	var users, requests, sessions, links, consents, keys, tombstones, events, settings, windows int
	err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT
 (SELECT count(*) FROM app_user WHERE id=$1),
 (SELECT count(*) FROM phone_verification WHERE phone='+79001234567'),
 (SELECT count(*) FROM auth_session WHERE user_id=$1),
 (SELECT count(*) FROM refresh_token r JOIN auth_session s ON s.id=r.session_id WHERE s.user_id=$1),
 (SELECT count(*) FROM user_consent WHERE user_id=$1),
 (SELECT count(*) FROM idempotency_keys WHERE scope=$1::uuid OR owner_id=$1::uuid),
 (SELECT count(*) FROM tombstone_users WHERE user_id=$1),
 (SELECT count(*) FROM outbox_events WHERE topic='user.deleted' AND event_key=$1::text),
 (SELECT count(*) FROM outbox_events WHERE topic='user.timezone_changed' AND event_key=$1::text),
 (SELECT count(*) FROM rate_limit_window)`, original.GetUserId()).Scan(&users, &requests, &sessions, &links, &consents, &keys, &tombstones, &events, &settings, &windows)
	if err != nil || users+requests+sessions+links+consents+keys+settings != 0 || tombstones != 1 || events != 1 || windows == 0 {
		t.Fatal(users, requests, sessions, links, consents, keys, tombstones, events, settings, windows, err)
	}
	if _, err = profiles.GetProfile(ctx, &userv1.GetProfileRequest{}); apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatal(err)
	}
	// Removed cached replies execute no old successful response; consumed secrets
	// remain invalid after deletion, for both registration and refresh.
	if out, err := auth.Register(authContext(t.Context(), regKey), registrationRequest("delete-reg")); err == nil || out != nil {
		t.Fatal("cached registration survived", err)
	}
	if _, err = auth.RefreshTokens(authContext(t.Context(), refreshKey), &userv1.RefreshTokensRequest{RefreshToken: original.GetTokens().GetRefreshToken()}); apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatal(err)
	}
	if _, err = auth.RefreshTokens(authContext(t.Context(), authKey()), &userv1.RefreshTokensRequest{RefreshToken: rotation.GetTokens().GetRefreshToken()}); apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatal(err)
	}
	if _, err = auth.Register(authContext(t.Context(), authKey()), registrationRequest("unused-result")); apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatal(err)
	}
	if _, err = profiles.GetProfile(apitest.User(t.Context(), other.GetUserId()), &userv1.GetProfileRequest{}); err != nil {
		t.Fatal("other user affected", err)
	}
	var count int
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM app_user WHERE id=$1`, other.GetUserId()).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	replacement, _ := registerAPI(t, env, "replacement-reg", "+79001234567")
	if replacement.GetUserId() == original.GetUserId() {
		t.Fatal("deleted identity reused")
	}
	if _, err = profiles.DeleteAccount(ctx, &userv1.DeleteAccountRequest{}); err != nil {
		t.Fatal(err)
	}
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM app_user WHERE id=$1`, replacement.GetUserId()).Scan(&count); err != nil || count != 1 {
		t.Fatal("old delete removed replacement", count, err)
	}
}

func TestDeleteAccountRollbackOnStorageFailure(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	r, _ := registerAPI(t, env, "rollback-reg", "+79001234567")
	if _, err := env.App.DB.Primary().Exec(t.Context(), `DROP TABLE outbox_events`); err != nil {
		t.Fatal(err)
	}
	_, err := userv1.NewProfileServiceClient(env.Conn).DeleteAccount(apitest.User(t.Context(), r.GetUserId()), &userv1.DeleteAccountRequest{})
	if status.Code(err) != codes.Internal || apitest.Reason(err) != "" {
		t.Fatal(err)
	}
	var users, links, consents, keys, requests, deleted int
	err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM app_user),(SELECT count(*) FROM refresh_token),(SELECT count(*) FROM user_consent),(SELECT count(*) FROM idempotency_keys),(SELECT count(*) FROM phone_verification),(SELECT count(*) FROM tombstone_users)`).Scan(&users, &links, &consents, &keys, &requests, &deleted)
	if err != nil || users != 1 || links != 1 || consents != 1 || keys != 1 || requests != 1 || deleted != 0 {
		t.Fatal("partial deletion", users, links, consents, keys, requests, deleted, err)
	}
}

func TestDeleteAccountConcurrentReplayAndProfileWrite(t *testing.T) {
	env := apitest.Start(t)
	seedDocuments(t, env)
	r, key := registerAPI(t, env, "race-reg", "+79001234567")
	hold, err := env.App.DB.Primary().Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(context.Background()) }()
	if _, err = hold.Exec(t.Context(), `SELECT 1 FROM idempotency_keys WHERE key=$1 FOR UPDATE`, key); err != nil {
		t.Fatal(err)
	}
	c := userv1.NewProfileServiceClient(env.Conn)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	deleteDone := make(chan error, 1)
	go func() {
		_, err := c.DeleteAccount(apitest.User(ctx, r.GetUserId()), &userv1.DeleteAccountRequest{})
		deleteDone <- err
	}()
	// A cached replay queues behind the held key. Deletion must release its
	// tombstone guard rather than hold it waiting for this replay's row lock.
	replayDone := make(chan error, 1)
	go func() {
		_, err := userv1.NewAuthServiceClient(env.Conn).Register(authContext(ctx, key), registrationRequest("race-reg"))
		replayDone <- err
	}()
	eventually(t, func() bool {
		var waiting bool
		err := env.App.DB.Primary().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND application_name='user-auth' AND wait_event_type='Lock')`).Scan(&waiting)
		return err == nil && waiting
	})
	theme := userv1.Theme_THEME_DARK
	if _, err = c.UpdateProfile(apitest.User(ctx, r.GetUserId()), &userv1.UpdateProfileRequest{Theme: &theme}); err != nil {
		t.Fatal("deletion held guard while waiting for key", err)
	}
	if err = hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-deleteDone; err != nil {
		t.Fatal("deletion failed under replay contention", err)
	}
	err = <-replayDone
	if err != nil && apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatal(err)
	}
	if _, err = c.GetProfile(apitest.User(ctx, r.GetUserId()), &userv1.GetProfileRequest{}); apitest.Reason(err) != "TOKEN_REVOKED" {
		t.Fatal(err)
	}
	// Concurrent repeats have no extra durable event.
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if _, err := c.DeleteAccount(apitest.User(ctx, r.GetUserId()), &userv1.DeleteAccountRequest{}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var events int
	if err = env.App.DB.Primary().QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE topic='user.deleted'`).Scan(&events); err != nil || events != 1 {
		t.Fatal(events, err)
	}
}

func TestDeleteAccountInvalidTokenAndUnknownUser(t *testing.T) {
	env := apitest.Start(t)
	c := userv1.NewProfileServiceClient(env.Conn)
	ctx := metadata.AppendToOutgoingContext(apitest.Caller(t.Context(), "bank-bff"), "authorization", "Bearer invalid")
	if _, err := c.DeleteAccount(ctx, &userv1.DeleteAccountRequest{}); err == nil {
		t.Fatal("invalid token accepted")
	}
	// A verified ID absent locally still schedules remote cleanup, exactly once.
	for range 2 {
		if _, err := c.DeleteAccount(apitest.User(t.Context(), profileUserA), &userv1.DeleteAccountRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM outbox_events`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestDeleteAccountSerializesWithNewAuthEffects(t *testing.T) {
	for _, operation := range []string{"register", "refresh"} {
		t.Run(operation, func(t *testing.T) {
			env := apitest.Start(t)
			seedDocuments(t, env)
			r, _ := registerAPI(t, env, "initial", "+79001234567")
			seedRegistration(t, env, "parallel", "+79001234567", time.Now().UTC())
			hold, err := env.App.DB.Primary().Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = hold.Rollback(context.Background()) }()
			if _, err = hold.Exec(t.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,110))`, r.GetUserId()); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			authDone, deleteDone := make(chan error, 1), make(chan error, 1)
			go func() {
				client := userv1.NewAuthServiceClient(env.Conn)
				var err error
				if operation == "register" {
					_, err = client.Register(authContext(ctx, authKey()), registrationRequest("parallel"))
				} else {
					_, err = client.RefreshTokens(authContext(ctx, authKey()), &userv1.RefreshTokensRequest{RefreshToken: r.GetTokens().GetRefreshToken()})
				}
				authDone <- err
			}()
			go func() {
				_, err := userv1.NewProfileServiceClient(env.Conn).DeleteAccount(apitest.User(ctx, r.GetUserId()), &userv1.DeleteAccountRequest{})
				deleteDone <- err
			}()
			// Both operations reached the real primary guard before either may commit.
			eventually(t, func() bool {
				var n int
				err := env.App.DB.Primary().QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND application_name IN ('user','user-auth') AND wait_event_type='Lock' AND query LIKE '%pg_advisory_xact_lock%'`).Scan(&n)
				return err == nil && n == 2
			})
			if err = hold.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err = <-deleteDone; err != nil {
				t.Fatal("delete failed", err)
			}
			if err = <-authDone; err != nil && apitest.Reason(err) != "INVALID_TOKEN" && apitest.Reason(err) != "TOKEN_REVOKED" {
				t.Fatal("unexpected auth result", err)
			}
			var users, sessions, refresh, requests, events int
			err = env.App.DB.Primary().QueryRow(ctx, `SELECT (SELECT count(*) FROM app_user),(SELECT count(*) FROM auth_session),(SELECT count(*) FROM refresh_token),(SELECT count(*) FROM phone_verification),(SELECT count(*) FROM outbox_events WHERE topic='user.deleted')`).Scan(&users, &sessions, &refresh, &requests, &events)
			if err != nil || users+sessions+refresh+requests != 0 || events != 1 {
				t.Fatal("auth resurrected deleted data", users, sessions, refresh, requests, events, err)
			}
		})
	}
}
