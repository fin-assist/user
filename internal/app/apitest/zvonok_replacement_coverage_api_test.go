package apitest_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fin-assist/user/internal/app"
	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/verification"
)

func TestAPIZvonokRecoversNewestOwnedReplacementWithoutExtendingItsLifetime(t *testing.T) {
	for _, newestFirst := range []bool{false, true} {
		name := "oldest-first"
		if newestFirst {
			name = "newest-first"
		}
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var firstCreated time.Time
			confirms, reads := 0, 0
			recovery := false
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				values := r.URL.Query()
				if r.Method == http.MethodPost {
					if r.ParseMultipartForm(8192) != nil || r.URL.RawQuery != "" {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					values = r.PostForm
				}
				if values.Get("public_key") != "replacement-test-key" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				if strings.HasSuffix(r.URL.Path, "calls_by_phone/") {
					reads++
					if confirms == 0 {
						_, _ = io.WriteString(w, "[]")
						return
					}
					if !recovery {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					old := map[string]any{"call_id": 41, "phone": values.Get("phone"), "created": firstCreated, "call_status": "expires"}
					fresh := map[string]any{"call_id": 42, "phone": values.Get("phone"), "created": firstCreated.Add(time.Millisecond), "status": "in_process"}
					calls := []any{old, fresh}
					if newestFirst {
						calls = []any{fresh, old}
					}
					// A terminal call older than this durable attempt is never adopted.
					calls = append(calls, map[string]any{"call_id": 40, "phone": values.Get("phone"), "created": firstCreated.Add(-time.Minute), "call_status": "expires"})
					_ = json.NewEncoder(w).Encode(calls)
					return
				}
				if !strings.HasSuffix(r.URL.Path, "confirm/") {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				confirms++
				if confirms == 1 {
					firstCreated = time.Now().UTC()
					_, _ = io.WriteString(w, "lost original response")
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "data": map[string]any{
					"call_id": 42, "phone": values.Get("phone"), "created": firstCreated.Add(time.Millisecond),
					"call_status": "in_process", "allowed_phones_for_call": []string{"+74951234567"},
				}})
			}))
			defer provider.Close()
			target, err := url.Parse(provider.URL)
			if err != nil {
				t.Fatal(err)
			}
			env := apitest.StartWithOptions(t, []app.Option{app.WithZvonokTransport(zvonokHTTPTransport{target, provider.Client().Transport})}, func(c *config.Config) {
				c.Zvonok = config.ZvonokConfig{Provider: "zvonok", PublicKey: "replacement-test-key", CampaignID: "123", CallbackAddr: "127.0.0.1:0", PollInterval: time.Second}
			})
			key, phone := authKey(), "+79991234567"
			if out, err := startCall(t, env, key, phone); err == nil || out != nil || startHits(t, env) != 1 {
				t.Fatalf("unknown write did not retain exactly one reservation: %v %v", out, err)
			}
			mu.Lock()
			recovery = true
			wantCreated := firstCreated.Add(time.Millisecond).Truncate(time.Microsecond)
			mu.Unlock()
			out, err := startCall(t, env, key, phone)
			if err != nil || out.GetVerificationToken() == "" || out.GetCallbackNumber() != "+74951234567" {
				t.Fatalf("owned replacement did not recover: %v %v", out, err)
			}
			var callID string
			var created, expires time.Time
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT provider_call_id,created_at,expires_at FROM phone_verification WHERE phone=$1`, phone).Scan(&callID, &created, &expires); err != nil || callID != "42" || !created.Equal(wantCreated) || !expires.Equal(created.Add(verification.RequestTTL)) || !expires.Equal(out.GetExpiresAt().AsTime().Truncate(time.Microsecond)) {
				t.Fatalf("predecessor identity or extended lifetime persisted: id=%s created=%s expires=%s err=%v", callID, created, expires, err)
			}
			mu.Lock()
			beforeReads, beforeConfirms := reads, confirms
			mu.Unlock()
			replay, err := startCall(t, env, key, phone)
			if err != nil || replay.GetVerificationToken() != out.GetVerificationToken() || startHits(t, env) != 1 {
				t.Fatalf("replay repeated reservation or changed proof: %v %v", replay, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if confirms != 2 || confirms != beforeConfirms || reads != beforeReads {
				t.Fatalf("replay performed provider I/O: confirms=%d reads=%d", confirms, reads)
			}
		})
	}
}
