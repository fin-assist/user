package server

import (
	"strings"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

const masked = "***"

// secretKeys are metadata keys that are never logged (grpc-api.md 4, «Metadata»).
var secretKeys = map[string]bool{
	"authorization":           true,
	"x-service-authorization": true,
	"idempotency-key":         true,
}

// maskMetadata returns md for a log line: secret keys and keys with "token" in the name
// masked, binary (-bin) values dropped.
func maskMetadata(md metadata.MD) map[string]string {
	out := make(map[string]string, len(md))
	for k, vs := range md {
		switch {
		case secretKeys[k] || strings.Contains(k, "token"):
			out[k] = masked
		case strings.HasSuffix(k, "-bin"):
			out[k] = "(binary)"
		default:
			out[k] = strings.Join(vs, ",")
		}
	}
	return out
}

// maskMessage returns a copy of m for a log line: every string field whose name contains
// "token" is replaced (verification_token, registration_token, refresh_token) and every
// bytes field is cleared whatever its name (a statement file, a key), at any depth. Masking
// of other personal data is after MVP (grpc-api.md 4).
func maskMessage(m proto.Message) proto.Message {
	if m == nil {
		return nil
	}
	c := proto.Clone(m)
	maskFields(c.ProtoReflect())
	return c
}

func maskFields(m protoreflect.Message) {
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		secret := strings.Contains(string(fd.Name()), "token")
		switch {
		case fd.IsList():
			l := v.List()
			for i := range l.Len() {
				switch {
				case fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind:
					maskFields(l.Get(i).Message())
				case secret && fd.Kind() == protoreflect.StringKind:
					l.Set(i, protoreflect.ValueOfString(masked))
				case fd.Kind() == protoreflect.BytesKind:
					l.Set(i, protoreflect.ValueOfBytes(nil))
				}
			}
		case fd.IsMap():
			mp := v.Map()
			mp.Range(func(k protoreflect.MapKey, mv protoreflect.Value) bool {
				vd := fd.MapValue()
				switch {
				case vd.Kind() == protoreflect.MessageKind:
					maskFields(mv.Message())
				case secret && vd.Kind() == protoreflect.StringKind:
					mp.Set(k, protoreflect.ValueOfString(masked))
				case vd.Kind() == protoreflect.BytesKind:
					mp.Set(k, protoreflect.ValueOfBytes(nil))
				}
				return true
			})
		case fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind:
			maskFields(v.Message())
		case secret && fd.Kind() == protoreflect.StringKind:
			m.Set(fd, protoreflect.ValueOfString(masked))
		case fd.Kind() == protoreflect.BytesKind:
			m.Set(fd, protoreflect.ValueOfBytes(nil))
		}
		return true
	})
}
