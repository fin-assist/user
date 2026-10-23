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
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	usecase "github.com/fin-assist/user/internal/auth"
	"github.com/fin-assist/user/internal/ratelimit"
	"github.com/fin-assist/user/internal/reqctx"
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
	in               usecase.RegisterInput
	reused           bool
	err, completeErr error
	completed        bool
}

func (s *fakeSessions) Register(_ context.Context, in usecase.RegisterInput) (usecase.Registered, error) {
	s.in = in
	return usecase.Registered{UserID: "user", Pair: usecase.Pair{Access: "access", Refresh: "refresh"}}, s.err
}
func (s *fakeSessions) Refresh(context.Context, string) (usecase.Rotation, error) {
	return usecase.Rotation{UserID: "user", Pair: usecase.Pair{Access: "new-access", Refresh: "new-refresh"}, Reused: s.reused}, s.err
}
func (s *fakeSessions) CompleteReuse(context.Context, string) error {
	s.completed = true
	return s.completeErr
}
func (s *fakeSessions) Logout(context.Context, string) error { return s.err }

type fakeLimits struct {
	allowed bool
	err     error
	calls   int
}

func (l *fakeLimits) Check(ctx context.Context, op ratelimit.Operation, phone, ip string) (ratelimit.Decision, ratelimit.Receipt, error) {
	l.calls++
	if ip != "203.0.113.7" || phone != "" || (op != ratelimit.Register && op != ratelimit.Refresh) {
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
	parser, err := idempotency.New(parserTx{}, idempotency.Options{Clock: time.Now, Cipher: cipher, Registerer: prometheus.NewRegistry(), Methods: map[string]idempotency.Policy{userv1.AuthService_Register_FullMethodName: {Encrypt: true}, userv1.AuthService_RefreshTokens_FullMethodName: {Encrypt: true, TTL: time.Minute}}})
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

func TestSessionHandlerMappingAndIndependentAttempts(t *testing.T) {
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
	if _, err := h.Logout(t.Context(), &userv1.LogoutRequest{}); err != nil {
		t.Fatal(err)
	}
	s.err = errDependency
	if _, err := h.Logout(t.Context(), &userv1.LogoutRequest{}); !errors.Is(err, errDependency) {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	Register(server, s, h.engine, h.state, h.attempts, l, h.guard)
	if _, ok := server.GetServiceInfo()["finassist.user.v1.AuthService"]; !ok {
		t.Fatal("service absent")
	}
	server.Stop()
}

func TestSessionHandlerKeysAndFailurePaths(t *testing.T) {
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

func TestSessionHandlerReplayGuardAndReuseCompletion(t *testing.T) {
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
