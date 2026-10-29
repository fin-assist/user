package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	idempotency "github.com/fin-assist/go-idempotency"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/go-grpc/reqctx"

	usecase "github.com/fin-assist/user/internal/auth"
	"github.com/fin-assist/user/internal/domain"
	"github.com/fin-assist/user/internal/ratelimit"
)

var errDependency = errors.New("test dependency")

type txMarker struct{}
type fakeTx struct {
	state bool
	err   error
}

func (x *fakeTx) Run(ctx context.Context, fn func(context.Context) error) error {
	if x.state {
		ctx = context.WithValue(ctx, txMarker{}, true)
	} else if ctx.Value(txMarker{}) != nil {
		panic("attempt joined state transaction")
	}
	if err := fn(ctx); err != nil {
		return err
	}
	return x.err
}

type parserTx struct{}

func (parserTx) Tx(context.Context) (pgx.Tx, bool) { return nil, false }

type fakeEngine struct {
	parser *idempotency.Engine
	replay bool
	body   []byte
	owner  string
	last   idempotency.Response
	err    error
}

func (e *fakeEngine) UnaryInterceptor(f func(context.Context) (string, error)) grpc.UnaryServerInterceptor {
	return e.parser.UnaryInterceptor(f)
}
func (e *fakeEngine) WithCompletion(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}
func (e *fakeEngine) Do(ctx context.Context, _ idempotency.Request, fn func(context.Context) (idempotency.Response, error)) (idempotency.Result, error) {
	if e.err != nil {
		return idempotency.Result{}, e.err
	}
	if e.replay {
		return idempotency.Result{Replayed: true, Response: idempotency.Response{Body: e.body, OwnerID: e.owner}}, nil
	}
	r, err := fn(ctx)
	e.last = r
	if err != nil {
		if _, ok := finerr.ToREST(err); ok {
			return idempotency.Result{Err: err}, idempotency.ErrNotStored
		}
		return idempotency.Result{}, idempotency.ErrStorage
	}
	return idempotency.Result{Response: r}, nil
}

type fakeSessions struct {
	proofID          int32
	in               usecase.RegisterInput
	reused           bool
	err, completeErr error
	completed        bool
	logoutUser       string
	logoutToken      string
	logouts          int
}

func (s *fakeSessions) Register(_ context.Context, in usecase.RegisterInput) (usecase.Registered, error) {
	s.in = in
	return usecase.Registered{VerificationID: s.proofID, UserID: "user", Pair: usecase.Pair{Access: "access", Refresh: "refresh"}}, s.err
}
func (s *fakeSessions) Refresh(context.Context, string) (usecase.Rotation, error) {
	return usecase.Rotation{UserID: "user", Pair: usecase.Pair{Access: "new-access", Refresh: "new-refresh"}, Reused: s.reused}, s.err
}
func (s *fakeSessions) CompleteReuse(context.Context, string) error {
	s.completed = true
	return s.completeErr
}
func (s *fakeSessions) Logout(_ context.Context, id, raw string) error {
	s.logoutUser, s.logoutToken = id, raw
	s.logouts++
	return s.err
}

type fakeLimits struct {
	allowed bool
	err     error
	calls   int
}

func (l *fakeLimits) Check(ctx context.Context, op ratelimit.Operation, phone, ip string) (ratelimit.Decision, ratelimit.Receipt, error) {
	l.calls++
	if ip != "203.0.113.7" || phone != "" || (op != ratelimit.Register && op != ratelimit.Refresh && op != ratelimit.Watch) {
		panic("untrusted limit input")
	}
	return ratelimit.Decision{Allowed: l.allowed, RetryAt: time.Now().Add(time.Minute)}, ratelimit.Receipt{}, l.err
}

type fakeGuard struct {
	alive bool
	err   error
}

func (g *fakeGuard) Guard(context.Context, string) (bool, error) { return g.alive, g.err }

func handlerFixture(t *testing.T) (*Handler, *fakeSessions, *fakeEngine, *fakeLimits, *fakeGuard) {
	t.Helper()
	cipher, err := idempotency.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	parser, err := idempotency.New(parserTx{}, idempotency.Options{Clock: time.Now, Cipher: cipher, Registerer: prometheus.NewRegistry(), Methods: map[string]idempotency.Policy{userv1.AuthService_StartPhoneVerification_FullMethodName: {Encrypt: true}, userv1.AuthService_Register_FullMethodName: {Encrypt: true}, userv1.AuthService_RefreshTokens_FullMethodName: {Encrypt: true, TTL: time.Minute}}})
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeSessions{}
	e := &fakeEngine{parser: parser}
	l := &fakeLimits{allowed: true}
	g := &fakeGuard{alive: true}
	h := &Handler{sessions: s, engine: e, state: &fakeTx{state: true}, attempts: &fakeTx{}, limits: l, guard: g}
	return h, s, e, l, g
}
func handlerContext() context.Context {
	return reqctx.WithClientIP(metadata.NewIncomingContext(context.Background(), metadata.Pairs("idempotency-key", "019c7714-3b77-74d1-9866-e1f484aae2ab")), "203.0.113.7")
}
func grpcReason(err error) string {
	if r, ok := finerr.ToREST(err); ok {
		return r.Code
	}
	return ""
}

func TestUnitSessionHandlerMappingAndIndependentAttempts(t *testing.T) {
	h, s, e, l, _ := handlerFixture(t)
	zone := "UTC"
	r, err := h.Register(handlerContext(), &userv1.RegisterRequest{RegistrationToken: "reg", PdConsentAccepted: true, PdConsentVersion: "v1", Timezone: &zone})
	if err != nil || r.GetUserId() != "user" || r.GetTokens().GetAccessToken() != "access" || s.in.Timezone != &zone || s.in.Token != "reg" || !s.in.Accepted || s.in.ConsentVersion != "v1" || l.calls != 1 || e.last.OwnerID != "user" {
		t.Fatal(r, err)
	}
	f, err := h.RefreshTokens(handlerContext(), &userv1.RefreshTokensRequest{RefreshToken: "old"})
	if err != nil || f.GetTokens().GetRefreshToken() != "new-refresh" {
		t.Fatal(f, err)
	}
	user := reqctx.WithUserID(t.Context(), "user")
	if _, err := h.Logout(user, &userv1.LogoutRequest{RefreshToken: "refresh"}); err != nil || s.logoutUser != "user" || s.logoutToken != "refresh" {
		t.Fatal(s.logoutUser, s.logoutToken, err)
	}
	// The verified owner is required; the refresh secret alone deletes nothing.
	logouts := s.logouts
	if _, err := h.Logout(t.Context(), &userv1.LogoutRequest{RefreshToken: "refresh"}); grpcReason(err) != "INVALID_TOKEN" || s.logouts != logouts {
		t.Fatal(err)
	}
	s.err = errDependency
	if _, err := h.Logout(user, &userv1.LogoutRequest{}); !errors.Is(err, errDependency) {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	Register(server, s, h.engine, h.state, h.attempts, l, h.guard, nil, time.Now)
	if _, ok := server.GetServiceInfo()["finassist.user.v1.AuthService"]; !ok {
		t.Fatal("service absent")
	}
	server.Stop()
}

func TestUnitSessionHandlerKeysAndFailurePaths(t *testing.T) {
	for _, keys := range [][]string{nil, {"bad"}, {"019c7714-3b77-74d1-9866-e1f484aae2ab", "019c7714-3b77-74d1-9866-e1f484aae2ac"}} {
		h, _, _, l, _ := handlerFixture(t)
		ctx := metadata.NewIncomingContext(t.Context(), metadata.MD{"idempotency-key": keys})
		if _, err := h.Register(ctx, &userv1.RegisterRequest{}); grpcReason(err) != "VALIDATION_ERROR" || l.calls != 0 {
			t.Fatal(err)
		}
	}
	for _, mutate := range []func(*Handler, *fakeEngine, *fakeLimits){
		func(_ *Handler, e *fakeEngine, _ *fakeLimits) { e.err = idempotency.ErrStorage },
		func(_ *Handler, _ *fakeEngine, l *fakeLimits) { l.err = errDependency },
		func(h *Handler, _ *fakeEngine, _ *fakeLimits) {
			h.state = &fakeTx{state: true, err: errDependency}
		},
		func(h *Handler, _ *fakeEngine, _ *fakeLimits) {
			h.attempts = &fakeTx{err: errDependency}
		},
	} {
		h, _, e, l, _ := handlerFixture(t)
		mutate(h, e, l)
		if _, err := h.Register(handlerContext(), &userv1.RegisterRequest{}); err == nil {
			t.Fatal("dependency error accepted")
		}
	}
	h, _, _, l, _ := handlerFixture(t)
	l.allowed = false
	if _, err := h.Register(handlerContext(), &userv1.RegisterRequest{}); grpcReason(err) != "RATE_LIMITED" {
		t.Fatal(err)
	}
	h, s, _, _, _ := handlerFixture(t)
	s.err = finerr.ConsentOutdated()
	if _, err := h.Register(handlerContext(), &userv1.RegisterRequest{}); grpcReason(err) != "CONSENT_OUTDATED" {
		t.Fatal(err)
	}
	h, _, _, _, _ = handlerFixture(t)
	if _, err := h.Register(handlerContext(), &userv1.RegisterRequest{RegistrationToken: "\xff"}); err == nil {
		t.Fatal("malformed protobuf accepted")
	}
	if err := h.execute(t.Context(), &userv1.RegisterRequest{}, &userv1.RegisterResponse{}, ratelimit.Register, nil, nil); err == nil {
		t.Fatal("missing extraction accepted")
	}
}

func TestUnitSessionHandlerReplayGuardAndReuseCompletion(t *testing.T) {
	for _, mode := range []string{"live", "deleted", "missing-owner", "guard-error", "malformed-body"} {
		h, _, e, l, g := handlerFixture(t)
		e.replay = true
		e.owner = "user"
		var err error
		e.body, err = proto.Marshal(&userv1.RegisterResponse{UserId: "user"})
		if err != nil {
			t.Fatal(err)
		}
		switch mode {
		case "deleted":
			g.alive = false
		case "missing-owner":
			e.owner = ""
		case "guard-error":
			g.err = errDependency
		case "malformed-body":
			e.body = []byte{0xff}
		}
		r, err := h.Register(handlerContext(), &userv1.RegisterRequest{})
		if mode == "live" {
			if err != nil || r.GetUserId() != "user" {
				t.Fatal(r, err)
			}
		} else if err == nil {
			t.Fatal(mode)
		}
		if l.calls != 0 {
			t.Fatal("replay executed effect")
		}
	}
	h, s, _, _, _ := handlerFixture(t)
	s.reused = true
	s.err = finerr.TokenRevoked()
	if _, err := h.RefreshTokens(handlerContext(), &userv1.RefreshTokensRequest{}); grpcReason(err) != "TOKEN_REVOKED" || !s.completed {
		t.Fatal(err)
	}
	s.completeErr = errDependency
	if _, err := h.RefreshTokens(handlerContext(), &userv1.RefreshTokensRequest{}); !errors.Is(err, errDependency) {
		t.Fatal("revocation failure hidden", err)
	}
}

func TestUnitUnknownEffectCauseSurvivesIdempotencyBoundary(t *testing.T) {
	h, s, e, _, _ := handlerFixture(t)
	s.err = errDependency
	if _, err := h.Register(handlerContext(), &userv1.RegisterRequest{}); !errors.Is(err, errDependency) {
		t.Fatal("effect cause replaced by ErrStorage", err)
	}
	s.err = nil
	e.err = idempotency.ErrStorage
	if _, err := h.Register(handlerContext(), &userv1.RegisterRequest{}); !errors.Is(err, idempotency.ErrStorage) {
		t.Fatal("engine error replaced", err)
	}
	e.err = nil
	s.err = finerr.InvalidToken()
	if _, err := h.Register(handlerContext(), &userv1.RegisterRequest{}); grpcReason(err) != "INVALID_TOKEN" {
		t.Fatal("ErrNotStored result lost", err)
	}
	h.state = &fakeTx{state: true, err: usecase.ErrMissing}
	s.err = nil
	if _, err := h.Register(handlerContext(), &userv1.RegisterRequest{}); grpcReason(err) != "INVALID_TOKEN" {
		t.Fatal("late domain error not translated", err)
	}
}

func TestUnitAuthPhaseAdviceBeforeEffectAndUnknownCommit(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		h, _, _, limits, _ := handlerFixture(t)
		limits.allowed = false
		var err error
		if refresh {
			_, err = h.RefreshTokens(handlerContext(), &userv1.RefreshTokensRequest{})
		} else {
			_, err = h.Register(handlerContext(), &userv1.RegisterRequest{})
		}
		rest, ok := finerr.ToREST(err)
		if !ok || rest.RequestRetryable == nil || !*rest.RequestRetryable || rest.RetryAfter <= 0 {
			t.Fatal(rest, err)
		}
	}
	h, sessions, _, _, _ := handlerFixture(t)
	sessions.err = domain.WithRequestRetry(finerr.ConsentOutdated(), false)
	_, err := h.Register(handlerContext(), &userv1.RegisterRequest{})
	rest, _ := finerr.ToREST(err)
	if rest.RequestRetryable == nil || *rest.RequestRetryable {
		t.Fatal("lost explicit refusal", rest)
	}
	h, _, _, limits, _ := handlerFixture(t)
	limits.allowed = false
	h.attempts = &fakeTx{err: errDependency}
	_, err = h.Register(handlerContext(), &userv1.RegisterRequest{})
	if rest, ok := finerr.ToREST(err); ok && rest.RequestRetryable != nil {
		t.Fatal("failed limiter commit became permission", rest)
	}
	h, _, engine, _, _ := handlerFixture(t)
	engine.err = finerr.RateLimited(time.Now().Add(time.Minute))
	_, err = h.Register(handlerContext(), &userv1.RegisterRequest{})
	rest, _ = finerr.ToREST(err)
	if rest.RequestRetryable != nil {
		t.Fatal("unmarked engine result gained permission", rest)
	}
}

func TestUnitFailedRegisterOnlyBindsAnAuthenticatedProofResource(t *testing.T) {
	for _, id := range []int32{0, 7} {
		h, s, e, _, _ := handlerFixture(t)
		s.proofID = id
		s.err = finerr.ValidationError(&errdetails.BadRequest_FieldViolation{Field: "timezone", Reason: "INVALID_FORMAT"})
		_, err := h.Register(handlerContext(), &userv1.RegisterRequest{RegistrationToken: "valid-proof", PdConsentAccepted: true, PdConsentVersion: "v1"})
		if grpcReason(err) != "VALIDATION_ERROR" {
			t.Fatal(err)
		}
		if id == 0 {
			if e.last.Resource != (idempotency.Resource{}) {
				t.Fatal("untrusted input bound to proof")
			}
		} else if e.last.Resource != (idempotency.Resource{Type: "phone_verification", ID: "7"}) {
			t.Fatal("authenticated proof resource lost", e.last.Resource)
		}
	}
}
