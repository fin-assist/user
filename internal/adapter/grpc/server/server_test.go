package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/fin-assist/user/internal/logx"
	"github.com/fin-assist/user/internal/reqctx"
)

const (
	emptyCall = "/grpc.testing.TestService/EmptyCall"
	unaryCall = "/grpc.testing.TestService/UnaryCall"
	streamOut = "/grpc.testing.TestService/StreamingOutputCall"
)

// seen records what the handler got from the interceptors.
type seen struct {
	user, caller, ip, version string
	hasUser, readPrimary      bool
}

type testService struct {
	testgrpc.UnimplementedTestServiceServer
	got   chan seen
	panic bool
	err   error // what UnaryCall returns instead of finerr.NotFound()
}

func record(ctx context.Context) seen {
	u, ok := reqctx.UserID(ctx)
	return seen{user: u, hasUser: ok, caller: reqctx.Caller(ctx), ip: reqctx.ClientIP(ctx),
		version: reqctx.ClientVersion(ctx), readPrimary: reqctx.ReadPrimary(ctx)}
}

func (t *testService) EmptyCall(ctx context.Context, _ *testgrpc.Empty) (*testgrpc.Empty, error) {
	if t.panic {
		panic("handler bug")
	}
	t.got <- record(ctx)
	return &testgrpc.Empty{}, nil
}

func (t *testService) UnaryCall(ctx context.Context, _ *testgrpc.SimpleRequest) (*testgrpc.SimpleResponse, error) {
	t.got <- record(ctx)
	if t.err != nil {
		return nil, t.err
	}
	return nil, finerr.NotFound()
}

func (t *testService) StreamingOutputCall(_ *testgrpc.StreamingOutputCallRequest, ss testgrpc.TestService_StreamingOutputCallServer) error {
	if t.panic {
		panic("stream handler bug")
	}
	t.got <- record(ss.Context())
	if t.err != nil {
		return t.err
	}
	return ss.Send(&testgrpc.StreamingOutputCallResponse{})
}

type deleted map[string]bool

func (d deleted) IsDeleted(_ context.Context, id string) (bool, error) {
	if id == "broken" {
		return false, errors.New("db down")
	}
	return d[id], nil
}

type env struct {
	srv  *Server
	svc  *testService
	conn *grpc.ClientConn
	logs *bytes.Buffer
	reg  *prometheus.Registry
}

func policy() Policy {
	return Policy{
		"/grpc.testing.TestService/*":   {Callers: []string{"bank-bff"}, Model: ModelUser},
		unaryCall:                       {Callers: []string{"bank-bff", "assistant"}, Model: ModelMixed},
		streamOut:                       {Callers: []string{"user"}, Model: ModelSystem},
		"/grpc.testing.TestService/Nop": {Callers: []string{AnyCaller}, Model: ModelNoUser},
	}
}

// start runs a server with the dev verifiers and p; adjust, if given, changes the Deps
// (another verifier) before New.
func start(t *testing.T, p Policy, adjust ...func(*Deps)) *env {
	t.Helper()
	logs := &bytes.Buffer{}
	reg := prometheus.NewRegistry()
	deps := Deps{
		Logger:     logx.New(logs, "json", slog.LevelDebug, "test"), // the production logger: context values in every line
		Metrics:    reg,
		Policy:     p,
		Services:   DevServiceVerifier{},
		Users:      DevUserVerifier{},
		Tombstones: deleted{"gone": true},
	}
	for _, a := range adjust {
		a(&deps)
	}
	srv, err := New(Options{}, deps)
	if err != nil {
		t.Fatal(err)
	}
	svc := &testService{got: make(chan seen, 1)}
	testgrpc.RegisterTestServiceServer(srv.GRPC(), svc)
	lis := bufconn.Listen(1 << 20)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { srv.grpc.Stop() })
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &env{srv: srv, svc: svc, conn: conn, logs: logs, reg: reg}
}

func call(t *testing.T, caller, user string, kv ...string) context.Context {
	md := metadata.Pairs(kv...)
	if caller != "" {
		md.Set("x-service-authorization", "Bearer dev:"+caller)
	}
	if user != "" {
		md.Set("authorization", "Bearer dev:"+user)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return metadata.NewOutgoingContext(ctx, md)
}

func reasonOf(err error) string { return Reason(err) }

func TestIntegrationUserCall(t *testing.T) {
	e := start(t, policy())
	c := testgrpc.NewTestServiceClient(e.conn)
	ctx := call(t, "bank-bff", "u1", "x-client-ip", "203.0.113.7", "x-client-version", "android/0.2.5+41", "x-read-primary", "1")
	if _, err := c.EmptyCall(ctx, &testgrpc.Empty{}); err != nil {
		t.Fatal(err)
	}
	got := <-e.svc.got
	want := seen{user: "u1", hasUser: true, caller: "bank-bff", ip: "203.0.113.7", version: "android/0.2.5+41", readPrimary: true}
	if got != want {
		t.Fatalf("handler saw %+v, want %+v", got, want)
	}
}

func TestIntegrationRefusals(t *testing.T) {
	e := start(t, policy())
	c := testgrpc.NewTestServiceClient(e.conn)
	cases := []struct {
		name   string
		ctx    context.Context
		code   codes.Code
		reason string
	}{
		{"no service token", call(t, "", "u1"), codes.Unauthenticated, ""},
		{"bad service token", metadata.AppendToOutgoingContext(context.Background(), "x-service-authorization", "Bearer forged"), codes.Unauthenticated, ""},
		{"foreign caller", call(t, "analytics", "u1"), codes.PermissionDenied, ""},
		{"no user token", call(t, "bank-bff", ""), codes.Unauthenticated, "INVALID_TOKEN"},
		{"bad user token", call(t, "bank-bff", "", "authorization", "Bearer forged"), codes.Unauthenticated, "INVALID_TOKEN"},
		{"deleted user", call(t, "bank-bff", "gone"), codes.Unauthenticated, "TOKEN_REVOKED"},
		{"tombstone unavailable", call(t, "bank-bff", "broken"), codes.Unavailable, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.EmptyCall(tc.ctx, &testgrpc.Empty{})
			if status.Code(err) != tc.code || reasonOf(err) != tc.reason {
				t.Fatalf("got %v (reason %q), want %v %q", err, reasonOf(err), tc.code, tc.reason)
			}
			if tc.reason == "" && len(status.Convert(err).Details()) != 0 {
				t.Fatal("an internal refusal must carry no details")
			}
		})
	}
	if n := testutil.ToFloat64(e.srv.denied.WithLabelValues(emptyCall, "analytics", "caller_not_allowed")); n != 1 {
		t.Fatalf("denied metric %v, want 1", n)
	}
	select {
	case <-e.svc.got:
		t.Fatal("the handler must not run for a refused call")
	default:
	}
}

func TestIntegrationHintsOnlyFromBankBFF(t *testing.T) {
	e := start(t, Policy{"/grpc.testing.TestService/*": {Callers: []string{"assistant"}, Model: ModelSystem}})
	c := testgrpc.NewTestServiceClient(e.conn)
	ctx := call(t, "assistant", "u1", "x-client-ip", "203.0.113.7", "x-read-primary", "1")
	if _, err := c.EmptyCall(ctx, &testgrpc.Empty{}); err != nil {
		t.Fatal(err)
	}
	got := <-e.svc.got
	if got.ip != "" || got.readPrimary {
		t.Fatalf("hints from %s must be ignored: %+v", got.caller, got)
	}
	if got.hasUser {
		t.Fatal("a system call must not act for the user of an authorization header")
	}
}

func TestIntegrationMixedModel(t *testing.T) {
	e := start(t, policy())
	c := testgrpc.NewTestServiceClient(e.conn)
	_, err := c.UnaryCall(call(t, "assistant", ""), &testgrpc.SimpleRequest{})
	if reasonOf(err) != "NOT_FOUND" {
		t.Fatalf("handler error must pass through: %v", err)
	}
	if got := <-e.svc.got; got.hasUser {
		t.Fatalf("system variant: %+v", got)
	}
	_, _ = c.UnaryCall(call(t, "bank-bff", "u2"), &testgrpc.SimpleRequest{})
	if got := <-e.svc.got; got.user != "u2" {
		t.Fatalf("user variant: %+v", got)
	}
}

func TestIntegrationStream(t *testing.T) {
	e := start(t, policy())
	c := testgrpc.NewTestServiceClient(e.conn)
	st, err := c.StreamingOutputCall(call(t, "user", ""), &testgrpc.StreamingOutputCallRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Recv(); err != nil {
		t.Fatal(err)
	}
	if got := <-e.svc.got; got.caller != "user" {
		t.Fatalf("stream: %+v", got)
	}
	if _, err := st.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream: %v", err)
	}

	st, _ = c.StreamingOutputCall(call(t, "bank-bff", "u1"), &testgrpc.StreamingOutputCallRequest{})
	if _, err := st.Recv(); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign stream caller: %v", err)
	}
}

func TestIntegrationPanicIsInternalAndLogged(t *testing.T) {
	e := start(t, policy())
	e.svc.panic = true
	c := testgrpc.NewTestServiceClient(e.conn)
	_, err := c.EmptyCall(call(t, "bank-bff", "u1"), &testgrpc.Empty{})
	if status.Code(err) != codes.Internal || strings.Contains(status.Convert(err).Message(), "handler bug") {
		t.Fatalf("unary: got %v: want a bare INTERNAL without the panic text", err)
	}
	st, err := c.StreamingOutputCall(call(t, "user", ""), &testgrpc.StreamingOutputCallRequest{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.Recv()
	if status.Code(err) != codes.Internal || strings.Contains(status.Convert(err).Message(), "handler bug") {
		t.Fatalf("stream: got %v: want a bare INTERNAL without the panic text", err)
	}
	if n := strings.Count(e.logs.String(), "panic in handler"); n != 2 {
		t.Fatalf("panic must be logged for both calls, got %d lines", n)
	}
	// finish runs after recover: the call line and the metric see INTERNAL, not OK.
	for _, m := range []string{emptyCall, streamOut} {
		if n := testutil.ToFloat64(e.srv.handled.WithLabelValues(m, "Internal", "")); n != 1 {
			t.Fatalf("%s: handled{code=Internal} = %v, want 1: finish must run after recover", m, n)
		}
	}
	if n := strings.Count(e.logs.String(), `"code":"Internal"`); n != 2 {
		t.Fatalf("want two call lines with code Internal, got %d: %s", n, e.logs.String())
	}
}

// subjectless accepts every token but names no user, as a broken verifier might.
type subjectless struct{}

func (subjectless) VerifyUser(context.Context, string) (string, error) { return "", nil }

func TestIntegrationUserTokenWithoutSubjectIsRefused(t *testing.T) {
	e := start(t, policy(), func(d *Deps) { d.Users = subjectless{} })
	_, err := testgrpc.NewTestServiceClient(e.conn).EmptyCall(call(t, "bank-bff", "u1"), &testgrpc.Empty{})
	if status.Code(err) != codes.Unauthenticated || reasonOf(err) != "INVALID_TOKEN" {
		t.Fatalf("got %v: an empty subject must not pass as a system call", err)
	}
	select {
	case got := <-e.svc.got:
		t.Fatalf("the handler ran as %+v", got)
	default:
	}
}

// secretText stands for what an internal error may carry: a table name, a DSN, a path.
const secretText = `relation "user_secret" does not exist`

// normalizeCases are handler errors with what the caller must see instead. details lists
// the detail types of a wrapped registry error that must survive the unwrapping.
var normalizeCases = []struct {
	name    string
	err     error
	code    codes.Code
	reason  string
	details []string
}{
	{"plain error", errors.New(secretText), codes.Internal, "", nil},
	{"wrapped registry error", fmt.Errorf("load %s: %w", secretText, finerr.NotFound()), codes.NotFound, "NOT_FOUND", nil},
	{"wrapped validation error", fmt.Errorf("check %s: %w", secretText,
		finerr.ValidationError(&errdetails.BadRequest_FieldViolation{Field: "phone", Reason: "INVALID_FORMAT"})),
		codes.InvalidArgument, "VALIDATION_ERROR", []string{"google.rpc.BadRequest"}},
	{"wrapped rate limit", fmt.Errorf("limit %s: %w", secretText, finerr.RateLimited(time.Now().Add(time.Minute))),
		codes.ResourceExhausted, "RATE_LIMITED", []string{"google.rpc.RetryInfo"}},
	{"wrapped deadline", fmt.Errorf("query %s: %w", secretText, context.DeadlineExceeded), codes.DeadlineExceeded, "", nil},
	{"wrapped cancel", fmt.Errorf("query %s: %w", secretText, context.Canceled), codes.Canceled, "", nil},
}

// detailNames lists the full names of the detail messages of a status.
func detailNames(err error) []string {
	var names []string
	for _, d := range status.Convert(err).Details() {
		if m, ok := d.(proto.Message); ok {
			names = append(names, string(m.ProtoReflect().Descriptor().FullName()))
		}
	}
	return names
}

func TestUnitNormalize(t *testing.T) {
	if normalize(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	for _, same := range []error{bare(codes.Unavailable), finerr.NotFound(), finerr.LimitExceeded(time.Now())} {
		if got := normalize(same); !proto.Equal(status.Convert(got).Proto(), status.Convert(same).Proto()) {
			t.Fatalf("a status must pass unchanged: %v -> %v", same, got)
		}
	}
	for _, tc := range normalizeCases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalize(tc.err)
			st := status.Convert(got)
			if st.Code() != tc.code || Reason(got) != tc.reason || strings.Contains(st.Message(), "user_secret") {
				t.Fatalf("got %v, want %v %q without the wrapping text", got, tc.code, tc.reason)
			}
			for _, want := range tc.details {
				if !slices.Contains(detailNames(got), want) {
					t.Fatalf("detail %s lost: %v", want, detailNames(got))
				}
			}
			if _, ok := finerr.ToREST(got); ok != (tc.reason != "") {
				t.Fatalf("bank-bff translation ok=%v for %v", ok, got)
			}
		})
	}
	// A status of OK presented as an error would turn the failure into a success.
	okErr := fmt.Errorf("wrapped: %w", okStatus{})
	if st := status.Convert(normalize(okErr)); st.Code() != codes.Internal {
		t.Fatalf("OK status as an error: got %v, want INTERNAL", st.Code())
	}
}

// okStatus is an error whose status is OK, which no constructor here builds.
type okStatus struct{}

func (okStatus) Error() string              { return "ok" }
func (okStatus) GRPCStatus() *status.Status { return status.New(codes.OK, "ok") } //nolint:forbidigo // a malformed status for the test

// What the handler returns without a status must not travel to the caller as text:
// grpc-go would send UNKNOWN with err.Error() as the message. Unary and stream handlers
// go through separate interceptors, so both are exercised.
func TestIntegrationHandlerErrorsAreNormalized(t *testing.T) {
	for _, tc := range normalizeCases {
		for _, kind := range []string{"unary", "stream"} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				e := start(t, policy())
				e.svc.err = tc.err
				c := testgrpc.NewTestServiceClient(e.conn)
				var err error
				if kind == "unary" {
					_, err = c.UnaryCall(call(t, "bank-bff", "u1"), &testgrpc.SimpleRequest{})
				} else {
					var st testgrpc.TestService_StreamingOutputCallClient
					if st, err = c.StreamingOutputCall(call(t, "user", ""), &testgrpc.StreamingOutputCallRequest{}); err != nil {
						t.Fatal(err)
					}
					_, err = st.Recv()
				}
				<-e.svc.got
				st := status.Convert(err)
				if st.Code() != tc.code || reasonOf(err) != tc.reason {
					t.Fatalf("got %v (reason %q), want %v %q", err, reasonOf(err), tc.code, tc.reason)
				}
				if strings.Contains(st.Message(), "user_secret") {
					t.Fatalf("the handler's text reached the caller: %q", st.Message())
				}
				for _, want := range tc.details {
					if !slices.Contains(detailNames(err), want) {
						t.Fatalf("detail %s lost on the wire: %v", want, detailNames(err))
					}
				}
				if !strings.Contains(e.logs.String(), "user_secret") {
					t.Fatalf("the cause must be in the log: %s", e.logs.String())
				}
			})
		}
	}
}

func TestIntegrationLogsMaskSecretsAndMetricsHaveNoUser(t *testing.T) {
	e := start(t, policy())
	ctx := call(t, "bank-bff", "u1", "idempotency-key", "k-123")
	if _, err := testgrpc.NewTestServiceClient(e.conn).EmptyCall(ctx, &testgrpc.Empty{}); err != nil {
		t.Fatal(err)
	}
	<-e.svc.got
	logs := e.logs.String()
	for _, secret := range []string{"dev:u1", "dev:bank-bff", "k-123"} {
		if strings.Contains(logs, secret) {
			t.Fatalf("log leaks %q: %s", secret, logs)
		}
	}
	mfs, _ := e.reg.Gather()
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetValue() == "u1" {
					t.Fatalf("metric %s has the user in label %s", mf.GetName(), l.GetName())
				}
			}
		}
	}
}

func TestIntegrationHealthIsOpenAndFollowsReadiness(t *testing.T) {
	e := start(t, policy())
	h := healthpb.NewHealthClient(e.conn)
	resp, err := h.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("before readiness: %v %v", resp, err)
	}
	e.srv.SetServing(true)
	resp, err = h.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("after readiness: %v %v", resp, err)
	}
}

func TestIntegrationServeRefusesUncoveredMethods(t *testing.T) {
	srv, err := New(Options{}, Deps{Logger: slog.New(slog.DiscardHandler), Metrics: prometheus.NewRegistry(),
		Policy:   Policy{emptyCall: {Callers: []string{"bank-bff"}, Model: ModelUser}},
		Services: DevServiceVerifier{}, Users: DevUserVerifier{}, Tombstones: NoTombstones{}})
	if err != nil {
		t.Fatal(err)
	}
	testgrpc.RegisterTestServiceServer(srv.GRPC(), &testService{})
	err = srv.Serve(bufconn.Listen(1024))
	if err == nil || !strings.Contains(err.Error(), "/grpc.testing.TestService/UnaryCall") {
		t.Fatalf("error %v, want uncovered methods listed", err)
	}
}

func TestUnitPolicyValidation(t *testing.T) {
	for name, p := range map[string]Policy{
		"bad key":    {"grpc.testing.TestService/EmptyCall": {Callers: []string{"a"}, Model: ModelUser}},
		"no callers": {emptyCall: {Model: ModelUser}},
		"no model":   {emptyCall: {Callers: []string{"a"}}},
	} {
		if err := p.validate(grpc.NewServer()); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestUnitNewRequiresDeps(t *testing.T) {
	if _, err := New(Options{}, Deps{}); err == nil {
		t.Fatal("missing deps must be an error, not a server that lets everyone in")
	}
}

// validationMessage builds a message type with protovalidate rules at runtime, so the
// test does not need generated code: name has min_len 1, phone a pattern, id is required.
func validationMessage(t *testing.T) *dynamicpb.Message {
	t.Helper()
	opts := func(r *validate.FieldRules) *descriptorpb.FieldOptions {
		o := &descriptorpb.FieldOptions{}
		proto.SetExtension(o, validate.E_Field, r)
		return o
	}
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	fdp := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("test/v.proto"),
		Package:    proto.String("test"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"buf/validate/validate.proto"},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Req"),
			Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("name"), Number: proto.Int32(1), Type: str, Label: opt, JsonName: proto.String("name"),
					Options: opts(validate.FieldRules_builder{String: validate.StringRules_builder{MinLen: proto.Uint64(1)}.Build()}.Build())},
				{Name: proto.String("phone"), Number: proto.Int32(2), Type: str, Label: opt, JsonName: proto.String("phone"),
					Options: opts(validate.FieldRules_builder{String: validate.StringRules_builder{Pattern: proto.String(`^\+7\d{10}$`)}.Build()}.Build())},
				{Name: proto.String("id"), Number: proto.Int32(3), Type: str, Label: opt, JsonName: proto.String("id"),
					Options: opts(validate.FieldRules_builder{Required: proto.Bool(true)}.Build())},
			},
		}},
	}
	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	m := dynamicpb.NewMessage(fd.Messages().ByName("Req"))
	m.Set(fd.Messages().ByName("Req").Fields().ByName("phone"), protoreflect.ValueOfString("8900"))
	return m
}

func TestUnitValidationBecomesBadRequest(t *testing.T) {
	e := unitServer(t)
	err := e.srv.validate(context.Background(), validationMessage(t))
	if reasonOf(err) != "VALIDATION_ERROR" {
		t.Fatalf("got %v", err)
	}
	got := map[string]string{}
	for _, d := range status.Convert(err).Details() {
		if br, ok := d.(*errdetails.BadRequest); ok {
			for _, v := range br.GetFieldViolations() {
				got[v.GetField()] = v.GetReason()
			}
		}
	}
	want := map[string]string{"name": "OUT_OF_RANGE", "phone": "INVALID_FORMAT", "id": "REQUIRED"}
	for f, code := range want {
		if got[f] != code {
			t.Errorf("%s: %q, want %q (all: %v)", f, got[f], code, got)
		}
	}
	if _, ok := finerr.ToREST(err); !ok {
		t.Fatal("bank-bff must be able to translate the validation error")
	}
}

// brokenRuleMessage builds a message whose field carries a CEL rule that does not compile:
// protovalidate then fails with a compilation error, not a validation one.
func brokenRuleMessage(t *testing.T) *dynamicpb.Message {
	t.Helper()
	rule := validate.Rule_builder{Id: proto.String("broken"), Expression: proto.String("this.nope.nope(")}.Build()
	o := &descriptorpb.FieldOptions{}
	proto.SetExtension(o, validate.E_Field, validate.FieldRules_builder{Cel: []*validate.Rule{rule}}.Build())
	fdp := &descriptorpb.FileDescriptorProto{
		Name:       proto.String("test/broken.proto"),
		Package:    proto.String("test"),
		Syntax:     proto.String("proto3"),
		Dependency: []string{"buf/validate/validate.proto"},
		MessageType: []*descriptorpb.DescriptorProto{{
			Name: proto.String("Broken"),
			Field: []*descriptorpb.FieldDescriptorProto{{
				Name: proto.String("name"), Number: proto.Int32(1), JsonName: proto.String("name"),
				Type:    descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum(),
				Label:   descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum(),
				Options: o,
			}},
		}},
	}
	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	return dynamicpb.NewMessage(fd.Messages().ByName("Broken"))
}

// A rule that protovalidate cannot evaluate is our defect: the caller gets INTERNAL_ERROR,
// not a validation error that blames its request, and the defect is logged with the
// request ID of the call.
func TestUnitBrokenValidationRuleIsInternalAndLogged(t *testing.T) {
	e := unitServer(t)
	ctx := reqctx.WithRequestID(context.Background(), "req-broken")
	err := e.srv.validate(ctx, brokenRuleMessage(t))
	if status.Code(err) != codes.Internal || reasonOf(err) != "INTERNAL_ERROR" {
		t.Fatalf("got %v, want INTERNAL_ERROR", err)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "protovalidate failed") || !strings.Contains(logs, "req-broken") {
		t.Fatalf("the defect must be logged with the request ID: %s", logs)
	}
}

func TestUnitMaskMessage(t *testing.T) {
	req := &userv1.WatchPhoneVerificationRequest{VerificationToken: "vt-secret"}
	got, ok := maskMessage(req).(*userv1.WatchPhoneVerificationRequest)
	if !ok || got.GetVerificationToken() != masked {
		t.Fatalf("masked copy: %v", got)
	}
	if req.GetVerificationToken() != "vt-secret" {
		t.Fatal("the request itself must not change")
	}
	if maskMessage(nil) != nil {
		t.Fatal("nil")
	}
}

func TestUnitDeletedAdmissionRequiresExactUserRule(t *testing.T) {
	for _, r := range []Rule{{Callers: []string{"bank-bff"}, Model: ModelNoUser, AllowDeleted: true}, {Callers: []string{"bank-bff"}, Model: ModelMixed, AllowDeleted: true}} {
		if err := (Policy{"/test.Profile/Delete": r}).validate(grpc.NewServer()); err == nil {
			t.Fatal("non-user deleted admission accepted")
		}
	}
	r := Rule{Callers: []string{"bank-bff"}, Model: ModelUser, AllowDeleted: true}
	if err := (Policy{"/test.Profile/*": r}).validate(grpc.NewServer()); err == nil {
		t.Fatal("wildcard deleted admission accepted")
	}
	if err := (Policy{"/test.Profile/Delete": r}).validate(grpc.NewServer()); err != nil {
		t.Fatal(err)
	}
}
