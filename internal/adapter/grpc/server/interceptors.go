package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"regexp"
	"runtime/debug"
	"time"

	"buf.build/go/protovalidate"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/user/internal/reqctx"
)

// Metadata keys (grpc-api.md 4).
const (
	mdAuthorization        = "authorization"
	mdServiceAuthorization = "x-service-authorization"
	mdRequestID            = "x-request-id"
	mdClientIP             = "x-client-ip"
	mdClientVersion        = "x-client-version"
	mdReadPrimary          = "x-read-primary"
)

// bankBFF is the only caller whose client hints (x-client-ip, x-client-version,
// x-read-primary) are trusted: it computes or copies them from the client connection.
const bankBFF = "bank-bff"

// requestID takes x-request-id or makes one. A value of unexpected shape is replaced, not
// logged: it comes from a client header through bank-bff.
var requestIDShape = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func withRequestID(ctx context.Context) context.Context {
	id := first(ctx, mdRequestID)
	if !requestIDShape.MatchString(id) {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		id = hex.EncodeToString(b)
	}
	return reqctx.WithRequestID(ctx, id)
}

// authorize checks the service token, the access table, the user token and the
// tombstone, in this order; see Rule and Model.
func (s *Server) authorize(ctx context.Context, method string) (context.Context, error) {
	if isOpen(method) {
		return ctx, nil
	}
	token, ok := bearer(first(ctx, mdServiceAuthorization))
	if !ok {
		s.denied.WithLabelValues(method, "", "no_service_token").Inc()
		return ctx, bare(codes.Unauthenticated)
	}
	caller, err := s.deps.Services.VerifyService(ctx, token)
	if err != nil {
		s.denied.WithLabelValues(method, "", "bad_service_token").Inc()
		s.log.WarnContext(ctx, "service token rejected", "method", method, "error", err)
		return ctx, bare(codes.Unauthenticated)
	}
	rule, ok := s.deps.Policy.lookup(method)
	if !ok || !rule.allows(caller) {
		// A foreign caller: bare PERMISSION_DENIED and an alert (grpc-api.md 3.3). The
		// caller label is a verified ServiceAccount name, so its set is small.
		s.denied.WithLabelValues(method, caller, "caller_not_allowed").Inc()
		s.log.ErrorContext(ctx, "caller not allowed", "method", method, "caller", caller)
		return ctx, bare(codes.PermissionDenied)
	}
	ctx = reqctx.WithCaller(ctx, caller)
	if caller == bankBFF {
		ctx = clientHints(ctx)
	}

	userToken, hasUser := bearer(first(ctx, mdAuthorization))
	switch rule.Model {
	case ModelSystem, ModelNoUser:
		// Neither acts for a user: an authorization header, if any, is ignored.
		return ctx, nil
	case ModelUser:
		if !hasUser {
			return ctx, finerr.InvalidToken()
		}
	case ModelMixed:
		if !hasUser {
			return ctx, nil // the system variant: user_id is a request field
		}
	default:
		// Policy.validate rejects such a rule before Serve; refuse rather than guess.
		s.log.ErrorContext(ctx, "access rule without a model", "method", method)
		return ctx, bare(codes.PermissionDenied)
	}
	userID, err := s.deps.Users.VerifyUser(ctx, userToken)
	if err != nil {
		return ctx, err
	}
	if userID == "" {
		// A verifier that accepts a token without a subject would turn a user call into a
		// system one (reqctx.UserID reports no user): fail closed instead.
		s.log.ErrorContext(ctx, "user token verified without a subject", "method", method)
		return ctx, finerr.InvalidToken()
	}
	deleted, err := s.deps.Tombstones.IsDeleted(ctx, userID)
	if err != nil {
		s.log.ErrorContext(ctx, "tombstone check failed", "method", method, "error", err)
		return ctx, bare(codes.Unavailable) // nothing ran yet: safe to retry
	}
	if deleted && !rule.AllowDeleted {
		return ctx, finerr.TokenRevoked()
	}
	return reqctx.WithUserID(ctx, userID), nil
}

func clientHints(ctx context.Context) context.Context {
	if ip := first(ctx, mdClientIP); ip != "" {
		ctx = reqctx.WithClientIP(ctx, ip)
	}
	if v := first(ctx, mdClientVersion); v != "" {
		ctx = reqctx.WithClientVersion(ctx, v)
	}
	if first(ctx, mdReadPrimary) == "1" {
		ctx = reqctx.WithReadPrimary(ctx)
	}
	return ctx
}

func first(ctx context.Context, key string) string {
	if vs := metadata.ValueFromIncomingContext(ctx, key); len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// validate checks a request message by its protovalidate annotations. Violations become
// VALIDATION_ERROR with one BadRequest entry per field (grpc-api.md 8, «Валидация полей»).
func (s *Server) validate(ctx context.Context, msg any) error {
	m, ok := msg.(proto.Message)
	if !ok {
		return nil
	}
	err := s.validator.Validate(m)
	if err == nil {
		return nil
	}
	var verr *protovalidate.ValidationError
	if !asValidation(err, &verr) {
		// A broken annotation (compilation or runtime error) is our defect, not the caller's.
		s.log.ErrorContext(ctx, "protovalidate failed", "message", m.ProtoReflect().Descriptor().FullName(), "error", err)
		return finerr.InternalError()
	}
	vs := make([]*errdetails.BadRequest_FieldViolation, 0, len(verr.Violations))
	for _, v := range verr.Violations {
		vs = append(vs, &errdetails.BadRequest_FieldViolation{
			Field:       protovalidate.FieldPathString(v.Proto.GetField()),
			Reason:      violationCode(v.Proto.GetRuleId()),
			Description: v.Proto.GetMessage(),
		})
	}
	return finerr.ValidationError(vs...)
}

func asValidation(err error, target **protovalidate.ValidationError) bool {
	v, ok := err.(*protovalidate.ValidationError) //nolint:errorlint // protovalidate returns it unwrapped
	if ok {
		*target = v
	}
	return ok
}

// violationCode maps a protovalidate rule ID to details[].code of api.md: REQUIRED,
// INVALID_FORMAT, OUT_OF_RANGE.
func violationCode(ruleID string) string {
	switch {
	case ruleID == "required":
		return "REQUIRED"
	case formatRules.MatchString(ruleID):
		return "INVALID_FORMAT"
	default:
		return "OUT_OF_RANGE"
	}
}

var formatRules = regexp.MustCompile(`\.(pattern|email|hostname|ip|ipv4|ipv6|uri|uri_ref|uuid|tuuid|address|well_known_regex|prefix|suffix|contains|not_contains|well_known)(_|$)|\.(defined_only)$`)

// callInfo is what the interceptors know about a request for its log line and metrics.
type callInfo struct {
	method string
	start  time.Time
	req    any // the request message of a unary call; nil for a stream
}

// finish ends a call: cause is what the handler (or an interceptor) returned, the result
// is what the caller gets (normalize). The log line carries the cause, the metrics and the
// caller see the result. Labels: method, code, reason — never user_id or another unbounded
// value (grpc-api.md 9).
func (s *Server) finish(ctx context.Context, c callInfo, cause error) error {
	err := normalize(cause)
	code := status.Code(err)
	r := Reason(err)
	s.handled.WithLabelValues(c.method, code.String(), r).Inc()
	s.duration.WithLabelValues(c.method).Observe(time.Since(c.start).Seconds())

	level := slog.LevelInfo
	switch code {
	case codes.Internal, codes.Unknown, codes.DataLoss:
		level = slog.LevelError
	}
	attrs := []any{"method", c.method, "code", code.String(), "duration_ms", time.Since(c.start).Milliseconds()}
	if r != "" {
		attrs = append(attrs, "reason", r)
	}
	if cause != nil {
		// status.message never reaches the client (grpc-api.md 5.2); here is where it is read.
		attrs = append(attrs, "error", cause.Error())
	}
	if s.log.Enabled(ctx, slog.LevelDebug) {
		md, _ := metadata.FromIncomingContext(ctx)
		attrs = append(attrs, "metadata", maskMetadata(md))
		if m, ok := c.req.(proto.Message); ok {
			attrs = append(attrs, "request", maskMessage(m))
		}
	}
	s.log.Log(ctx, level, "grpc call", attrs...)
	return err
}

func (s *Server) recover(ctx context.Context, method string, err *error) {
	if p := recover(); p != nil {
		s.log.ErrorContext(ctx, "panic in handler", "method", method, "panic", p, "stack", string(debug.Stack()))
		*err = bare(codes.Internal)
	}
}

// The deferred functions run in reverse order: recover turns a panic into a bare INTERNAL,
// then finish normalizes the error for the caller and logs the call with its cause.
func (s *Server) unary(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
	c := callInfo{method: info.FullMethod, start: time.Now(), req: req}
	ctx = withRequestID(ctx)
	defer func() { err = s.finish(ctx, c, err) }()
	defer s.recover(ctx, info.FullMethod, &err)

	if ctx, err = s.authorize(ctx, info.FullMethod); err != nil {
		return nil, err
	}
	if err = s.validate(ctx, req); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

func (s *Server) stream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
	c := callInfo{method: info.FullMethod, start: time.Now()}
	ctx := withRequestID(ss.Context())
	defer func() { err = s.finish(ctx, c, err) }()
	defer s.recover(ctx, info.FullMethod, &err)

	if ctx, err = s.authorize(ctx, info.FullMethod); err != nil {
		return err
	}
	return handler(srv, &serverStream{ServerStream: ss, ctx: ctx, s: s})
}

// serverStream replaces the context and validates every received message.
type serverStream struct {
	grpc.ServerStream
	ctx context.Context
	s   *Server
}

func (w *serverStream) Context() context.Context { return w.ctx }

func (w *serverStream) RecvMsg(m any) error {
	if err := w.ServerStream.RecvMsg(m); err != nil {
		return err
	}
	return w.s.validate(w.ctx, m)
}

// metrics of the server, registered in New.
func newMetrics(reg prometheus.Registerer) (handled *prometheus.CounterVec, duration *prometheus.HistogramVec, denied *prometheus.CounterVec, err error) {
	handled = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "grpc_server_handled_total",
		Help: "Completed gRPC calls by method, status code and ErrorInfo reason.",
	}, []string{"method", "code", "reason"})
	duration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "grpc_server_handling_seconds",
		Help:    "Duration of gRPC calls by method.",
		Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 180},
	}, []string{"method"})
	denied = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "finassist_grpc_denied_total",
		Help: "Calls refused before the handler: missing or bad service token, caller not in the access table. Alert on caller_not_allowed.",
	}, []string{"method", "caller", "cause"})
	for _, c := range []prometheus.Collector{handled, duration, denied} {
		if err = reg.Register(c); err != nil {
			return nil, nil, nil, err
		}
	}
	return handled, duration, denied, nil
}
