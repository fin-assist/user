package apitest_test

import (
	"strings"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/ratelimit"
)

func consentStartRequest(phone string) *userv1.StartPhoneVerificationRequest {
	return &userv1.StartPhoneVerificationRequest{
		Phone: phone, PdConsentAccepted: true, PdConsentVersion: "v1", AgeConfirmed: true,
	}
}

// Call the actual RPC rather than a UI or handler helper: scalar defaults in an
// older client must not bypass the server's pre-provider gates.
func TestAPIRegistrationConsentDirectStartRefusalsBeforeProvider(t *testing.T) {
	for _, tc := range []struct {
		name, reason, field string
		mutate              func(*userv1.StartPhoneVerificationRequest)
	}{
		{"old-client-scalars", "VALIDATION_ERROR", "", func(r *userv1.StartPhoneVerificationRequest) {
			r.PdConsentAccepted, r.AgeConfirmed, r.PdConsentVersion = false, false, ""
		}},
		{"age", "VALIDATION_ERROR", "age_confirmed", func(r *userv1.StartPhoneVerificationRequest) { r.AgeConfirmed = false }},
		{"age-before-consent-and-version", "VALIDATION_ERROR", "age_confirmed", func(r *userv1.StartPhoneVerificationRequest) {
			r.AgeConfirmed, r.PdConsentAccepted, r.PdConsentVersion = false, false, "old"
		}},
		{"empty-version", "VALIDATION_ERROR", "pd_consent_version", func(r *userv1.StartPhoneVerificationRequest) { r.PdConsentVersion = "" }},
		{"long-unicode-version", "VALIDATION_ERROR", "pd_consent_version", func(r *userv1.StartPhoneVerificationRequest) { r.PdConsentVersion = strings.Repeat("я", 65) }},
		{"consent", "CONSENT_REQUIRED", "", func(r *userv1.StartPhoneVerificationRequest) { r.PdConsentAccepted = false }},
		{"consent-before-version", "CONSENT_REQUIRED", "", func(r *userv1.StartPhoneVerificationRequest) {
			r.PdConsentAccepted, r.PdConsentVersion = false, "old"
		}},
		{"outdated", "CONSENT_OUTDATED", "", func(r *userv1.StartPhoneVerificationRequest) { r.PdConsentVersion = "old" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := &creationProvider{}
			env := startEnv(t, provider)
			seedDocuments(t, env)
			request := consentStartRequest("+79001234567")
			tc.mutate(request)
			_, err := userv1.NewAuthServiceClient(env.Conn).StartPhoneVerification(authContext(t.Context(), authKey()), request)
			if apitest.Reason(err) != tc.reason {
				t.Fatalf("reason = %q, want %q: %v", apitest.Reason(err), tc.reason, err)
			}
			assertRequestPermission(t, err, false)
			if tc.field != "" {
				found := false
				for _, detail := range status.Convert(err).Details() {
					if bad, ok := detail.(*errdetails.BadRequest); ok {
						for _, violation := range bad.GetFieldViolations() {
							if violation.GetField() == tc.field {
								found = true
								if tc.field == "age_confirmed" && violation.GetReason() != "REQUIRED" {
									t.Fatalf("age violation = %v", violation)
								}
							}
						}
					}
				}
				if !found {
					t.Fatalf("missing %s violation: %v", tc.field, err)
				}
			}
			if creates, recovers := provider.counts(); creates != 0 || recovers != 0 {
				t.Fatalf("refusal contacted provider: create=%d recover=%d", creates, recovers)
			}
			var attempts int
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM phone_verification`).Scan(&attempts); err != nil || attempts != 0 {
				t.Fatalf("refusal saved a working attempt: count=%d err=%v", attempts, err)
			}
		})
	}
}

func TestAPIRegistrationConsentKeyAndBusyPrecedeAssertions(t *testing.T) {
	provider := &creationProvider{}
	env := startEnv(t, provider)
	seedDocuments(t, env)
	client := userv1.NewAuthServiceClient(env.Conn)
	key := authKey()
	request := consentStartRequest("+79001234567")
	first, err := client.StartPhoneVerification(authContext(t.Context(), key), request)
	if err != nil {
		t.Fatal(err)
	}
	changed := proto.Clone(request).(*userv1.StartPhoneVerificationRequest)
	changed.AgeConfirmed, changed.PdConsentAccepted, changed.PdConsentVersion = false, false, "old"
	if _, err := client.StartPhoneVerification(authContext(t.Context(), key), changed); apitest.Reason(err) != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("changed assertions bypassed key comparison: %v", err)
	}
	if _, err := client.StartPhoneVerification(authContext(t.Context(), authKey()), changed); apitest.Reason(err) != "RATE_LIMITED" {
		t.Fatalf("consent gate ran before busy-phone refusal: %v", err)
	}
	replayed, err := client.StartPhoneVerification(authContext(t.Context(), key), request)
	if err != nil || replayed.GetVerificationToken() != first.GetVerificationToken() {
		t.Fatalf("original request no longer replays: %v %v", replayed, err)
	}
	if creates, _ := provider.counts(); creates != 1 {
		t.Fatalf("conflict/busy/replay created %d provider requests", creates)
	}
}

func TestAPIRegistrationConsentLimitPrecedesAssertionsAndBusy(t *testing.T) {
	provider := &creationProvider{}
	env := startEnv(t, provider, func(c *config.Config) {
		rule := c.Limits[ratelimit.VerificationIPShort]
		rule.Limit = 1
		c.Limits[ratelimit.VerificationIPShort] = rule
	})
	seedDocuments(t, env)
	client := userv1.NewAuthServiceClient(env.Conn)
	request := consentStartRequest("+79001234567")
	if _, err := client.StartPhoneVerification(authContext(t.Context(), authKey()), request); err != nil {
		t.Fatal(err)
	}
	request.AgeConfirmed, request.PdConsentAccepted, request.PdConsentVersion = false, false, "old"
	_, err := client.StartPhoneVerification(authContext(t.Context(), authKey()), request)
	wantRetry := env.LimitNow.Truncate(time.Minute).Add(time.Minute)
	if apitest.Reason(err) != "RATE_LIMITED" || !startRetryAt(t, err).Equal(wantRetry) {
		t.Fatalf("limiter did not precede busy/consent: %v", err)
	}
	if creates, _ := provider.counts(); creates != 1 {
		t.Fatalf("limit called provider: creates=%d", creates)
	}
}

func rotatePersonalDataDocument(t *testing.T, env *apitest.Env, version string) {
	t.Helper()
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE consent_document SET is_current=false WHERE type='personal_data'`); err != nil {
		t.Fatal(err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `INSERT INTO consent_document(type,version,title,url,is_current) VALUES('personal_data',$1,'Updated PD consent','https://example.test/pd-updated',true)`, version); err != nil {
		t.Fatal(err)
	}
}

func TestAPIRegistrationConsentVersionIsPinnedThroughRegister(t *testing.T) {
	provider := &creationProvider{}
	env := startEnv(t, provider)
	seedDocuments(t, env)
	phoneJob(t, env)
	client := userv1.NewAuthServiceClient(env.Conn)
	request, key := consentStartRequest("+79001234567"), authKey()
	started, err := client.StartPhoneVerification(authContext(t.Context(), key), request)
	if err != nil {
		t.Fatal(err)
	}
	rotatePersonalDataDocument(t, env, "v3")
	replayed, err := client.StartPhoneVerification(authContext(t.Context(), key), request)
	if err != nil || replayed.GetVerificationToken() != started.GetVerificationToken() {
		t.Fatalf("document rotation invalidated accepted attempt: %v %v", replayed, err)
	}
	newRequest := consentStartRequest("+79001234568")
	if _, err := client.StartPhoneVerification(authContext(t.Context(), authKey()), newRequest); apitest.Reason(err) != "CONSENT_OUTDATED" {
		t.Fatalf("new attempt accepted obsolete version: %v", err)
	}
	watch := openPhoneWatch(t, env, phoneContext(t.Context()), started.GetVerificationToken())
	if pending, err := watch.Recv(); err != nil || pending.GetStatus() != userv1.VerificationStatus_VERIFICATION_STATUS_PENDING {
		t.Fatal(pending, err)
	}
	confirmCreated(t, env, provider, request.GetPhone())
	verified, err := watch.Recv()
	if err != nil || verified.GetRegistrationToken() == "" {
		t.Fatal(verified, err)
	}
	wrong := registrationRequest(verified.GetRegistrationToken())
	wrong.PdConsentVersion = "v3"
	if _, err := client.Register(authContext(t.Context(), authKey()), wrong); apitest.Reason(err) != "CONSENT_OUTDATED" {
		t.Fatalf("current version substituted for original receipt: %v", err)
	}
	registered, err := client.Register(authContext(t.Context(), authKey()), registrationRequest(verified.GetRegistrationToken()))
	if err != nil || registered.GetUserId() == "" {
		t.Fatalf("original receipt rejected after rotation: %v %v", registered, err)
	}
	if creates, _ := provider.counts(); creates != 1 {
		t.Fatalf("rotation/replay repeated external call: %d", creates)
	}
}

func TestAPIRegistrationConsentVersionLengthCountsUnicodeCharacters(t *testing.T) {
	provider := &creationProvider{}
	env := startEnv(t, provider)
	seedDocuments(t, env)
	version := strings.Repeat("я", 64)
	rotatePersonalDataDocument(t, env, version)
	request := consentStartRequest("+79001234567")
	request.PdConsentVersion = version
	response, err := userv1.NewAuthServiceClient(env.Conn).StartPhoneVerification(authContext(t.Context(), authKey()), request)
	if err != nil || response.GetVerificationToken() == "" {
		t.Fatalf("valid 64-character version rejected as 128 bytes: %v %v", response, err)
	}
}

func TestAPIRegistrationConsentDocumentStorageFailurePreventsProvider(t *testing.T) {
	provider := &creationProvider{}
	env := startEnv(t, provider)
	seedDocuments(t, env)
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE consent_document RENAME TO unavailable_consent_document`); err != nil {
		t.Fatal(err)
	}
	_, err := userv1.NewAuthServiceClient(env.Conn).StartPhoneVerification(authContext(t.Context(), authKey()), consentStartRequest("+79001234567"))
	if status.Code(err) != codes.Internal {
		t.Fatalf("storage failure was not internal: %v", err)
	}
	if creates, recovers := provider.counts(); creates != 0 || recovers != 0 {
		t.Fatalf("storage failure contacted provider: create=%d recover=%d", creates, recovers)
	}
}
