// Package auth exposes the implemented AuthService methods. The shared library
// validates metadata keys and commits encrypted replies with session effects.
package auth

import (
	"context"
	"errors"
	"math"
	"strconv"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	idempotency "github.com/fin-assist/go-idempotency"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/fin-assist/go-grpc/reqctx"

	"github.com/fin-assist/user/internal/adapter/grpc/retryinfo"
	usecase "github.com/fin-assist/user/internal/auth"
	"github.com/fin-assist/user/internal/domain"
	"github.com/fin-assist/user/internal/ratelimit"
	"github.com/fin-assist/user/internal/verification"
)

// Sessions is the consumer interface of the three implemented RPCs.
type Sessions interface {
	Register(context.Context, usecase.RegisterInput) (usecase.Registered, error)
	Refresh(context.Context, string) (usecase.Rotation, error)
	CompleteReuse(context.Context, string) error
	Logout(context.Context, string, string) error
}

// Phone is the provider-independent streaming lifecycle.
type Phone interface {
	Watch(context.Context, string, func(verification.Outcome) error) error
}

// PhoneStart owns the multi-transaction external-effect protocol.
type PhoneStart interface {
	StartWithConsent(context.Context, idempotency.Request, string, string, verification.Consent) (verification.Started, error)
}

// Engine is the library surface used by this transport adapter.
type Engine interface {
	UnaryInterceptor(func(context.Context) (string, error)) grpc.UnaryServerInterceptor
	WithCompletion(context.Context) (context.Context, context.CancelFunc)
	Do(context.Context, idempotency.Request, func(context.Context) (idempotency.Response, error)) (idempotency.Result, error)
}

// Transactions commits an explicit service transaction.
type Transactions interface {
	Run(context.Context, func(context.Context) error) error
}

// Limits counts requests before token checks, including rejected secrets.
type Limits interface {
	Check(context.Context, ratelimit.Operation, string, string) (ratelimit.Decision, ratelimit.Receipt, error)
}

// Guard rechecks cached replies against deletion before they leave the transaction.
type Guard interface {
	Guard(context.Context, string) (bool, error)
}

// Handler keeps the idempotency transaction separate from the attempt-counter
// transaction: a non-cacheable 401 must still consume an IP attempt.
type Handler struct {
	userv1.UnimplementedAuthServiceServer
	sessions        Sessions
	phone           Phone
	engine          Engine
	state, attempts Transactions
	limits          Limits
	guard           Guard
	start           PhoneStart
	now             func() time.Time
}

// Register installs the AuthService lifecycle and the phone creation coordinator.
func Register(server grpc.ServiceRegistrar, sessions Sessions, engine Engine, state, attempts Transactions, limits Limits, guard Guard, phone Phone, now func() time.Time, starts ...PhoneStart) {
	h := &Handler{sessions: sessions, engine: engine, state: state, attempts: attempts, limits: limits, guard: guard, phone: phone, now: now}
	if len(starts) > 0 {
		h.start = starts[0]
	}
	userv1.RegisterAuthServiceServer(server, h)
}

func (h *Handler) StartPhoneVerification(ctx context.Context, in *userv1.StartPhoneVerificationRequest) (*userv1.StartPhoneVerificationResponse, error) {
	var result verification.Started
	_, err := h.engine.UnaryInterceptor(func(context.Context) (string, error) { return "", nil })(ctx, in, &grpc.UnaryServerInfo{FullMethod: userv1.AuthService_StartPhoneVerification_FullMethodName}, func(ctx context.Context, _ any) (any, error) {
		request, ok := idempotency.FromContext(ctx)
		if !ok || h.start == nil {
			return nil, verification.ErrProviderUnavailable
		}
		var err error
		request.Hash, err = idempotency.HashProto(in)
		if err != nil {
			return nil, retryinfo.Apply(err)
		}
		result, err = h.start.StartWithConsent(ctx, request, in.GetPhone(), reqctx.ClientIP(ctx), verification.Consent{Accepted: in.GetPdConsentAccepted(), Version: in.GetPdConsentVersion(), AgeConfirmed: in.GetAgeConfirmed()})
		return nil, retryinfo.Apply(err)
	})
	if err != nil {
		return nil, retryinfo.Apply(err)
	}
	seconds := min(max(0, int64(result.ExpiresAt.Sub(h.now())/time.Second)), math.MaxInt32)
	left := int32(seconds) //nolint:gosec // clamped to [0, MaxInt32] on the line above
	return &userv1.StartPhoneVerificationResponse{VerificationToken: result.Token, CallbackNumber: result.CallbackNumber, ExpiresAt: timestamppb.New(result.ExpiresAt), ExpiresIn: &left}, nil
}

func (h *Handler) Register(ctx context.Context, in *userv1.RegisterRequest) (*userv1.RegisterResponse, error) {
	out := new(userv1.RegisterResponse)
	err := h.run(ctx, userv1.AuthService_Register_FullMethodName, in, out, ratelimit.Register, func(ctx context.Context) (proto.Message, string, bool, error) {
		r, err := h.sessions.Register(ctx, usecase.RegisterInput{Token: in.GetRegistrationToken(), Accepted: in.GetPdConsentAccepted(), ConsentVersion: in.GetPdConsentVersion(), Timezone: in.Timezone})
		if err != nil && r.VerificationID > 0 {
			err = &registrationFailure{cause: err, id: r.VerificationID}
		}
		return &userv1.RegisterResponse{Tokens: pair(r.Pair), UserId: r.UserID}, r.UserID, false, err
	}, nil)
	return out, err
}

func (h *Handler) RefreshTokens(ctx context.Context, in *userv1.RefreshTokensRequest) (*userv1.RefreshTokensResponse, error) {
	out := new(userv1.RefreshTokensResponse)
	err := h.run(ctx, userv1.AuthService_RefreshTokens_FullMethodName, in, out, ratelimit.Refresh, func(ctx context.Context) (proto.Message, string, bool, error) {
		r, err := h.sessions.Refresh(ctx, in.GetRefreshToken())
		return &userv1.RefreshTokensResponse{Tokens: pair(r.Pair)}, r.UserID, r.Reused, err
	}, func(ctx context.Context) error { return h.sessions.CompleteReuse(ctx, in.GetRefreshToken()) })
	return out, err
}

// Logout is a user call (grpc-api.md 3.3): the session owner comes from the verified
// access token, never from the refresh secret alone.
func (h *Handler) Logout(ctx context.Context, in *userv1.LogoutRequest) (*userv1.LogoutResponse, error) {
	id, ok := reqctx.UserID(ctx)
	if !ok {
		return nil, retryinfo.Apply(domain.WithRequestRetry(finerr.InvalidToken(), false))
	}
	if err := h.sessions.Logout(ctx, id, in.GetRefreshToken()); err != nil {
		return nil, retryinfo.Apply(err)
	}
	return &userv1.LogoutResponse{}, nil
}

type action func(context.Context) (proto.Message, string, bool, error)

func (h *Handler) run(ctx context.Context, method string, in, out proto.Message, op ratelimit.Operation, perform action, completeReuse func(context.Context) error) error {
	// Run the library's extraction interceptor here, after the server has already
	// checked service identity/access. This avoids a second key parser or identity
	// interceptor and keeps extraction confined to the implemented auth methods.
	_, err := h.engine.UnaryInterceptor(func(context.Context) (string, error) { return "", nil })(ctx, in, &grpc.UnaryServerInfo{FullMethod: method}, func(ctx context.Context, _ any) (any, error) {
		return nil, h.execute(ctx, in, out, op, perform, completeReuse)
	})
	return retryinfo.Apply(err)
}

func (h *Handler) execute(ctx context.Context, in, out proto.Message, op ratelimit.Operation, perform action, completeReuse func(context.Context) error) error {
	request, ok := idempotency.FromContext(ctx)
	if !ok {
		return errors.New("auth: idempotency context required")
	}
	var err error
	request.Hash, err = idempotency.HashProto(in)
	if err != nil {
		return err
	}
	completion, cancel := h.engine.WithCompletion(ctx)
	defer cancel()
	var result idempotency.Result
	var reused bool
	var effectFailure error
	err = h.state.Run(completion, func(ctx context.Context) error {
		var doErr error
		result, doErr = h.engine.Do(ctx, request, func(effect context.Context) (idempotency.Response, error) {
			// completion has no outer transaction. Commit the counter in the
			// ordinary pool before checking a secret in the dedicated auth pool.
			if err := h.attempt(completion, op); err != nil {
				return idempotency.Response{}, retryinfo.Apply(usecase.ContractError(err))
			}
			msg, id, reuse, err := perform(effect)
			reused = reuse
			if err != nil {
				// The idempotency library accepts only contract errors. Keep an
				// unexpected effect cause for the outer operation mapper/diagnostics.
				if _, contract := finerr.ToREST(err); !contract {
					effectFailure = err
				}
				response := idempotency.Response{}
				var linked *registrationFailure
				if errors.As(err, &linked) {
					response.Resource = idempotency.Resource{Type: "phone_verification", ID: strconv.FormatInt(int64(linked.id), 10)}
				}
				return response, retryinfo.Apply(usecase.ContractError(err))
			}
			body, err := proto.Marshal(msg)
			return idempotency.Response{Body: body, OwnerID: id}, err
		})
		if doErr != nil {
			if effectFailure != nil {
				return effectFailure
			}
			return doErr
		}
		if result.Err == nil && result.Replayed {
			if result.Response.OwnerID == "" {
				return errors.New("auth: cached result owner required")
			}
			alive, err := h.guard.Guard(ctx, result.Response.OwnerID)
			if err != nil {
				return err
			}
			if !alive {
				if request.Endpoint == userv1.AuthService_Register_FullMethodName {
					return domain.WithRequestRetry(finerr.InvalidToken(), false)
				}
				return domain.WithRequestRetry(finerr.TokenRevoked(), false)
			}
		}
		return nil
	})
	if errors.Is(err, idempotency.ErrNotStored) {
		if reused && completeReuse != nil {
			if err := completeReuse(completion); err != nil {
				return err
			}
		}
		return result.Err
	}
	if err != nil {
		return retryinfo.Apply(usecase.ContractError(err))
	}
	if result.Err != nil {
		return result.Err
	}
	return proto.Unmarshal(result.Response.Body, out)
}

func (h *Handler) attempt(ctx context.Context, op ratelimit.Operation) error {
	var d ratelimit.Decision
	var receipt ratelimit.Receipt
	err := h.attempts.Run(ctx, func(ctx context.Context) error {
		var err error
		d, receipt, err = h.limits.Check(ctx, op, "", reqctx.ClientIP(ctx))
		return err
	})
	if err != nil {
		return err
	}
	receipt.Committed()
	if !d.Allowed {
		return domain.WithRequestRetry(finerr.RateLimited(d.RetryAt), true)
	}
	return nil
}

func pair(p usecase.Pair) *userv1.TokenPair {
	return &userv1.TokenPair{AccessToken: p.Access, RefreshToken: p.Refresh}
}

func (h *Handler) WatchPhoneVerification(in *userv1.WatchPhoneVerificationRequest, stream grpc.ServerStreamingServer[userv1.WatchPhoneVerificationResponse]) error {
	if err := h.attempt(stream.Context(), ratelimit.Watch); err != nil {
		return retryinfo.Apply(err)
	}
	return retryinfo.Apply(h.phone.Watch(stream.Context(), in.GetVerificationToken(), func(o verification.Outcome) error { return stream.Send(verificationMessage(o)) }))
}
func verificationMessage(o verification.Outcome) *userv1.WatchPhoneVerificationResponse {
	out := &userv1.WatchPhoneVerificationResponse{}
	switch o.State {
	case verification.Pending:
		out.Status = userv1.VerificationStatus_VERIFICATION_STATUS_PENDING
	case verification.Expired:
		out.Status = userv1.VerificationStatus_VERIFICATION_STATUS_EXPIRED
	case verification.Verified:
		out.Status = userv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED
		out.IsNewUser = &o.IsNewUser
		if o.IsNewUser {
			out.RegistrationToken = &o.RegistrationToken
		} else {
			out.Tokens = pair(o.Pair)
		}
	}
	return out
}

// registrationFailure carries only an authenticated proof resource inside the service.
// It is extracted before finerr encoding and never becomes wire metadata.
type registrationFailure struct {
	cause error
	id    int32
}

func (e *registrationFailure) Error() string { return e.cause.Error() }
func (e *registrationFailure) Unwrap() error { return e.cause }
