package apitest_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/user/internal/app"
	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/config"
)

// Freeze the real provider response after the durable reservation commits. The
// storage disruption then exercises the second transaction, rather than failing
// the initial lookup. Repair and retry must recover the same external operation.
func TestAPIStartCompletionStorageFailurePreservesOwnedRecoveryAndCharge(t *testing.T) {
	for _, tc := range []struct {
		name, breakSQL, restoreSQL string
		reject                     bool
	}{
		{"complete-call", `ALTER TABLE phone_verification RENAME COLUMN provider_call_id TO unavailable`, `ALTER TABLE phone_verification RENAME COLUMN unavailable TO provider_call_id`, false},
		{"owner-lookup", `ALTER TABLE app_user RENAME COLUMN phone TO unavailable`, `ALTER TABLE app_user RENAME COLUMN unavailable TO phone`, false},
		{"pool-persistence", `ALTER TABLE phone_verification RENAME COLUMN allowed_phones_for_call TO unavailable`, `ALTER TABLE phone_verification RENAME COLUMN unavailable TO allowed_phones_for_call`, false},
		{"owner-guard", `ALTER TABLE tombstone_users RENAME TO unavailable_tombstones`, `ALTER TABLE unavailable_tombstones RENAME TO tombstone_users`, false},
		{"compensation", `ALTER TABLE rate_limit_window RENAME COLUMN hits TO unavailable`, `ALTER TABLE rate_limit_window RENAME COLUMN unavailable TO hits`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			var mu sync.Mutex
			var created time.Time
			confirms := 0
			phone := "+79001234567"
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				values := r.URL.Query()
				if r.Method == http.MethodPost {
					if r.ParseMultipartForm(8192) != nil {
						w.WriteHeader(400)
						return
					}
					values = r.PostForm
				}
				if values.Get("public_key") != "storage-test-secret" {
					w.WriteHeader(403)
					return
				}
				mu.Lock()
				if strings.HasSuffix(r.URL.Path, "calls_by_phone/") || strings.HasSuffix(r.URL.Path, "call_by_id/") {
					calls := []any{}
					if !created.IsZero() {
						calls = append(calls, map[string]any{"call_id": 41, "phone": phone, "created": created, "call_status": "in_process", "allowed_phones_for_call": []string{"+74951234567", "+74957654321"}})
					}
					mu.Unlock()
					_ = json.NewEncoder(w).Encode(calls)
					return
				}
				confirms++
				first := confirms == 1
				if created.IsZero() && (!tc.reject || !first) {
					created = time.Now().UTC()
				}
				stamp := created
				mu.Unlock()
				if first {
					close(entered)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					if tc.reject {
						w.WriteHeader(http.StatusTooManyRequests)
						return
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "data": map[string]any{"call_id": 41, "phone": phone, "created": stamp, "call_status": "in_process", "allowed_phones_for_call": []string{"+74951234567", "+74957654321"}}})
			}))
			defer func() { unblock(); provider.Close() }()
			target, err := url.Parse(provider.URL)
			if err != nil {
				t.Fatal(err)
			}
			env := apitest.StartWithOptions(t, []app.Option{app.WithZvonokTransport(zvonokHTTPTransport{target, provider.Client().Transport})}, func(c *config.Config) {
				c.Zvonok = config.ZvonokConfig{Provider: "zvonok", PublicKey: "storage-test-secret", CampaignID: "123", CallbackAddr: "127.0.0.1:0", PollInterval: time.Second}
			})
			seedProfiles(t, env)
			seedDocuments(t, env)
			key := authKey()
			type result struct {
				out *userv1.StartPhoneVerificationResponse
				err error
			}
			done := make(chan result, 1)
			go func() { out, err := startCall(t, env, key, phone); done <- result{out, err} }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("provider was not reached after reservation")
			}
			if _, err := env.App.DB.Primary().Exec(t.Context(), tc.breakSQL); err != nil {
				t.Fatal(err)
			}
			unblock()
			var first result
			select {
			case first = <-done:
			case <-time.After(12 * time.Second):
				t.Fatal("completion did not return")
			}
			if first.out != nil || status.Code(first.err) != codes.Internal || apitest.Reason(first.err) != "" {
				t.Fatal(first.out, first.err)
			}
			if _, err := env.App.DB.Primary().Exec(t.Context(), tc.restoreSQL); err != nil {
				t.Fatal(err)
			}
			var complete bool
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT provider_call_id IS NOT NULL OR callback_number IS NOT NULL OR verified_at IS NOT NULL FROM phone_verification WHERE start_key=$1`, key).Scan(&complete); err != nil || complete {
				t.Fatal("partial completion leaked", complete, err)
			}
			if hits := startHits(t, env); hits != 1 {
				t.Fatal("failed completion changed committed charge", hits)
			}
			if tc.reject {
				if out, e := startCall(t, env, key, phone); e == nil || out != nil {
					t.Fatal("rejected unknown cannot reconfirm")
				}
				mu.Lock()
				writes := confirms
				mu.Unlock()
				if writes != 1 {
					t.Fatal("compensation failure repeated confirm", writes)
				}
				return
			}
			recovered, err := startCall(t, env, key, phone)
			if err != nil || recovered.GetVerificationToken() == "" || recovered.GetCallbackNumber() == "" || startHits(t, env) != 1 {
				t.Fatal(recovered, err)
			}
			mu.Lock()
			before := confirms
			mu.Unlock()
			replay, err := startCall(t, env, key, phone)
			mu.Lock()
			after := confirms
			mu.Unlock()
			if err != nil || replay.GetVerificationToken() != recovered.GetVerificationToken() || before != after {
				t.Fatal("replay repeated provider or changed secret", replay, err, before, after)
			}
		})
	}
}
