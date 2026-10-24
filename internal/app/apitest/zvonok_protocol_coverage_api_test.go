package apitest_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fin-assist/user/internal/app"
	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/config"
)

func TestAPIZvonokMalformedProviderRepliesNeverPublishSecretsAndRecoverOnce(t *testing.T) {
	for _, mode := range []string{"history-null", "history-object", "history-invalid-call", "history-wrong-phone", "history-http-error", "confirm-invalid-json", "confirm-http-error", "confirm-rejected", "confirm-future", "confirm-wrong-phone", "confirm-invalid-id", "confirm-empty-pool", "confirm-invalid-number", "confirm-duplicate-pool", "confirm-too-many-numbers", "confirm-oversized-body", "ambiguous-recovery"} {
		t.Run(mode, func(t *testing.T) {
			var healthy atomic.Bool
			var confirms atomic.Int32
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				values := r.URL.Query()
				if r.Method == http.MethodPost {
					if r.ParseMultipartForm(8192) != nil || r.URL.RawQuery != "" {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					values = r.PostForm
				}
				if values.Get("public_key") != "local-provider-secret" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				call := map[string]any{"call_id": 41, "phone": values.Get("phone"), "created": time.Now().UTC(), "call_status": "in_process", "allowed_phones_for_call": []string{"+74951234567"}}
				bad := !healthy.Load()
				if strings.HasSuffix(r.URL.Path, "calls_by_phone/") {
					if bad {
						switch mode {
						case "history-null":
							_, _ = io.WriteString(w, "null")
							return
						case "history-object":
							_, _ = io.WriteString(w, `{"private":"upstream data"}`)
							return
						case "history-invalid-call":
							call["call_id"] = "not-a-call"
							_ = json.NewEncoder(w).Encode([]any{call})
							return
						case "history-wrong-phone":
							call["phone"] = "+79997654321"
							_ = json.NewEncoder(w).Encode([]any{call})
							return
						case "history-http-error":
							w.WriteHeader(http.StatusInternalServerError)
							_, _ = io.WriteString(w, "private upstream data")
							return
						case "ambiguous-recovery":
							if confirms.Load() > 0 {
								other := map[string]any{"call_id": 42, "phone": values.Get("phone"), "created": time.Now().UTC(), "call_status": "in_process"}
								_ = json.NewEncoder(w).Encode([]any{call, other})
								return
							}
						}
					}
					_, _ = io.WriteString(w, "[]")
					return
				}
				if !strings.HasSuffix(r.URL.Path, "confirm/") {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				confirms.Add(1)
				if bad {
					switch mode {
					case "confirm-invalid-json", "ambiguous-recovery":
						_, _ = io.WriteString(w, "private invalid JSON")
						return
					case "confirm-http-error":
						w.WriteHeader(http.StatusInternalServerError)
					case "confirm-rejected":
						w.WriteHeader(http.StatusBadRequest)
						_, _ = io.WriteString(w, `{"status":"error","data":"private refusal"}`)
						return
					case "confirm-future":
						call["created"] = time.Now().Add(time.Hour).UTC()
					case "confirm-wrong-phone":
						call["phone"] = "+79997654321"
					case "confirm-invalid-id":
						call["call_id"] = "invalid"
					case "confirm-empty-pool":
						call["allowed_phones_for_call"] = []string{}
					case "confirm-invalid-number":
						call["allowed_phones_for_call"] = []string{"private invalid callback"}
					case "confirm-duplicate-pool":
						call["allowed_phones_for_call"] = []string{"+74951234567", "+74951234567"}
					case "confirm-too-many-numbers":
						call["allowed_phones_for_call"] = make([]string, 1001)
					case "confirm-oversized-body":
						_, _ = io.WriteString(w, strings.Repeat("x", (1<<20)+1))
						return
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "data": call})
			}))
			defer provider.Close()
			target, err := url.Parse(provider.URL)
			if err != nil {
				t.Fatal(err)
			}
			env := apitest.StartWithOptions(t, []app.Option{app.WithZvonokTransport(zvonokHTTPTransport{target, provider.Client().Transport})}, func(c *config.Config) {
				c.Zvonok = config.ZvonokConfig{Provider: "zvonok", PublicKey: "local-provider-secret", CampaignID: "123", CallbackAddr: "127.0.0.1:0", PollInterval: time.Second}
			})
			key, phone := authKey(), "+79991234567"
			response, err := startCall(t, env, key, phone)
			if err == nil || response != nil || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "local-provider-secret") || strings.Contains(err.Error(), phone) {
				t.Fatalf("untrusted provider reply reached a public response: %v %v", response, err)
			}
			var stored int
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM phone_verification WHERE provider_call_id IS NOT NULL OR callback_number IS NOT NULL OR allowed_phones_for_call IS NOT NULL OR verified_at IS NOT NULL`).Scan(&stored); err != nil || stored != 0 {
				t.Fatalf("invalid provider reply produced local confirmation effects: %d %v", stored, err)
			}
			if (strings.HasPrefix(mode, "history-") && mode != "history-wrong-phone" || mode == "confirm-rejected") && startHits(t, env) != 0 {
				t.Fatal("definitively refused creation retained a rate-limit reservation")
			}
			healthy.Store(true)
			response, err = startCall(t, env, key, phone)
			if err != nil || response.GetVerificationToken() == "" || response.GetCallbackNumber() != "+74951234567" || startHits(t, env) != 1 {
				t.Fatalf("corrected provider did not recover exactly one reservation: %v %v", response, err)
			}
			calls := confirms.Load()
			replay, err := startCall(t, env, key, phone)
			if err != nil || replay.GetVerificationToken() != response.GetVerificationToken() || confirms.Load() != calls || startHits(t, env) != 1 {
				t.Fatalf("replay repeated provider effects or changed secrets: %v %v", replay, err)
			}
		})
	}
}
