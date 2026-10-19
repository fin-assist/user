// Package reqctx carries request-scoped values through context: the request ID, the
// verified caller and user, and the hints bank-bff passes in metadata. The gRPC
// interceptors set them; handlers, repositories, clients and the logger read them.
//
// Only verified or explicitly trusted values are stored here (docs/server.md,
// «Идентичность»): a handler can rely on UserID being the sub of a checked token.
package reqctx

import "context"

type key int

const (
	requestIDKey key = iota
	callerKey
	userIDKey
	clientIPKey
	clientVersionKey
	readPrimaryKey
)

// WithRequestID sets the end-to-end request ID (metadata x-request-id).
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// RequestID returns the request ID, empty when none.
func RequestID(ctx context.Context) string { return get[string](ctx, requestIDKey) }

// WithCaller sets the verified name of the calling service.
func WithCaller(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, callerKey, name)
}

// Caller returns the verified name of the calling service, empty when unknown.
func Caller(ctx context.Context) string { return get[string](ctx, callerKey) }

// WithUserID sets the user of a user call: the sub of the verified access token.
func WithUserID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, userIDKey, id)
}

// UserID returns the user of a user call and true, or "" and false for a system call.
// System calls take user_id from the request message instead (grpc-api.md, 3.2).
func UserID(ctx context.Context) (string, bool) {
	id := get[string](ctx, userIDKey)
	return id, id != ""
}

// WithClientIP sets x-client-ip; the interceptor accepts it only from bank-bff.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, clientIPKey, ip)
}

// ClientIP returns the client address computed by bank-bff, empty when none.
func ClientIP(ctx context.Context) string { return get[string](ctx, clientIPKey) }

// WithClientVersion sets x-client-version. It is copied from a client header and
// spoofable: for logs and metrics only, never for authorization or limits.
func WithClientVersion(ctx context.Context, v string) context.Context {
	return context.WithValue(ctx, clientVersionKey, v)
}

// ClientVersion returns x-client-version, empty when none.
func ClientVersion(ctx context.Context) string { return get[string](ctx, clientVersionKey) }

// WithReadPrimary makes reads of this request go to the primary (x-read-primary: 1,
// read after the client's own write, grpc-api.md 7.3).
func WithReadPrimary(ctx context.Context) context.Context {
	return context.WithValue(ctx, readPrimaryKey, true)
}

// ReadPrimary reports whether reads must go to the primary.
func ReadPrimary(ctx context.Context) bool { return get[bool](ctx, readPrimaryKey) }

func get[T any](ctx context.Context, k key) T {
	v, _ := ctx.Value(k).(T)
	return v
}
