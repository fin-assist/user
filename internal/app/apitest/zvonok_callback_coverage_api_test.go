package apitest_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAPIZvonokCallbackRejectsUntrustedRoutingBeforeProviderOrSQLChanges(t *testing.T) {
	env, provider := zvonokEnv(t, "")
	phone := "+79991234567"
	started, err := startCall(t, env, authKey(), phone)
	if err != nil || started.GetVerificationToken() == "" {
		t.Fatal(started, err)
	}
	call := mockCall(provider, phone, "")
	query := "ct_call_id=" + strconv.FormatInt(call.ID, 10)
	server := httptest.NewServer(env.App.PhoneCallback)
	defer server.Close()
	send := func(method, path, query, forwarded string) int {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		request, err := http.NewRequestWithContext(ctx, method, server.URL+path+"?"+query, nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("X-Forwarded-For", forwarded)
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		if err != nil || len(body) != 0 || response.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("callback exposed body or cacheable state: %q %v", body, err)
		}
		return response.StatusCode
	}
	var before string
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT to_jsonb(v)::text FROM phone_verification v WHERE phone=$1`, phone).Scan(&before); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	lookups := provider.lookups
	provider.mu.Unlock()
	for _, tc := range []struct {
		name, method, path, query, forwarded string
		status                               int
	}{
		{"wrong-method", http.MethodPost, "/callbacks/zvonok", query, "193.232.64.150", http.StatusNotFound},
		{"wrong-route", http.MethodGet, "/callbacks/another", query, "193.232.64.150", http.StatusNotFound},
		{"oversized-query", http.MethodGet, "/callbacks/zvonok", query + "&ignored=" + strings.Repeat("x", 8193), "193.232.64.150", http.StatusBadRequest},
		{"missing-id", http.MethodGet, "/callbacks/zvonok", "ct_phone=forged&ct_status=pincode_ok", "193.232.64.150", http.StatusBadRequest},
		{"zero-id", http.MethodGet, "/callbacks/zvonok", "ct_call_id=0", "193.232.64.150", http.StatusBadRequest},
		{"duplicate-id", http.MethodGet, "/callbacks/zvonok", query + "&ct_call_id=999", "193.232.64.150", http.StatusBadRequest},
		{"invalid-forwarded-address", http.MethodGet, "/callbacks/zvonok", query, "not-an-address", http.StatusForbidden},
		{"too-many-proxies", http.MethodGet, "/callbacks/zvonok", query, strings.Repeat("127.0.0.1,", 16) + "193.232.64.150", http.StatusForbidden},
		{"only-trusted-proxies", http.MethodGet, "/callbacks/zvonok", query, "127.0.0.1,127.0.0.2", http.StatusForbidden},
		{"forged-leftmost-provider", http.MethodGet, "/callbacks/zvonok", query, "193.232.64.150,203.0.113.99", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := send(tc.method, tc.path, tc.query, tc.forwarded); got != tc.status {
				t.Fatalf("callback status=%d want%d", got, tc.status)
			}
		})
	}
	provider.mu.Lock()
	afterLookups := provider.lookups
	provider.mu.Unlock()
	var after string
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT to_jsonb(v)::text FROM phone_verification v WHERE phone=$1`, phone).Scan(&after); err != nil || before != after || afterLookups != lookups {
		t.Fatalf("rejected callback reached provider or changed verification: lookups=%d/%d err=%v", lookups, afterLookups, err)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE phone_verification RENAME COLUMN provider_call_id TO temporarily_unavailable`); err != nil {
		t.Fatal(err)
	}
	if got := send(http.MethodGet, "/callbacks/zvonok", query, "193.232.64.150"); got != http.StatusServiceUnavailable {
		t.Fatalf("callback acknowledged unavailable local storage: %d", got)
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `ALTER TABLE phone_verification RENAME COLUMN temporarily_unavailable TO provider_call_id`); err != nil {
		t.Fatal(err)
	}
	if got := send(http.MethodGet, "/callbacks/zvonok", query, "193.232.64.150"); got != http.StatusBadRequest {
		t.Fatalf("unconfirmed provider state was acknowledged after storage recovery: %d", got)
	}
}
