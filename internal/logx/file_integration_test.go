package logx

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fin-assist/go-grpc/reqctx"
)

func TestIntegrationFileSinkPreservesSharedRequestContext(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "service.log")
			sink, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sink.Close() })
			logger := New(sink, format, slog.LevelWarn, "user").With("component", "authentication").WithGroup("request")
			ctx := reqctx.WithUserID(reqctx.WithCaller(reqctx.WithRequestID(context.Background(), "request-1"), "bank-bff"), "user-1")
			logger.InfoContext(ctx, "filtered")
			logger.WarnContext(ctx, "accepted", "outcome", "available")
			logger.ErrorContext(context.Background(), "background", "outcome", "unavailable")
			if err := sink.Sync(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) != 2 || strings.Contains(string(data), "filtered") {
				t.Fatal("level filter or file record boundaries changed")
			}
			if format == "json" {
				for i, line := range lines {
					var record map[string]any
					if err := json.Unmarshal([]byte(line), &record); err != nil {
						t.Fatal(err)
					}
					if record["service"] != "user" || record["component"] != "authentication" {
						t.Fatalf("static context lost: %v", record)
					}
					group, ok := record["request"].(map[string]any)
					if !ok {
						t.Fatal("group lost")
					}
					if i == 0 {
						if group["request_id"] != "request-1" || group["caller"] != "bank-bff" || group["user_id"] != "user-1" || group["outcome"] != "available" || record["level"] != "WARN" {
							t.Fatalf("request context lost: %v", record)
						}
					} else {
						if _, ok := group["user_id"]; ok {
							t.Fatal("identity leaked to background record")
						}
						if record["level"] != "ERROR" {
							t.Fatal("error level lost")
						}
					}
				}
			} else {
				for _, want := range []string{"service=user", "component=authentication", "request.request_id=request-1", "request.caller=bank-bff", "request.user_id=user-1", "level=WARN", "level=ERROR"} {
					if !strings.Contains(string(data), want) {
						t.Fatalf("text record missing %s", want)
					}
				}
				if strings.Contains(lines[1], "user-1") {
					t.Fatal("identity leaked to background record")
				}
			}
		})
	}
}
