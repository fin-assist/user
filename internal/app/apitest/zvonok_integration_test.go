package apitest_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/adapter/zvonok"
	"github.com/fin-assist/user/internal/verification"
)

type integrationProviderGate struct{}

func (integrationProviderGate) Wait(context.Context) error { return nil }

func TestIntegrationZvonokProviderUsesTLSProtocolForCreationRecoveryAndCheckedProof(t *testing.T) {
	for _, mode := range []string{"ok", "lost", "reject", "lost-unavailable"} {
		t.Run(mode, func(t *testing.T) {
			p := &zvonokHTTP{calls: map[string]zvonokCall{}, mode: mode, balance: json.RawMessage(`"100.29"`)}
			srv := httptest.NewTLSServer(p)
			defer srv.Close()
			target, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			client, err := zvonok.New("test-private-key", "123", integrationProviderGate{}, zvonokHTTPTransport{target, srv.Client().Transport}, prometheus.NewRegistry())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			now := time.Now().UTC()
			attempt := verification.Attempt{ID: 7, Provider: "zvonok", Campaign: "123", FirstDispatchAt: now, DispatchNotAfter: now.Add(verification.RequestTTL), Phone: "+79001234567", CreatedAt: now, ExpiresAt: now.Add(verification.RequestTTL)}
			call, err := client.Create(t.Context(), attempt)
			switch mode {
			case "reject":
				if !errors.Is(err, verification.ErrProviderRejected) {
					t.Fatal("definite refusal not distinguished", err)
				}
				return
			case "lost-unavailable":
				if err == nil {
					t.Fatal("uncertain outcome accepted")
				}
				p.mu.Lock()
				p.mode = "ok"
				p.mu.Unlock()
			case "lost":
				if err == nil {
					t.Fatal("dropped reply accepted without recovery")
				}
			case "ok":
				if err != nil || call.ID == "" || len(call.Numbers) != 2 {
					t.Fatal(call, err)
				}
			}
			recovered, err := client.Recover(t.Context(), attempt)
			if err != nil || recovered.ID == "" || len(recovered.Numbers) == 0 {
				t.Fatal("durable attempt did not recover", recovered, err)
			}
			attempt.CallID = recovered.ID
			attempt.CreatedAt = recovered.StartedAt
			pending, err := client.Status(t.Context(), attempt)
			if err != nil || pending.State != verification.Pending {
				t.Fatal("pending status", pending, err)
			}
			mockCall(p, attempt.Phone, "unexpected-provider-state")
			unknown, err := client.Status(t.Context(), attempt)
			if err != nil || unknown.State != verification.Expired {
				t.Fatal("unknown state authorized confirmation", unknown, err)
			}
			mockCall(p, attempt.Phone, "pincode_ok")
			proof, err := client.Status(t.Context(), attempt)
			if err != nil || proof.State != verification.Verified || proof.ID != attempt.CallID || proof.Phone != attempt.Phone {
				t.Fatal("checked proof", proof, err)
			}
			wrong := attempt
			wrong.Phone = "+79007654321"
			if _, err := client.Status(t.Context(), wrong); !errors.Is(err, verification.ErrProof) {
				t.Fatal("foreign phone authorized", err)
			}
		})
	}
}

// The checker combines authenticated HTTP state with local PostgreSQL ownership;
// callback hints alone never create a proof or disclose an unknown provider ID.
func TestIntegrationCheckedProviderProofCommitsLocalVerification(t *testing.T) {
	env, p := zvonokEnv(t, "")
	srv := httptest.NewTLSServer(p)
	defer srv.Close()
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := zvonok.New("test-private-key", "123", integrationProviderGate{}, zvonokHTTPTransport{target, srv.Client().Transport}, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	now := time.Now().UTC()
	attempt := verification.Attempt{ID: 12, Provider: "zvonok", Campaign: "123", FirstDispatchAt: now, DispatchNotAfter: now.Add(verification.RequestTTL), Phone: "+79001234567", CreatedAt: now, ExpiresAt: now.Add(verification.RequestTTL)}
	call, err := client.Create(t.Context(), attempt)
	if err != nil {
		t.Fatal(err)
	}
	id := pendingPhoneAPI(t, env, "checked-provider-proof", attempt.Phone, attempt.ExpiresAt)
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE phone_verification SET provider='zvonok',campaign='123',provider_call_id=$1,created_at=$3,expires_at=$4 WHERE id=$2`, call.ID, id, call.StartedAt, call.PublicExpiry()); err != nil {
		t.Fatal(err)
	}
	repo := postgres.NewVerifications(env.App.DB, "123")
	checker, err := verification.NewChecker("zvonok", client, repo, env.App.Verifications, time.Second, 1, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	if err := checker.CheckCall(t.Context(), "unknown-call"); err == nil {
		t.Fatal("unknown callback accepted")
	}
	if err := checker.CheckCall(t.Context(), call.ID); !errors.Is(err, verification.ErrUnconfirmed) {
		t.Fatal(err)
	}
	request, err := repo.ByCall(t.Context(), "zvonok", call.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := checker.Check(t.Context(), request, false); !errors.Is(err, verification.ErrUnconfirmed) {
		t.Fatal(err)
	}
	// A repeated ordinary poll is coalesced, while a final poll bypasses capacity.
	if err := checker.Check(t.Context(), request, false); err != nil {
		t.Fatal(err)
	}
	other := request
	other.ID++
	if err := checker.Check(t.Context(), other, false); err != nil {
		t.Fatal(err)
	}
	foreign := request
	foreign.Provider = "foreign"
	if err := checker.Check(t.Context(), foreign, true); err != nil {
		t.Fatal(err)
	}
	mockCall(p, attempt.Phone, "pincode_ok")
	if err := checker.Check(t.Context(), request, true); err != nil {
		t.Fatal(err)
	}
	if err := checker.CheckCall(t.Context(), call.ID); err != nil {
		t.Fatal(err)
	}
	_, out, err := env.App.Verifications.Snapshot(t.Context(), "checked-provider-proof")
	if err != nil || out.State != verification.Verified || out.RegistrationToken == "" {
		t.Fatal(out, err)
	}
	var verified bool
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT verified_at IS NOT NULL FROM phone_verification WHERE id=$1`, id).Scan(&verified); err != nil || !verified {
		t.Fatal(verified, err)
	}
	// An ingress postback authenticates provider state and local ownership through
	// the same real checker. Trusted proxy chains are interpreted from the right.
	postback, err := zvonok.NewPostback(checker, []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		remote, forwarded, path string
		code                    int
	}{
		{"10.0.0.1:80", "193.232.64.150, 10.0.0.2", "/callbacks/zvonok?ct_call_id=" + call.ID, http.StatusOK},
		{"10.0.0.1:80", "foreign", "/callbacks/zvonok?ct_call_id=" + call.ID, http.StatusForbidden},
		{"10.0.0.1:80", strings.Repeat("10.0.0.2,", 17), "/callbacks/zvonok?ct_call_id=" + call.ID, http.StatusForbidden},
		{"10.0.0.1:80", "10.0.0.2", "/callbacks/zvonok?ct_call_id=" + call.ID, http.StatusForbidden},
		{"198.51.100.1:80", "193.232.64.150", "/callbacks/zvonok?ct_call_id=" + call.ID, http.StatusForbidden},
		{"193.232.64.150:80", "", "/callbacks/zvonok?ct_call_id=unknown", http.StatusBadRequest},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.RemoteAddr = tc.remote
		req.Header.Set("X-Forwarded-For", tc.forwarded)
		reply := httptest.NewRecorder()
		postback.ServeHTTP(reply, req)
		if reply.Code != tc.code || reply.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(tc, reply.Code)
		}
	}
	// FIN-306: a proxy-appended header line holds the rightmost hop; a spoofed
	// first line must not make a foreign client look like the provider.
	for _, tc := range []struct {
		lines []string
		code  int
	}{
		{[]string{"193.232.64.150", "198.51.100.9"}, http.StatusForbidden},
		{[]string{"198.51.100.9", "193.232.64.150"}, http.StatusOK},
	} {
		req := httptest.NewRequest(http.MethodGet, "/callbacks/zvonok?ct_call_id="+call.ID, nil)
		req.RemoteAddr = "10.0.0.1:80"
		for _, line := range tc.lines {
			req.Header.Add("X-Forwarded-For", line)
		}
		reply := httptest.NewRecorder()
		postback.ServeHTTP(reply, req)
		if reply.Code != tc.code {
			t.Fatal(tc.lines, reply.Code)
		}
	}
	// Local ownership mismatch must fail without confirming a second request.
	foreign = request
	foreign.Phone = "+79007654321"
	if err := checker.Check(t.Context(), foreign, true); !errors.Is(err, verification.ErrProof) {
		t.Fatal(err)
	}
	// Exercise expiry/eviction without a fake clock or sleeps: a final pass is still
	// authenticated even when no ordinary polling slot can be allocated.
	request.VerifiedAt = &now
	if err := checker.Check(t.Context(), request, true); err != nil {
		t.Fatal(err)
	}
}
