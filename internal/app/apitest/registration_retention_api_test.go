package apitest_test

import (
	"strconv"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/user/internal/app/apitest"
)

func TestAPIRegistrationRetentionPurgesLiveFailedRegisterKeyWithoutAuthCleanup(t *testing.T) {
	provider := &evidenceProvider{}
	env := evidenceEnv(t, provider)
	seedDocuments(t, env)
	client := userv1.NewAuthServiceClient(env.Conn)
	request, startKey := consentStartRequest("+79001234567"), authKey()
	started, err := client.StartPhoneVerification(authContext(t.Context(), startKey), request)
	if err != nil {
		t.Fatal(err)
	}
	confirmCreated(t, env, &provider.creationProvider, request.GetPhone())
	watch := openPhoneWatch(t, env, phoneContext(t.Context()), started.GetVerificationToken())
	verified, err := watch.Recv()
	if err != nil || !verified.GetIsNewUser() || verified.GetRegistrationToken() == "" {
		t.Fatal("new-user registration proof missing", err)
	}
	var attempt int32
	var verifiedAt time.Time
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT id,verified_at FROM phone_verification WHERE phone=$1`, request.GetPhone()).Scan(&attempt, &verifiedAt); err != nil {
		t.Fatal(err)
	}

	// This key is first admitted after confirmation, so its 24h cache can still
	// be live when the earlier abandoned attempt reaches its evidence cutoff.
	registerKey := authKey()
	registration := registrationRequest(verified.GetRegistrationToken())
	registration.Timezone = stringPtr("Local")
	for range 2 {
		_, err := client.Register(authContext(t.Context(), registerKey), registration)
		if status.Code(err) != codes.InvalidArgument || apitest.Reason(err) != "VALIDATION_ERROR" {
			t.Fatal("valid proof did not retain its timezone refusal", err)
		}
		assertRequestPermission(t, err, false)
		found := false
		for _, detail := range status.Convert(err).Details() {
			if bad, ok := detail.(*errdetails.BadRequest); ok {
				for _, violation := range bad.GetFieldViolations() {
					if violation.GetField() == "timezone" && violation.GetReason() == "INVALID_FORMAT" {
						found = true
					}
				}
			}
		}
		if !found {
			t.Fatal("missing timezone validation detail")
		}
	}
	var resourceType, resourceID string
	var done bool
	var registerExpires time.Time
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT resource_type,resource_id,done,expires_at FROM idempotency_keys WHERE endpoint=$1 AND key=$2`, userv1.AuthService_Register_FullMethodName, registerKey).Scan(&resourceType, &resourceID, &done, &registerExpires); err != nil || resourceType != "phone_verification" || resourceID != strconv.FormatInt(int64(attempt), 10) || !done || !registerExpires.Add(-24*time.Hour).After(verifiedAt) {
		t.Fatalf("cached refusal lost its authenticated attempt: resource=%s/%s done=%t err=%v", resourceType, resourceID, done, err)
	}
	var registerHits int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT COALESCE(sum(hits),0) FROM rate_limit_window WHERE scope='register_ip:60'`).Scan(&registerHits); err != nil || registerHits != 1 {
		t.Fatal("cached timezone refusal was executed twice", registerHits, err)
	}

	// Keep another live attempt and its key as a control. Cleanup must remove
	// resources by the expired attempt ID, without a global cache purge.
	otherStartKey := authKey()
	otherRequest := consentStartRequest("+79001234568")
	if _, err := client.StartPhoneVerification(authContext(t.Context(), otherStartKey), otherRequest); err != nil {
		t.Fatal(err)
	}
	var otherAttempt int32
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT id FROM phone_verification WHERE phone=$1`, otherRequest.GetPhone()).Scan(&otherAttempt); err != nil {
		t.Fatal(err)
	}

	// Age only the abandoned proof/request and its original Start cache. Leave
	// the later Register cache's real future expiration untouched. No background
	// jobs (especially auth-cleanup) run in this API fixture.
	now := time.Now().UTC()
	oldExpiry := now.Add(-25 * time.Hour)
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE phone_verification SET created_at=$2,expires_at=$3,verified_at=$4,known_busy_until=$3 WHERE id=$1`, attempt, oldExpiry.Add(-5*time.Minute), oldExpiry, oldExpiry.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE phone_consent_receipt SET consent_accepted_at=$2,age_confirmed_at=$2,attempt_end=$3,retain_until=$4,key_expires_at=$5 WHERE attempt_id=$1`, attempt, oldExpiry.Add(-5*time.Minute), oldExpiry, oldExpiry.Add(24*time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE idempotency_keys SET expires_at=$3 WHERE endpoint=$1 AND key=$2`, userv1.AuthService_StartPhoneVerification_FullMethodName, startKey, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if !registerExpires.After(time.Now()) {
		t.Fatal("Register key expired before evidence cleanup; live-key regression not exercised")
	}
	if _, err := env.App.Verifications.Cleanup(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	var keys, requests, receipts, bindings, users, sessions int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM idempotency_keys WHERE key=ANY($1::uuid[]) OR (resource_type='phone_verification' AND resource_id=$2)),(SELECT count(*) FROM phone_verification WHERE id=$3),(SELECT count(*) FROM phone_consent_receipt WHERE attempt_id=$3),(SELECT count(*) FROM phone_provider_binding WHERE attempt_id=$3),(SELECT count(*) FROM app_user),(SELECT count(*) FROM auth_session)`, []string{startKey, registerKey}, resourceID, attempt).Scan(&keys, &requests, &receipts, &bindings, &users, &sessions); err != nil || keys != 0 || requests != 0 || receipts != 0 || bindings != 0 || users != 0 || sessions != 0 {
		t.Fatal("phone cleanup retained abandoned proof derivatives", keys, requests, receipts, bindings, users, sessions, err)
	}
	var otherKeys, otherRequests, otherReceipts int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM idempotency_keys WHERE endpoint=$1 AND key=$2 AND resource_type='phone_verification' AND resource_id=$3),(SELECT count(*) FROM phone_verification WHERE id=$4),(SELECT count(*) FROM phone_consent_receipt WHERE attempt_id=$4)`, userv1.AuthService_StartPhoneVerification_FullMethodName, otherStartKey, strconv.FormatInt(int64(otherAttempt), 10), otherAttempt).Scan(&otherKeys, &otherRequests, &otherReceipts); err != nil || otherKeys != 1 || otherRequests != 1 || otherReceipts != 1 {
		t.Fatal("retention cleanup touched another live attempt", otherKeys, otherRequests, otherReceipts, err)
	}
	if _, err := client.Register(authContext(t.Context(), registerKey), registration); apitest.Reason(err) != "INVALID_TOKEN" {
		t.Fatal("erased proof still returned its old cached refusal", err)
	}
	if creates, recovers := provider.counts(); creates != 2 || recovers != 0 {
		t.Fatal("retention cleanup contacted provider", creates, recovers)
	}
}
