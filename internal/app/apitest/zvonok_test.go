package apitest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/protobuf/proto"

	"github.com/fin-assist/user/internal/app"
	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/config"
)

type zvonokCall struct {
	ID      int64     `json:"call_id"`
	Phone   string    `json:"phone"`
	Created time.Time `json:"created"`
	State   string    `json:"call_status"`
	Numbers []string  `json:"allowed_phones_for_call,omitempty"`
	Balance string    `json:"balance,omitempty"`
}
type zvonokHTTP struct {
	mu      sync.Mutex
	calls   map[string]zvonokCall
	mode    string
	adds    int
	lookups int
	next    int64
}

func (p *zvonokHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var values url.Values
	if r.Method == http.MethodPost {
		if r.URL.RawQuery != "" || r.ParseMultipartForm(8192) != nil {
			w.WriteHeader(400)
			return
		}
		values = r.PostForm
	} else {
		values = r.URL.Query()
	}
	if values.Get("public_key") != "test-private-key" {
		w.WriteHeader(403)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "calls_by_phone/") {
		p.lookups++
		if p.mode == "lost-unavailable" && p.adds > 0 {
			w.WriteHeader(503)
			return
		}
		var calls []zvonokCall
		if c, ok := p.calls[values.Get("phone")]; ok {
			c.Numbers = nil
			calls = append(calls, c)
		}
		if calls == nil {
			calls = []zvonokCall{}
		}
		_ = json.NewEncoder(w).Encode(calls)
		return
	}
	if strings.HasSuffix(r.URL.Path, "call_by_id/") {
		p.lookups++
		for _, c := range p.calls {
			if strconv.FormatInt(c.ID, 10) == values.Get("call_id") {
				c.Numbers = nil
				_ = json.NewEncoder(w).Encode([]zvonokCall{c})
				return
			}
		}
		_ = json.NewEncoder(w).Encode([]zvonokCall{})
		return
	}
	if !strings.HasSuffix(r.URL.Path, "confirm/") {
		w.WriteHeader(404)
		return
	}
	p.adds++
	if p.mode == "reject" || (p.mode == "lost-reject" && p.adds > 1) {
		w.WriteHeader(429)
		return
	}
	phone := values.Get("phone")
	c, ok := p.calls[phone]
	if !ok || c.State != "in_process" {
		p.next++
		c = zvonokCall{ID: p.next, Phone: phone, Created: time.Now().UTC(), State: "in_process", Numbers: []string{"+74951234567", "+74957654321"}, Balance: "100.29"}
		p.calls[phone] = c
	}
	if (p.mode == "lost" || p.mode == "lost-unavailable" || p.mode == "lost-reject") && p.adds == 1 {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "data": c})
}

type zvonokHTTPTransport struct {
	target    *url.URL
	transport http.RoundTripper
}

func (x zvonokHTTPTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	u := *r.URL
	u.Scheme = x.target.Scheme
	u.Host = x.target.Host
	copy.URL = &u
	return x.transport.RoundTrip(copy)
}

func zvonokEnv(t *testing.T, mode string) (*apitest.Env, *zvonokHTTP) {
	t.Helper()
	p := &zvonokHTTP{calls: map[string]zvonokCall{}, mode: mode}
	srv := httptest.NewTLSServer(p)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	env := apitest.StartWithOptions(t, []app.Option{app.WithZvonokTransport(zvonokHTTPTransport{u, srv.Client().Transport})}, func(c *config.Config) {
		c.Zvonok = config.ZvonokConfig{Provider: "zvonok", PublicKey: "test-private-key", CampaignID: "123", CallbackAddr: "127.0.0.1:0", PollInterval: time.Second, TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}
		c.Phone.ResyncInterval = time.Second
	})
	return env, p
}
func callbackHTTP(t *testing.T, env *apitest.Env, id int64, ip string) int {
	t.Helper()
	srv := httptest.NewServer(env.App.PhoneCallback)
	defer srv.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("%s/callbacks/zvonok?ct_call_id=%d&ct_phone=forged&ct_status=forged", srv.URL, id), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Forwarded-For", ip)
	r, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) != 0 {
		t.Fatal(string(body), err)
	}
	return r.StatusCode
}
func mockCall(p *zvonokHTTP, phone string, state string) zvonokCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := p.calls[phone]
	if state != "" {
		c.State = state
		p.calls[phone] = c
	}
	return c
}

func TestZvonokAPIStartRecoveryPoolAndIdempotentReplay(t *testing.T) {
	env, p := zvonokEnv(t, "lost")
	key, phone := authKey(), "+79991234567"
	first, err := startCall(t, env, key, phone)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := startCall(t, env, key, phone)
	if err != nil || first.GetVerificationToken() != replay.GetVerificationToken() || first.GetCallbackNumber() != replay.GetCallbackNumber() || !first.GetExpiresAt().AsTime().Equal(replay.GetExpiresAt().AsTime()) {
		t.Fatal(first, replay, err)
	}
	p.mu.Lock()
	adds, calls := p.adds, len(p.calls)
	p.mu.Unlock()
	if adds != 2 || calls != 1 {
		t.Fatal("duplicate provider call", adds, calls)
	}
	var pool []string
	var selected string
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT allowed_phones_for_call,callback_number FROM phone_verification`).Scan(&pool, &selected); err != nil || len(pool) != 2 || selected != first.GetCallbackNumber() {
		t.Fatal(pool, selected, err)
	}
	if startHits(t, env) != 1 {
		t.Fatal("retry charged")
	}
	if _, err = startCall(t, env, authKey(), phone); apitest.Reason(err) != "RATE_LIMITED" {
		t.Fatal(err)
	}
	if value := zvonokMetric(t, env, "finassist_zvonok_balance_rubles", nil); value != 100.29 {
		t.Fatal("balance observation", value)
	}
}

func TestZvonokAPIUnknownOutcomeKeepsOwnAttemptAndRejectsForeignCall(t *testing.T) {
	for _, mode := range []string{"lost-unavailable", "lost-reject"} {
		t.Run(mode, func(t *testing.T) { unknownZvonokOutcome(t, mode) })
	}
}
func unknownZvonokOutcome(t *testing.T, mode string) {
	t.Helper()
	env, p := zvonokEnv(t, mode)
	key, phone := authKey(), "+79991234567"
	if out, err := startCall(t, env, key, phone); err == nil || out != nil {
		t.Fatal(out, err)
	}
	if startHits(t, env) != 1 {
		t.Fatal("unknown outcome refunded")
	}
	p.mu.Lock()
	p.mode = ""
	p.mu.Unlock()
	if _, err := startCall(t, env, key, phone); err != nil {
		t.Fatal(err)
	}
	if startHits(t, env) != 1 {
		t.Fatal("own recovery charged")
	}
	foreign := "+79997654321"
	p.mu.Lock()
	p.calls[foreign] = zvonokCall{ID: 99, Phone: foreign, Created: time.Now().Add(-time.Minute), State: "in_process"}
	adds := p.adds
	p.mu.Unlock()
	if out, err := startCall(t, env, authKey(), foreign); apitest.Reason(err) != "RATE_LIMITED" || out != nil {
		t.Fatal(out, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.adds != adds {
		t.Fatal("foreign call adopted")
	}
}

func TestZvonokAPIExplicitRejectionRefundsAndAllowsRetry(t *testing.T) {
	env, p := zvonokEnv(t, "reject")
	key := authKey()
	if _, err := startCall(t, env, key, "+79991234567"); err == nil {
		t.Fatal("429 accepted")
	}
	if startHits(t, env) != 0 {
		t.Fatal("explicit rejection charged")
	}
	p.mu.Lock()
	p.mode = ""
	p.mu.Unlock()
	if _, err := startCall(t, env, key, "+79991234567"); err != nil {
		t.Fatal(err)
	}
	if startHits(t, env) != 1 {
		t.Fatal("retry not charged once")
	}
}

func TestZvonokAPIPostbackCheckedProofRepeatAndLateEvent(t *testing.T) {
	env, p := zvonokEnv(t, "")
	phoneJob(t, env)
	phone := "+79991234567"
	out, err := startCall(t, env, authKey(), phone)
	if err != nil {
		t.Fatal(err)
	}
	c := mockCall(p, phone, "")
	if code := callbackHTTP(t, env, c.ID, "203.0.113.1"); code != 403 {
		t.Fatal(code)
	}
	if code := callbackHTTP(t, env, 999, "193.232.64.150"); code != 404 {
		t.Fatal(code)
	}
	if code := callbackHTTP(t, env, c.ID, "193.232.64.150"); code != 400 {
		t.Fatal("unconfirmed hints trusted", code)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stream := openPhoneWatch(t, env, phoneContext(ctx), out.GetVerificationToken())
	if pending, err := stream.Recv(); err != nil || pending.GetStatus() != userv1.VerificationStatus_VERIFICATION_STATUS_PENDING {
		t.Fatal(pending, err)
	}
	mockCall(p, phone, "pincode_ok")
	if code := callbackHTTP(t, env, c.ID, "193.232.64.150"); code != 200 {
		t.Fatal(code)
	}
	verified, err := stream.Recv()
	if err != nil || verified.GetStatus() != userv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED || verified.GetRegistrationToken() == "" {
		t.Fatal(verified, err)
	}
	if code := callbackHTTP(t, env, c.ID, "193.232.64.150"); code != 200 {
		t.Fatal(code)
	}
	replayed := openPhoneWatch(t, env, phoneContext(ctx), out.GetVerificationToken())
	again, err := replayed.Recv()
	if err != nil || !proto.Equal(verified, again) {
		t.Fatal(again, err)
	}
	latePhone := "+79997654321"
	if _, err = startCall(t, env, authKey(), latePhone); err != nil {
		t.Fatal(err)
	}
	late := mockCall(p, latePhone, "pincode_ok")
	if _, err = env.App.DB.Primary().Exec(t.Context(), `UPDATE phone_verification SET expires_at=$2 WHERE phone=$1`, latePhone, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if code := callbackHTTP(t, env, late.ID, "193.232.64.150"); code != 200 {
		t.Fatal(code)
	}
	var confirmed bool
	if err = env.App.DB.Primary().QueryRow(t.Context(), `SELECT verified_at IS NOT NULL FROM phone_verification WHERE phone=$1`, latePhone).Scan(&confirmed); err != nil || confirmed {
		t.Fatal("late confirmation", confirmed, err)
	}
}

func TestZvonokAPILostPostbackUsesWatchFallback(t *testing.T) {
	env, p := zvonokEnv(t, "")
	phoneJob(t, env)
	out, err := startCall(t, env, authKey(), "+79991234567")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	stream := openPhoneWatch(t, env, phoneContext(ctx), out.GetVerificationToken())
	if pending, err := stream.Recv(); err != nil || pending.GetStatus() != userv1.VerificationStatus_VERIFICATION_STATUS_PENDING {
		t.Fatal(pending, err)
	}
	mockCall(p, "+79991234567", "pincode_ok")
	verified, err := stream.Recv()
	if err != nil || verified.GetStatus() != userv1.VerificationStatus_VERIFICATION_STATUS_VERIFIED {
		t.Fatal(verified, err)
	}
	if value := zvonokMetric(t, env, "finassist_phone_provider_checks_total", map[string]string{"source": "fallback", "result": "confirmed"}); value != 1 {
		t.Fatal(value)
	}
}

func zvonokMetric(t *testing.T, env *apitest.Env, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := env.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.Metric {
			match := len(m.Label) == len(labels)
			for _, l := range m.Label {
				if labels[l.GetName()] != l.GetValue() {
					match = false
				}
			}
			if match {
				if m.Gauge != nil {
					return m.Gauge.GetValue()
				}
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}
