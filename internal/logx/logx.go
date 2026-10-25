// Package logx builds the service logger: slog with JSON (or text in dev) and the request
// ID, caller and user taken from the context of every *Context call.
package logx

import (
	"context"
	"io"
	"log/slog"

	"github.com/fin-assist/go-grpc/reqctx"
)

// New returns a logger writing to w. service is added to every line.
func New(w io.Writer, format string, level slog.Level, service string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if format == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(contextHandler{h}).With("service", service)
}

// contextHandler adds request-scoped values from the context. Use slog.InfoContext (and
// the other *Context methods) so they appear.
type contextHandler struct{ slog.Handler }

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := reqctx.RequestID(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	if c := reqctx.Caller(ctx); c != "" {
		r.AddAttrs(slog.String("caller", c))
	}
	if u, ok := reqctx.UserID(ctx); ok {
		// user_id is allowed in logs, never in metric labels (grpc-api.md 9).
		r.AddAttrs(slog.String("user_id", u))
	}
	return h.Handler.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(as)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}
