package apitest_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	idempotency "github.com/fin-assist/go-idempotency"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/verification"
)

type evidenceProvider struct {
	creationProvider
	before  func(context.Context, verification.Attempt) error
	outcome func(verification.Attempt, verification.Call, error) (verification.Call, error)
}

func (p *evidenceProvider) Create(ctx context.Context, a verification.Attempt) (verification.Call, error) {
	if p.before != nil {
		if err := p.before(ctx, a); err != nil {
			return verification.Call{}, err
		}
	}
	call, err := p.creationProvider.Create(ctx, a)
	if p.outcome != nil {
		return p.outcome(a, call, err)
	}
	return call, err
}

func evidenceEnv(t *testing.T, provider *evidenceProvider, mutate ...func(*config.Config)) *apitest.Env {
	t.Helper()
	env := apitest.StartWithPhone(t, provider, creationSelector{}, mutate...)
	provider.env = env
	return env
}

type observedReceipt struct {
	id                           int32
	phone, ip, version           string
	accepted, age, end, retained time.Time
	linked                       *string
	hash                         []byte
	err                          error
}

func TestAPIRegistrationEvidenceCommittedBeforeProviderAndLinkedAfterProof(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-account", true: "existing-account"}[existing], func(t *testing.T) {
			provider := &evidenceProvider{}
			env := evidenceEnv(t, provider)
			if existing {
				seedProfiles(t, env)
			} else {
				seedDocuments(t, env)
			}
			phoneJob(t, env)
			request, key := consentStartRequest("+79001234567"), authKey()
			wantHash, err := idempotency.HashProto(request)
			if err != nil {
				t.Fatal(err)
			}
			observed := make(chan observedReceipt, 1)
			provider.before = func(ctx context.Context, a verification.Attempt) error {
				r := observedReceipt{id: a.ID}
				r.err = env.App.DB.Primary().QueryRow(ctx, `SELECT phone,client_ip,consent_version,consent_accepted_at,age_confirmed_at,attempt_end,retain_until,linked_user_id::text,request_hash FROM phone_consent_receipt WHERE attempt_id=$1`, a.ID).Scan(&r.phone, &r.ip, &r.version, &r.accepted, &r.age, &r.end, &r.retained, &r.linked, &r.hash)
				observed <- r
				return r.err
			}
			client := userv1.NewAuthServiceClient(env.Conn)
			started, err := client.StartPhoneVerification(authContext(t.Context(), key), request)
			if err != nil {
				t.Fatal(err)
			}
			var receipt observedReceipt
			select {
			case receipt = <-observed:
			case <-time.After(time.Second):
				t.Fatal("provider did not observe a committed receipt")
			}
			if receipt.err != nil || receipt.phone != request.GetPhone() || receipt.ip != "203.0.113.7" || receipt.version != "v1" || receipt.accepted.IsZero() || !receipt.age.Equal(receipt.accepted) || receipt.linked != nil || !bytes.Equal(receipt.hash, wantHash[:]) || !receipt.retained.Equal(receipt.end.Add(24*time.Hour)) {
				t.Fatal("incorrect pre-provider receipt", receipt)
			}
			var owner *string
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT linked_user_id::text FROM phone_consent_receipt WHERE attempt_id=$1`, receipt.id).Scan(&owner); err != nil || owner != nil {
				t.Fatal("phone-only lookup assigned unverified assertions to account", owner, err)
			}
			watch := openPhoneWatch(t, env, phoneContext(t.Context()), started.GetVerificationToken())
			if _, err := watch.Recv(); err != nil {
				t.Fatal(err)
			}
			confirmCreated(t, env, &provider.creationProvider, request.GetPhone())
			verified, err := watch.Recv()
			if err != nil {
				t.Fatal(err)
			}
			user := profileUserA
			if existing {
				if verified.GetIsNewUser() || verified.GetTokens().GetRefreshToken() == "" {
					t.Fatal(verified)
				}
			} else {
				if !verified.GetIsNewUser() || verified.GetRegistrationToken() == "" {
					t.Fatal(verified)
				}
				if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT linked_user_id::text FROM phone_consent_receipt WHERE attempt_id=$1`, receipt.id).Scan(&owner); err != nil || owner != nil {
					t.Fatal("new account linked before Register", owner, err)
				}
				// A subsequent IP is not an additional consent event or its timestamp.
				registerCtx := metadata.AppendToOutgoingContext(apitest.Caller(t.Context(), "bank-bff"), "x-client-ip", "203.0.113.8", "idempotency-key", authKey())
				registered, err := client.Register(registerCtx, registrationRequest(verified.GetRegistrationToken()))
				if err != nil {
					t.Fatal(err)
				}
				user = registered.GetUserId()
			}
			var accepted, age time.Time
			var ip string
			var retained *time.Time
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT linked_user_id::text,consent_accepted_at,age_confirmed_at,client_ip,retain_until FROM phone_consent_receipt WHERE attempt_id=$1`, receipt.id).Scan(&owner, &accepted, &age, &ip, &retained); err != nil || owner == nil || *owner != user || !accepted.Equal(receipt.accepted) || !age.Equal(receipt.age) || ip != receipt.ip || retained != nil {
				t.Fatal("proof association rewrote or prematurely expired evidence", owner, accepted, age, ip, retained, err)
			}
			if !existing {
				var granted time.Time
				if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT granted_at FROM user_consent WHERE user_id=$1 AND type='personal_data'`, user).Scan(&granted); err != nil || !granted.Equal(receipt.accepted) {
					t.Fatal("Register replaced original consent time", granted, err)
				}
			}
		})
	}
}

func TestAPIRegistrationEvidenceUnknownDeadlineReleasesPhoneWithoutReconfirm(t *testing.T) {
	provider := &evidenceProvider{creationProvider: creationProvider{mode: "unknown"}}
	env := evidenceEnv(t, provider, func(c *config.Config) { c.Phone.UnknownTimeout = time.Second })
	seedDocuments(t, env)
	client := userv1.NewAuthServiceClient(env.Conn)
	request, oldKey := consentStartRequest("+79001234567"), authKey()
	if _, err := client.StartPhoneVerification(authContext(t.Context(), oldKey), request); status.Code(err) != codes.Internal {
		t.Fatal(err)
	}
	var id int32
	var originalEnd, originalRetained time.Time
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT attempt_id,attempt_end,retain_until FROM phone_consent_receipt WHERE start_key=$1`, oldKey).Scan(&id, &originalEnd, &originalRetained); err != nil {
		t.Fatal(err)
	}
	if !originalRetained.Equal(originalEnd.Add(24 * time.Hour)) {
		t.Fatal("wrong evidence retention", originalEnd, originalRetained)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE phone_verification SET unknown_deadline=$2,expires_at=$2 WHERE id=$1`, id, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err := client.StartPhoneVerification(authContext(t.Context(), oldKey), request)
	if status.Code(err) != codes.Internal {
		t.Fatal("expired original key was restarted", err)
	}
	assertRequestPermission(t, err, false)
	changed := consentStartRequest("+79001234568")
	if _, err := client.StartPhoneVerification(authContext(t.Context(), oldKey), changed); apitest.Reason(err) != "IDEMPOTENCY_CONFLICT" {
		t.Fatal("terminal key lost its original hash", err)
	}
	if creates, recovers := provider.counts(); creates != 1 || recovers != 0 {
		t.Fatal("expired original effect repeated", creates, recovers)
	}
	provider.setMode("")
	newKey := authKey()
	created, err := client.StartPhoneVerification(authContext(t.Context(), newKey), request)
	if err != nil || created.GetVerificationToken() == "" {
		t.Fatal("new independent intent did not acquire released phone", created, err)
	}
	if creates, recovers := provider.counts(); creates != 2 || recovers != 0 {
		t.Fatal("unexpected external calls", creates, recovers)
	}
	var count int
	var end, retained time.Time
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM phone_consent_receipt WHERE phone=$1`, request.GetPhone()).Scan(&count); err != nil || count != 2 {
		t.Fatal("new attempt reused or deleted old receipt", count, err)
	}
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT attempt_end,retain_until FROM phone_consent_receipt WHERE attempt_id=$1`, id).Scan(&end, &retained); err != nil || !end.Equal(originalEnd) || !retained.Equal(originalRetained) {
		t.Fatal("release/replay extended evidence retention", end, retained, err)
	}
	if startHits(t, env) != 2 {
		t.Fatal("terminal replays changed accounting", startHits(t, env))
	}
}

func TestAPIRegistrationEvidenceDispatchedDuplicateIsTerminalAfterCached429(t *testing.T) {
	provider := &evidenceProvider{}
	until := time.Now().UTC().Add(2 * time.Minute)
	provider.outcome = func(_ verification.Attempt, _ verification.Call, _ error) (verification.Call, error) {
		return verification.Call{}, &verification.ProviderDuplicateError{BusyUntil: until}
	}
	env := evidenceEnv(t, provider)
	seedDocuments(t, env)
	client := userv1.NewAuthServiceClient(env.Conn)
	request, key := consentStartRequest("+79001234567"), authKey()
	for range 2 {
		_, err := client.StartPhoneVerification(authContext(t.Context(), key), request)
		if apitest.Reason(err) != "RATE_LIMITED" || !startRetryAt(t, err).After(until.Add(-time.Nanosecond)) {
			t.Fatal("missing dispatched duplicate refusal", err)
		}
		assertRequestPermission(t, err, false)
	}
	if _, err := client.StartPhoneVerification(authContext(t.Context(), authKey()), request); apitest.Reason(err) != "RATE_LIMITED" {
		t.Fatal("known busy fact ignored", err)
	}
	if creates, recovers := provider.counts(); creates != 1 || recovers != 0 {
		t.Fatal("busy/replay contacted provider", creates, recovers)
	}
	// Advance just the persisted refusal deadline without sleeping. The original
	// dispatched key must remain terminal even though a duplicate cache can expire.
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE idempotency_keys SET expires_at=$2 WHERE key=$1`, key, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE phone_known_busy SET busy_until=$2 WHERE phone=$1`, request.GetPhone(), time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err := client.StartPhoneVerification(authContext(t.Context(), key), request)
	if status.Code(err) != codes.Internal {
		t.Fatal("dispatched original key reused after429", err)
	}
	assertRequestPermission(t, err, false)
	if creates, recovers := provider.counts(); creates != 1 || recovers != 0 {
		t.Fatal("expired429 repeated original effect", creates, recovers)
	}
}

func TestAPIRegistrationEvidenceLiveBindingCollisionDoesNotBecomeUnknown(t *testing.T) {
	provider := &evidenceProvider{}
	provider.outcome = func(a verification.Attempt, c verification.Call, err error) (verification.Call, error) {
		c.ID = "stable-provider-id"
		return c, err
	}
	env := evidenceEnv(t, provider)
	seedDocuments(t, env)
	client := userv1.NewAuthServiceClient(env.Conn)
	request, firstKey := consentStartRequest("+79001234567"), authKey()
	if _, err := client.StartPhoneVerification(authContext(t.Context(), firstKey), request); err != nil {
		t.Fatal(err)
	}
	// The old live row and archive binding retain the ID after its phone lease
	// elapses. A provider reusing it cannot transfer the old result to this intent.
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE phone_verification SET expires_at=$2,known_busy_until=$2,released_at=$2 WHERE start_key=$1`, firstKey, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err := client.StartPhoneVerification(authContext(t.Context(), authKey()), request)
	if apitest.Reason(err) != "RATE_LIMITED" {
		t.Fatal("live unique collision escaped typed duplicate path", err)
	}
	assertRequestPermission(t, err, false)
	var bound int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM phone_provider_binding WHERE call_id='stable-provider-id'`).Scan(&bound); err != nil || bound != 1 {
		t.Fatal("ID was rebound", bound, err)
	}
}

func TestAPIRegistrationEvidenceStorageAndCommitRefusalPreventProvider(t *testing.T) {
	for _, clause := range []string{"NOT DEFERRABLE", "DEFERRABLE INITIALLY DEFERRED"} {
		t.Run(clause, func(t *testing.T) {
			provider := &evidenceProvider{}
			env := evidenceEnv(t, provider)
			seedDocuments(t, env)
			// Test-only FK target leaves the visible document intact while rejecting
			// its evidence at INSERT or COMMIT. Production DDL is unchanged.
			if _, err := env.App.DB.Primary().Exec(t.Context(), `CREATE TABLE evidence_test_document(type text,version text,PRIMARY KEY(type,version)); ALTER TABLE phone_consent_receipt DROP CONSTRAINT phone_consent_receipt_document_fkey`); err != nil {
				t.Fatal(err)
			}
			if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE phone_consent_receipt ADD CONSTRAINT evidence_test_document_fkey FOREIGN KEY(consent_type,consent_version) REFERENCES evidence_test_document(type,version) `+clause); err != nil {
				t.Fatal(err)
			}
			client := userv1.NewAuthServiceClient(env.Conn)
			key, request := authKey(), consentStartRequest("+79001234567")
			if out, err := client.StartPhoneVerification(authContext(t.Context(), key), request); err == nil || out != nil {
				t.Fatal("non-durable evidence accepted", out, err)
			}
			if creates, recovers := provider.counts(); creates != 0 || recovers != 0 {
				t.Fatal("receipt failure disclosed phone to provider", creates, recovers)
			}
			for _, table := range []string{"phone_verification", "phone_consent_receipt", "idempotency_keys", "rate_limit_window"} {
				var count int
				if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM `+table).Scan(&count); err != nil || count != 0 {
					t.Fatal("pre-provider transaction partly committed", table, count, err)
				}
			}
			if _, err := env.App.DB.Primary().Exec(t.Context(), `INSERT INTO evidence_test_document VALUES('personal_data','v1')`); err != nil {
				t.Fatal(err)
			}
			if out, err := client.StartPhoneVerification(authContext(t.Context(), key), request); err != nil || out.GetVerificationToken() == "" {
				t.Fatal("same key could not retry rolled-back transaction", out, err)
			}
			if creates, recovers := provider.counts(); creates != 1 || recovers != 0 {
				t.Fatal(creates, recovers)
			}
		})
	}
}
