package logx

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/fin-assist/go-grpc/reqctx"
)

func TestUnitContextValuesInEveryLine(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, "json", slog.LevelInfo, "user").With("component", "test").WithGroup("g")
	ctx := reqctx.WithUserID(reqctx.WithCaller(reqctx.WithRequestID(context.Background(), "req-1"), "bank-bff"), "u1")
	log.InfoContext(ctx, "hello", "k", "v")

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("%v: %s", err, buf.String())
	}
	if line["service"] != "user" || line["component"] != "test" {
		t.Fatalf("static attrs: %v", line)
	}
	// Attributes added by Handle land in the open group, as slog does for record attrs.
	g, _ := line["g"].(map[string]any)
	if g["request_id"] != "req-1" || g["caller"] != "bank-bff" || g["user_id"] != "u1" || g["k"] != "v" {
		t.Fatalf("context attrs: %v", line)
	}
}

func TestUnitTextFormatAndLevel(t *testing.T) {
	var buf bytes.Buffer
	log := New(&buf, "text", slog.LevelWarn, "user")
	log.InfoContext(context.Background(), "dropped")
	log.WarnContext(context.Background(), "kept")
	out := buf.String()
	if strings.Contains(out, "dropped") || !strings.Contains(out, "msg=kept") || strings.Contains(out, "request_id") {
		t.Fatalf("got %q", out)
	}
}
