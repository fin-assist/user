package apitest_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/ratelimit"
	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/verification"
)

type creationProvider struct {
	mu                sync.Mutex
	mode              string
	creates, recovers int
	calls             map[string]verification.Call
	started, proceed  chan struct{}
	env               *apitest.Env
}

func (p *creationProvider) Name() string { return "test" }
func (p *creationProvider) Create(ctx context.Context, a verification.Attempt) (verification.Call, error) {
	p.mu.Lock()
	p.creates++
	mode := p.mode
	count := p.creates
	started, proceed := p.started, p.proceed
	p.started = nil
	if p.calls == nil {
		p.calls = map[string]verification.Call{}
	}
	c := verification.Call{ID: fmt.Sprintf("call-%d", count), Phone: a.Phone, StartedAt: a.FirstDispatchAt, ReceivedAt: time.Now().UTC(), State: verification.Pending, Campaign: a.Campaign, Numbers: []string{"+74951234567", "+74957654321"}}
	p.calls[a.Phone] = c
	p.mu.Unlock()
	if p.env != nil {
		var open int
		if err := p.env.App.DB.Primary().QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND state='idle in transaction'`).Scan(&open); err != nil || open != 0 {
			return verification.Call{}, errors.New("provider called with open transaction")
		}
	}
	if started != nil {
		close(started)
	}
	if proceed != nil {
		select {
		case <-proceed:
		case <-ctx.Done():
			return verification.Call{}, ctx.Err()
		}
	}
	switch mode {
	case "reject":
		return verification.Call{}, verification.ErrProviderRejected
	case "duplicate":
		return verification.Call{}, verification.ErrProviderDuplicate
	case "lost", "unknown":
		return verification.Call{}, verification.ErrProviderUnavailable
	case "empty":
		c.Numbers = nil
	}
	return c, nil
}
func (p *creationProvider) Recover(_ context.Context, a verification.Attempt) (verification.Call, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recovers++
	if p.mode == "unknown" {
		return verification.Call{}, verification.ErrProviderUnavailable
	}
	if c, ok := p.calls[a.Phone]; ok {
		return c, nil
	}
	// A new process has only the durable attempt; the adapter establishes provider
	// ownership before returning that same attempt with its captured lifetime/pool.
	c := verification.Call{ID: "recovered:" + fmt.Sprint(a.ID), Phone: a.Phone, StartedAt: a.FirstDispatchAt, ReceivedAt: time.Now().UTC(), Campaign: a.Campaign, State: verification.Pending, Numbers: []string{"+74951112233"}}
	if p.calls == nil {
		p.calls = map[string]verification.Call{}
	}
	p.calls[a.Phone] = c
	return c, nil
}
func (p *creationProvider) Status(_ context.Context, a verification.Attempt) (verification.ProviderStatus, error) {
	return verification.ProviderStatus{ID: a.CallID, Phone: a.Phone, State: verification.Verified}, nil
}
func (p *creationProvider) setMode(mode string) { p.mu.Lock(); defer p.mu.Unlock(); p.mode = mode }
func (p *creationProvider) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.creates, p.recovers
}
func (p *creationProvider) call(phone string) verification.Call {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[phone]
}

type creationSelector struct{ fail bool }

func (s creationSelector) Select(_ context.Context, _ int32, n []string) (string, error) {
	if s.fail {
		return "", errors.New("selection failed")
	}
	return n[len(n)-1], nil
}
func startEnv(t *testing.T, p *creationProvider, mutate ...func(*config.Config)) *apitest.Env {
	t.Helper()
	env := apitest.StartWithPhone(t, p, creationSelector{}, mutate...)
	p.env = env
	seedDocuments(t, env)
	return env
}
func startCall(t *testing.T, env *apitest.Env, key, phone string) (*userv1.StartPhoneVerificationResponse, error) {
	t.Helper()
	return userv1.NewAuthServiceClient(env.Conn).StartPhoneVerification(authContext(t.Context(), key), &userv1.StartPhoneVerificationRequest{Phone: phone, PdConsentAccepted: true, PdConsentVersion: "v1", AgeConfirmed: true})
}
func startHits(t *testing.T, env *apitest.Env) int {
	t.Helper()
	var n int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT COALESCE(sum(hits),0) FROM rate_limit_window WHERE scope='verification_phone_short:3600'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func confirmCreated(t *testing.T, env *apitest.Env, p *creationProvider, phone string) {
	t.Helper()
	c := p.call(phone)
	proof, err := p.Status(t.Context(), verification.Attempt{CallID: c.ID, Phone: phone})
	if err != nil || proof.State != verification.Verified {
		t.Fatal(proof, err)
	}
	changed, err := env.App.Verifications.Confirm(t.Context(), verification.Proof{Provider: p.Name(), CallID: proof.ID, Phone: proof.Phone})
	if err != nil || !changed {
		t.Fatal(changed, err)
	}
}

func TestAPIStartAPIFullNewAndExistingSignInAndDeletion(t *testing.T) {
	p := &creationProvider{}
	env := startEnv(t, p)
	seedDocuments(t, env)
	phoneJob(t, env)
	c := userv1.NewAuthServiceClient(env.Conn)
	key := authKey()
	phone := "+79001234567"
	out, err := startCall(t, env, key, phone)
	if err != nil || out.GetCallbackNumber() != "+74957654321" || out.GetExpiresIn() < 298 || out.GetExpiresIn() > 300 {
		t.Fatal(out, err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(out.GetVerificationToken())
	if err != nil || len(raw) != 32 {
		t.Fatal("verification entropy", err)
	}
	var storedHash []byte
	h := token.Sum(out.GetVerificationToken())
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT token_hash FROM phone_verification WHERE phone=$1`, phone).Scan(&storedHash); err != nil || !bytes.Equal(storedHash, h[:]) {
		t.Fatal("verification token not hashed", err)
	}
	again, err := startCall(t, env, key, phone)
	if err != nil || again.GetVerificationToken() != out.GetVerificationToken() || again.GetCallbackNumber() != out.GetCallbackNumber() || again.GetExpiresIn() > out.GetExpiresIn() {
		t.Fatal(again, err)
	}
	var payload []byte
	var owner *string
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT payload,owner_id::text FROM idempotency_keys WHERE key=$1`, key).Scan(&payload, &owner); err != nil || owner != nil || bytes.Contains(payload, []byte(out.GetVerificationToken())) {
		t.Fatal("replay not encrypted", err)
	}
	stream := openPhoneWatch(t, env, phoneContext(t.Context()), out.GetVerificationToken())
	if first, err := stream.Recv(); err != nil || first.GetStatus() != userv1.VerificationStatus_VERIFICATION_STATUS_PENDING {
		t.Fatal(first, err)
	}
	confirmCreated(t, env, p, phone)
	verified, err := stream.Recv()
	if err != nil || !verified.GetIsNewUser() || verified.GetRegistrationToken() == "" {
		t.Fatal(verified, err)
	}
	reg, err := c.Register(authContext(t.Context(), authKey()), registrationRequest(verified.GetRegistrationToken()))
	if err != nil {
		t.Fatal(err)
	}
	checkAccessJWT(t, env, reg.GetTokens().GetAccessToken(), reg.GetUserId())
	rotated, err := c.RefreshTokens(authContext(t.Context(), authKey()), &userv1.RefreshTokensRequest{RefreshToken: reg.GetTokens().GetRefreshToken()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Logout(apitest.User(t.Context(), reg.GetUserId()), &userv1.LogoutRequest{RefreshToken: rotated.GetTokens().GetRefreshToken()}); err != nil {
		t.Fatal(err)
	}
	// Let the previous provider lifetime elapse without a wall-clock sleep.
	now := time.Now().UTC()
	if _, err = env.App.DB.Primary().Exec(t.Context(), `UPDATE phone_verification SET created_at=$1,expires_at=$2,released_at=$2 WHERE phone=$3`, now.Add(-6*time.Minute), now.Add(-time.Minute), phone); err != nil {
		t.Fatal(err)
	}
	secondKey := authKey()
	second, err := startCall(t, env, secondKey, phone)
	if err != nil {
		t.Fatal(err)
	}
	secondStream := openPhoneWatch(t, env, phoneContext(t.Context()), second.GetVerificationToken())
	if _, err = secondStream.Recv(); err != nil {
		t.Fatal(err)
	}
	confirmCreated(t, env, p, phone)
	logged, err := secondStream.Recv()
	if err != nil || logged.GetIsNewUser() || logged.GetTokens().GetRefreshToken() == "" {
		t.Fatal(logged, err)
	}
	checkAccessJWT(t, env, logged.GetTokens().GetAccessToken(), reg.GetUserId())
	busyKey := authKey()
	if _, err = startCall(t, env, busyKey, phone); apitest.Reason(err) != "RATE_LIMITED" {
		t.Fatal(err)
	}
	if _, err = userv1.NewProfileServiceClient(env.Conn).DeleteAccount(apitest.User(t.Context(), reg.GetUserId()), &userv1.DeleteAccountRequest{}); err != nil {
		t.Fatal(err)
	}
	var keys, requests, links int
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM idempotency_keys WHERE key IN($1,$2,$3)),(SELECT count(*) FROM phone_verification WHERE phone=$4),(SELECT count(*) FROM phone_verification WHERE start_key IS NOT NULL)`, key, secondKey, busyKey, phone).Scan(&keys, &requests, &links); err != nil || keys != 0 || requests != 0 || links != 0 {
		t.Fatal("phone keys survived deletion", keys, requests, links, err)
	}
}

func TestAPIStartAPILostReplyRecoveryAndProcessRestart(t *testing.T) {
	p := &creationProvider{mode: "lost"}
	env := startEnv(t, p)
	key, phone := authKey(), "+79001234567"
	if out, e := startCall(t, env, key, phone); e == nil || out != nil {
		t.Fatal("lost response exposed success")
	}
	out, e := startCall(t, env, key, phone)
	if e != nil || out.GetVerificationToken() == "" {
		t.Fatal("read-only recovery failed", e)
	}
	if creates, recovers := p.counts(); creates != 1 || recovers != 1 {
		t.Fatal("unexpected effects", creates, recovers)
	}
	p.setMode("unknown")
	key = authKey()
	other := "+79007654321"
	if out, e = startCall(t, env, key, other); e == nil || out != nil {
		t.Fatal("unknown exposed success")
	}
	remote := p.call(other)
	before := startHits(t, env)
	_ = env.Conn.Close()
	shutdown, cancel := context.WithTimeout(t.Context(), time.Second)
	env.App.GRPC.Shutdown(shutdown, 0)
	cancel()
	env.App.Close()
	recovered := &creationProvider{calls: map[string]verification.Call{other: remote}}
	second := secondStartInstance(t, env, recovered)
	out, e = startCall(t, second, key, other)
	if e != nil || out.GetCallbackNumber() != remote.Numbers[len(remote.Numbers)-1] {
		t.Fatal("restart did not read original call", e)
	}
	if creates, recovers := recovered.counts(); creates != 0 || recovers != 1 || startHits(t, second) != before {
		t.Fatal("restart repeated external effect/accounting", creates, recovers)
	}
	if !out.GetExpiresAt().AsTime().Equal(remote.PublicExpiry()) {
		t.Fatal("recovery extended provider lifetime")
	}
	confirmCreated(t, second, recovered, other)
	stream := openPhoneWatch(t, second, phoneContext(t.Context()), out.GetVerificationToken())
	verified, e := stream.Recv()
	if e != nil || !verified.GetIsNewUser() {
		t.Fatal("recovered call cannot complete", e)
	}
}

func TestAPIStartAPIRejectionCompensationAndProviderDuplicate(t *testing.T) {
	p := &creationProvider{mode: "reject"}
	env := startEnv(t, p)
	key := authKey()
	phone := "+79001234567"
	if _, err := startCall(t, env, key, phone); status.Code(err) != codes.Internal {
		t.Fatal(err)
	}
	if hits := startHits(t, env); hits != 0 {
		t.Fatal("rejection consumed budget", hits)
	}
	if got := startMetric(t, env, "finassist_rate_limit_refunded_total", "verification_phone_short"); got != 1 {
		t.Fatal(got)
	}
	p.setMode("ok")
	key = authKey() // A conclusive rejection is terminal for its original key.
	if _, err := startCall(t, env, key, phone); err != nil {
		t.Fatal(err)
	}
	p.setMode("duplicate")
	dupKey := authKey()
	other := "+79007654321"
	if _, err := startCall(t, env, dupKey, other); apitest.Reason(err) != "RATE_LIMITED" {
		t.Fatal(err)
	}
	before, _ := p.counts()
	if _, err := startCall(t, env, dupKey, other); apitest.Reason(err) != "RATE_LIMITED" {
		t.Fatal(err)
	}
	after, _ := p.counts()
	if before != after {
		t.Fatal("duplicate replay called provider")
	}
	if got := phoneMetric(t, env, "finassist_phone_verification_provider_duplicate_total", nil); got != 1 {
		t.Fatal(got)
	}
}

func startMetric(t *testing.T, env *apitest.Env, name, scope string) float64 {
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
			for _, l := range m.Label {
				if l.GetName() == "scope" && l.GetValue() == scope {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	t.Fatal("metric not found", name, scope)
	return 0
}

func TestAPIStartAPIConcurrentKeysAndCommittedReservation(t *testing.T) {
	started, proceed := make(chan struct{}), make(chan struct{})
	p := &creationProvider{started: started, proceed: proceed}
	env := startEnv(t, p)
	key := authKey()
	phone := "+79001234567"
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	type reply struct {
		out *userv1.StartPhoneVerificationResponse
		err error
	}
	done := make(chan reply, 1)
	go func() {
		out, err := userv1.NewAuthServiceClient(env.Conn).StartPhoneVerification(authContext(ctx, key), &userv1.StartPhoneVerificationRequest{Phone: phone, PdConsentAccepted: true, PdConsentVersion: "v1", AgeConfirmed: true})
		done <- reply{out, err}
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := startCall(t, env, key, phone); apitest.Reason(err) != "REQUEST_IN_PROGRESS" {
		t.Fatal(err)
	}
	if _, err := startCall(t, env, key, "+79007654321"); apitest.Reason(err) != "IDEMPOTENCY_CONFLICT" {
		t.Fatal(err)
	}
	if _, err := startCall(t, env, authKey(), phone); apitest.Reason(err) != "RATE_LIMITED" {
		t.Fatal(err)
	}
	close(proceed)
	select {
	case got := <-done:
		if got.err != nil || got.out.GetVerificationToken() == "" {
			t.Fatal(got)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if hits := startHits(t, env); hits != 2 {
		t.Fatal("replay/in-progress consumed attempt", hits)
	}
}

func TestAPIStartAPIValidationDisabledAndLimitPriority(t *testing.T) {
	disabled := apitest.Start(t)
	if _, err := startCall(t, disabled, authKey(), "+79001234567"); status.Code(err) != codes.Internal || startHits(t, disabled) != 0 {
		t.Fatal(err)
	}
	p := &creationProvider{}
	env := startEnv(t, p, func(c *config.Config) {
		r := c.Limits[ratelimit.VerificationPhoneLong]
		r.Limit = 1
		c.Limits[ratelimit.VerificationPhoneLong] = r
	})
	key := authKey()
	if _, err := startCall(t, env, key, "wrong"); apitest.Reason(err) != "VALIDATION_ERROR" {
		t.Fatal(err)
	}
	if _, err := startCall(t, env, key, "+79001234567"); apitest.Reason(err) != "IDEMPOTENCY_CONFLICT" {
		t.Fatal(err)
	}
	if _, err := startCall(t, env, authKey(), "+79001234567"); err != nil {
		t.Fatal(err)
	}
	if _, err := startCall(t, env, authKey(), "+79001234567"); apitest.Reason(err) != "RATE_LIMITED" || !startRetryAt(t, err).Equal(env.LimitNow.Truncate(24*time.Hour).Add(24*time.Hour)) {
		t.Fatal("limit must precede busy phone", err)
	}
	if creates, _ := p.counts(); creates != 1 {
		t.Fatal("limit/busy called provider", creates)
	}
	c := userv1.NewAuthServiceClient(env.Conn)
	if _, err := c.StartPhoneVerification(phoneContext(t.Context()), &userv1.StartPhoneVerificationRequest{Phone: "+79001234567", PdConsentAccepted: true, PdConsentVersion: "v1", AgeConfirmed: true}); apitest.Reason(err) != "VALIDATION_ERROR" {
		t.Fatal(err)
	}
	if _, err := c.StartPhoneVerification(metadata.AppendToOutgoingContext(apitest.Caller(t.Context(), "assistant"), "idempotency-key", authKey()), &userv1.StartPhoneVerificationRequest{Phone: "+79001234567", PdConsentAccepted: true, PdConsentVersion: "v1", AgeConfirmed: true}); status.Code(err) != codes.PermissionDenied {
		t.Fatal(err)
	}
}

func startRetryAt(t *testing.T, err error) time.Time {
	t.Helper()
	for _, detail := range status.Convert(err).Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok {
			when, parseErr := time.Parse(time.RFC3339, info.GetMetadata()["retry_at"])
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			return when
		}
	}
	t.Fatal("missing ErrorInfo.retry_at", err)
	return time.Time{}
}

// Hold only the external call; the initial reservation must already be committed.
func blockedStart(t *testing.T, env *apitest.Env, p *creationProvider, ctx context.Context, key, phone string) (<-chan error, func()) {
	t.Helper()
	var once sync.Once
	release := func() { once.Do(func() { close(p.proceed) }) }
	t.Cleanup(release)
	started := p.started
	done := make(chan error, 1)
	callCtx := authContext(ctx, key)
	go func() {
		_, err := userv1.NewAuthServiceClient(env.Conn).StartPhoneVerification(callCtx, &userv1.StartPhoneVerificationRequest{Phone: phone, PdConsentAccepted: true, PdConsentVersion: "v1", AgeConfirmed: true})
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("provider was not reached")
	}
	return done, release
}

func awaitStart(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("creation did not finish")
		return nil
	}
}

func TestAPIStartAPIDeletionDuringProviderCannotResurrect(t *testing.T) {
	p := &creationProvider{started: make(chan struct{}), proceed: make(chan struct{})}
	env := startEnv(t, p)
	seedProfiles(t, env)
	key, phone := authKey(), "+79001234567"
	done, release := blockedStart(t, env, p, t.Context(), key, phone)
	if _, err := userv1.NewProfileServiceClient(env.Conn).DeleteAccount(apitest.User(t.Context(), profileUserA), &userv1.DeleteAccountRequest{}); err != nil {
		t.Fatal(err)
	}
	release()
	if err := awaitStart(t, done); err == nil {
		t.Fatal("deleted creation returned credentials")
	}
	c := p.call(phone)
	if changed, err := env.App.Verifications.Confirm(t.Context(), verification.Proof{Provider: p.Name(), CallID: c.ID, Phone: phone}); !errors.Is(err, verification.ErrMissing) || changed {
		t.Fatal("deleted request confirmed", changed, err)
	}
	var count int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM phone_verification WHERE phone=$1)+(SELECT count(*) FROM idempotency_keys WHERE key=$2)+(SELECT count(*) FROM phone_verification WHERE start_key IS NOT NULL)`, phone, key).Scan(&count); err != nil || count != 0 {
		t.Fatal("creation resurrected data", count, err)
	}
}

func TestAPIStartAPICallerCancellationPreservesCommittedCreation(t *testing.T) {
	p := &creationProvider{started: make(chan struct{}), proceed: make(chan struct{})}
	env := startEnv(t, p)
	key, phone := authKey(), "+79001234567"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done, release := blockedStart(t, env, p, ctx, key, phone)
	cancel()
	if err := awaitStart(t, done); status.Code(err) != codes.Canceled {
		t.Fatal(err)
	}
	release()
	deadline, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		out, err := userv1.NewAuthServiceClient(env.Conn).StartPhoneVerification(authContext(deadline, key), &userv1.StartPhoneVerificationRequest{Phone: phone, PdConsentAccepted: true, PdConsentVersion: "v1", AgeConfirmed: true})
		if err == nil {
			if out.GetVerificationToken() == "" || startHits(t, env) != 1 {
				t.Fatal("cancelled effect lost or charged twice", out)
			}
			break
		}
		if apitest.Reason(err) != "REQUEST_IN_PROGRESS" {
			t.Fatal(err)
		}
		select {
		case <-tick.C:
		case <-deadline.Done():
			t.Fatal(deadline.Err())
		}
	}
	if creates, recovers := p.counts(); creates != 1 || recovers != 0 {
		t.Fatal("cancelled creation repeated", creates, recovers)
	}
}

func TestAPIStartAPIRejectionKeepsBusyKeyForPhoneScopedDeletion(t *testing.T) {
	p := &creationProvider{mode: "reject", started: make(chan struct{}), proceed: make(chan struct{})}
	env := startEnv(t, p)
	seedProfiles(t, env)
	key, phone := authKey(), "+79001234567"
	done, release := blockedStart(t, env, p, t.Context(), key, phone)
	busyKey := authKey()
	_, busy := startCall(t, env, busyKey, phone)
	if apitest.Reason(busy) != "RATE_LIMITED" {
		t.Fatal(busy)
	}
	var expires time.Time
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT expires_at FROM phone_verification WHERE phone=$1`, phone).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	expected := expires.Truncate(time.Minute)
	if expected.Before(expires) {
		expected = expected.Add(time.Minute)
	}
	if !startRetryAt(t, busy).Equal(expected) {
		t.Fatal("busy deadline not rounded up", busy)
	}
	release()
	if err := awaitStart(t, done); status.Code(err) != codes.Internal || startHits(t, env) != 1 {
		t.Fatal("compensated another key's attempt", err)
	}
	_, replayed := startCall(t, env, busyKey, phone)
	if apitest.Reason(replayed) != "RATE_LIMITED" || !startRetryAt(t, replayed).Equal(expected) {
		t.Fatal("busy response changed after holder rejection", replayed)
	}
	p.setMode("ok")
	foreignKey, foreignPhone := authKey(), "+79007654321"
	foreign, err := startCall(t, env, foreignKey, foreignPhone)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = userv1.NewProfileServiceClient(env.Conn).DeleteAccount(apitest.User(t.Context(), profileUserA), &userv1.DeleteAccountRequest{}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM idempotency_keys WHERE key IN($1,$2)`, key, busyKey).Scan(&count); err != nil || count != 0 {
		t.Fatal("busy response survived account deletion", count, err)
	}
	again, err := startCall(t, env, foreignKey, foreignPhone)
	if err != nil || again.GetVerificationToken() != foreign.GetVerificationToken() {
		t.Fatal("other phone's no-user replay removed", again, err)
	}
}

func TestAPIStartAPIDeletionRetriesBusyPreAccountCacheKey(t *testing.T) {
	p := &creationProvider{}
	env := startEnv(t, p)
	key, phone := authKey(), "+79001234567"
	initial, err := startCall(t, env, key, phone)
	if err != nil {
		t.Fatal(err)
	}
	// The reply has no OwnerID: deletion must discover it through the phone link.
	seedProfiles(t, env)
	hold, err := env.App.DB.Primary().Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(context.Background()) }()
	if _, err = hold.Exec(t.Context(), `SELECT 1 FROM idempotency_keys WHERE key=$1 AND owner_id IS NULL FOR UPDATE`, key); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	profiles := userv1.NewProfileServiceClient(env.Conn)
	deleted := make(chan error, 1)
	go func() {
		_, err := profiles.DeleteAccount(apitest.User(ctx, profileUserA), &userv1.DeleteAccountRequest{})
		deleted <- err
	}()
	replayed := make(chan error, 1)
	go func() {
		_, err := userv1.NewAuthServiceClient(env.Conn).StartPhoneVerification(authContext(ctx, key), &userv1.StartPhoneVerificationRequest{Phone: phone, PdConsentAccepted: true, PdConsentVersion: "v1", AgeConfirmed: true})
		replayed <- err
	}()
	eventually(t, func() bool {
		var waiting bool
		err := env.App.DB.Primary().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND application_name='user-auth' AND wait_event_type='Lock')`).Scan(&waiting)
		return err == nil && waiting
	})
	theme := userv1.Theme_THEME_DARK
	if _, err = profiles.UpdateProfile(apitest.User(ctx, profileUserA), &userv1.UpdateProfileRequest{Theme: &theme}); err != nil {
		t.Fatal("deletion held guard while waiting for no-user Start key", err)
	}
	if err = hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = awaitStart(t, deleted); err != nil {
		t.Fatal(err)
	}
	// A replay serialized after deletion may be refused or create a genuinely new
	// request; it must not resurrect the old cached secret or its verification row.
	_ = awaitStart(t, replayed)
	var count int
	h := token.Sum(initial.GetVerificationToken())
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM phone_verification WHERE token_hash=$1`, h[:]).Scan(&count); err != nil || count != 0 {
		t.Fatal("deleted verification survived", count, err)
	}
}

func TestAPIStartAPIResourceBindingFailureRollsBackClaimAndLimits(t *testing.T) {
	p := &creationProvider{}
	env := startEnv(t, p)
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE idempotency_keys RENAME COLUMN resource_id TO unavailable`); err != nil {
		t.Fatal(err)
	}
	key, phone := authKey(), "+79001234567"
	if out, err := startCall(t, env, key, phone); out != nil || status.Code(err) != codes.Internal {
		t.Fatal(out, err)
	}
	var requests, keys int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM phone_verification),(SELECT count(*) FROM idempotency_keys WHERE key=$1)`, key).Scan(&requests, &keys); err != nil || requests != 0 || keys != 0 || startHits(t, env) != 0 {
		t.Fatal("failed link committed partial claim", requests, keys, err)
	}
	if creates, recovers := p.counts(); creates != 0 || recovers != 0 {
		t.Fatal("unlinked claim called provider", creates, recovers)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE idempotency_keys RENAME COLUMN unavailable TO resource_id`); err != nil {
		t.Fatal(err)
	}
	if _, err := startCall(t, env, key, phone); err != nil {
		t.Fatal("failed link prevented retry", err)
	}
}

func TestAPIStartAPIPurgeParentStorageFailureRollsBack(t *testing.T) {
	env := startEnv(t, &creationProvider{})
	seedProfiles(t, env)
	if _, err := startCall(t, env, authKey(), "+79001234567"); err != nil {
		t.Fatal(err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE phone_verification RENAME TO temporarily_unavailable`); err != nil {
		t.Fatal(err)
	}
	profiles := userv1.NewProfileServiceClient(env.Conn)
	if _, err := profiles.DeleteAccount(apitest.User(t.Context(), profileUserA), &userv1.DeleteAccountRequest{}); status.Code(err) != codes.Internal {
		t.Fatal(err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE temporarily_unavailable RENAME TO phone_verification`); err != nil {
		t.Fatal(err)
	}
	if _, err := profiles.GetProfile(apitest.User(t.Context(), profileUserA), &userv1.GetProfileRequest{}); err != nil {
		t.Fatal("failed parent lock partially purged account", err)
	}
	if _, err := profiles.DeleteAccount(apitest.User(t.Context(), profileUserA), &userv1.DeleteAccountRequest{}); err != nil {
		t.Fatal("failed parent lock prevented retry", err)
	}
}
