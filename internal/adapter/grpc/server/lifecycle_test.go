package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// blocking holds a stream open until the test ends, as an answer stream would.
type blocking struct {
	testgrpc.UnimplementedTestServiceServer
	started chan struct{}
}

func (b *blocking) StreamingOutputCall(_ *testgrpc.StreamingOutputCallRequest, ss testgrpc.TestService_StreamingOutputCallServer) error {
	close(b.started)
	<-ss.Context().Done()
	return ss.Context().Err()
}

func TestIntegrationShutdownReportsNotServing(t *testing.T) {
	e := start(t, policy())
	e.srv.SetServing(true)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	e.srv.Shutdown(ctx, 0)
	e.srv.SetServing(true) // ignored after Shutdown: probes keep seeing NOT_SERVING
	resp, err := e.srv.health.Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("after Shutdown: %v %v", resp, err)
	}
}

func TestIntegrationOneServerPerRegistry(t *testing.T) {
	e := start(t, policy())
	if _, err := New(Options{}, e.srv.deps); err == nil {
		t.Fatal("a second server on the same registry must fail instead of sharing its metrics silently")
	}
}

func TestIntegrationShutdownWaitsForStreamUntilDeadline(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	srv, err := New(Options{Reflection: true}, testDeps(t))
	if err != nil {
		t.Fatal(err)
	}
	svc := &blocking{started: make(chan struct{})}
	testgrpc.RegisterTestServiceServer(srv.GRPC(), svc)
	go func() { _ = srv.Serve(lis) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx := metadata.AppendToOutgoingContext(t.Context(), "x-service-authorization", "Bearer dev:user")
	st, err := testgrpc.NewTestServiceClient(conn).StreamingOutputCall(ctx, &testgrpc.StreamingOutputCallRequest{})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = st.Recv() }()
	<-svc.started

	sctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	begun := time.Now()
	srv.Shutdown(sctx, time.Hour) // the delay is cut by the context too
	if d := time.Since(begun); d > 5*time.Second {
		t.Fatalf("Shutdown took %v: the open stream must be cut at the deadline", d)
	}
}

func testDeps(t *testing.T) Deps {
	t.Helper()
	return Deps{
		Logger:     slog.New(slog.DiscardHandler),
		Metrics:    prometheus.NewRegistry(),
		Policy:     Policy{"/grpc.testing.TestService/*": {Callers: []string{"user"}, Model: ModelSystem}},
		Services:   DevServiceVerifier{},
		Users:      DevUserVerifier{},
		Tombstones: NoTombstones{},
	}
}

func TestUnitMaskMetadata(t *testing.T) {
	got := maskMetadata(metadata.MD{
		"authorization":        {"Bearer a"},
		"x-verification-token": {"v"},
		"trace-bin":            {"\x00\x01"},
		"x-request-id":         {"a", "b"},
	})
	want := map[string]string{"authorization": masked, "x-verification-token": masked, "trace-bin": "(binary)", "x-request-id": "a,b"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %q, want %q", k, got[k], v)
		}
	}
}

// tokenMessage builds a message with token fields in every shape maskFields handles:
// a repeated string, a bytes field, a list of messages, maps with string, bytes and message
// values — and bytes fields without "token" in the name (a file), which are cleared too.
func tokenMessage(t *testing.T) (*dynamicpb.Message, protoreflect.MessageDescriptor) {
	t.Helper()
	str := descriptorpb.FieldDescriptorProto_TYPE_STRING.Enum()
	byt := descriptorpb.FieldDescriptorProto_TYPE_BYTES.Enum()
	msg := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	f := func(name string, n int32, typ *descriptorpb.FieldDescriptorProto_Type, label *descriptorpb.FieldDescriptorProto_Label, typeName string) *descriptorpb.FieldDescriptorProto {
		fd := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(n), Type: typ, Label: label, JsonName: proto.String(name)}
		if typeName != "" {
			fd.TypeName = proto.String(typeName)
		}
		return fd
	}
	entry := func(name, valueType string, typ *descriptorpb.FieldDescriptorProto_Type) *descriptorpb.DescriptorProto {
		return &descriptorpb.DescriptorProto{
			Name:    proto.String(name),
			Options: &descriptorpb.MessageOptions{MapEntry: proto.Bool(true)},
			Field:   []*descriptorpb.FieldDescriptorProto{f("key", 1, str, opt, ""), f("value", 2, typ, opt, valueType)},
		}
	}
	fdp := &descriptorpb.FileDescriptorProto{
		Name: proto.String("test/mask.proto"), Package: proto.String("masktest"), Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("Inner"), Field: []*descriptorpb.FieldDescriptorProto{
				f("refresh_token", 1, str, opt, ""), f("raw_token", 2, byt, opt, ""), f("name", 3, str, opt, ""),
				f("file", 4, byt, opt, ""),
			}},
			{Name: proto.String("Outer"),
				Field: []*descriptorpb.FieldDescriptorProto{
					f("tokens", 1, str, rep, ""),
					f("items", 2, msg, rep, ".masktest.Inner"),
					f("token_map", 3, msg, rep, ".masktest.Outer.TokenMapEntry"),
					f("inner_map", 4, msg, rep, ".masktest.Outer.InnerMapEntry"),
					f("one", 5, msg, opt, ".masktest.Inner"),
					f("blob_tokens", 6, byt, rep, ""),
					f("pages", 7, byt, rep, ""),
					f("file_map", 8, msg, rep, ".masktest.Outer.FileMapEntry"),
				},
				NestedType: []*descriptorpb.DescriptorProto{
					entry("TokenMapEntry", "", str),
					entry("InnerMapEntry", ".masktest.Inner", msg),
					entry("FileMapEntry", "", byt),
				}},
		},
	}
	file, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	if err != nil {
		t.Fatal(err)
	}
	inner, outer := file.Messages().ByName("Inner"), file.Messages().ByName("Outer")
	newInner := func() protoreflect.Message {
		m := dynamicpb.NewMessage(inner)
		m.Set(inner.Fields().ByName("refresh_token"), protoreflect.ValueOfString("secret"))
		m.Set(inner.Fields().ByName("raw_token"), protoreflect.ValueOfBytes([]byte("secret")))
		m.Set(inner.Fields().ByName("name"), protoreflect.ValueOfString("visible"))
		m.Set(inner.Fields().ByName("file"), protoreflect.ValueOfBytes([]byte("secret")))
		return m
	}
	m := dynamicpb.NewMessage(outer)
	of := outer.Fields()
	m.Mutable(of.ByName("tokens")).List().Append(protoreflect.ValueOfString("secret"))
	m.Mutable(of.ByName("blob_tokens")).List().Append(protoreflect.ValueOfBytes([]byte("secret")))
	m.Mutable(of.ByName("pages")).List().Append(protoreflect.ValueOfBytes([]byte("secret")))
	m.Mutable(of.ByName("file_map")).Map().Set(protoreflect.ValueOfString("k").MapKey(), protoreflect.ValueOfBytes([]byte("secret")))
	m.Mutable(of.ByName("items")).List().Append(protoreflect.ValueOfMessage(newInner()))
	m.Mutable(of.ByName("token_map")).Map().Set(protoreflect.ValueOfString("k").MapKey(), protoreflect.ValueOfString("secret"))
	m.Mutable(of.ByName("inner_map")).Map().Set(protoreflect.ValueOfString("k").MapKey(), protoreflect.ValueOfMessage(newInner()))
	m.Set(of.ByName("one"), protoreflect.ValueOfMessage(newInner()))
	return m, outer
}

func TestUnitMaskMessageAtAnyDepth(t *testing.T) {
	m, _ := tokenMessage(t)
	masked := maskMessage(m)
	if containsSecret(masked) {
		t.Fatalf("a token survived masking: %v", masked)
	}
	if !strings.Contains(fmt.Sprint(masked), "visible") {
		t.Fatalf("fields without token in the name must stay: %v", masked)
	}
	if !containsSecret(m) {
		t.Fatal("the original must not change")
	}
}

// containsSecret walks m and reports any string or bytes value equal to "secret".
func containsSecret(m proto.Message) bool {
	found := false
	var walk func(protoreflect.Message)
	value := func(v protoreflect.Value) {
		switch x := v.Interface().(type) {
		case string:
			found = found || x == "secret"
		case []byte:
			found = found || string(x) == "secret"
		case protoreflect.Message:
			walk(x)
		}
	}
	walk = func(m protoreflect.Message) {
		m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			switch {
			case fd.IsList():
				for i := range v.List().Len() {
					value(v.List().Get(i))
				}
			case fd.IsMap():
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool { value(mv); return true })
			default:
				value(v)
			}
			return true
		})
	}
	walk(m.ProtoReflect())
	return found
}
