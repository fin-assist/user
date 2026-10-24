package zvonok

import (
	"context"
	"errors"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/verification"
)

type callbackFunc func(context.Context, string) error

func (f callbackFunc) CheckCall(ctx context.Context, id string) error { return f(ctx, id) }

func TestUnitPostbackTrustBoundaryAndAcknowledgment(t *testing.T) {
	for _, tt := range []struct {
		name, peer, forwarded, path, method string
		err                                 error
		want, calls                         int
	}{
		{"valid", "193.232.64.150:1234", "", "/callbacks/zvonok?ct_call_id=123&ct_status=forged&ct_phone=forged", "GET", nil, 200, 1},
		{"forged-forwarded", "203.0.113.1:2", "193.232.64.150", "/callbacks/zvonok?ct_call_id=123", "GET", nil, 403, 0},
		{"trusted-proxy", "10.0.0.1:2", "203.0.113.1, 193.232.64.150", "/callbacks/zvonok?ct_call_id=123", "GET", nil, 200, 1},
		{"trusted-chain", "10.0.0.1:2", "193.232.64.150, 10.0.0.2", "/callbacks/zvonok?ct_call_id=123", "GET", nil, 200, 1},
		{"bad-chain", "10.0.0.1:2", "193.232.64.150, invalid", "/callbacks/zvonok?ct_call_id=123", "GET", nil, 403, 0},
		{"no-chain", "10.0.0.1:2", "", "/callbacks/zvonok?ct_call_id=123", "GET", nil, 403, 0},
		{"malformed-peer", "invalid", "", "/callbacks/zvonok?ct_call_id=123", "GET", nil, 403, 0},
		{"unknown", "193.232.64.150:1", "", "/callbacks/zvonok?ct_call_id=123", "GET", verification.ErrMissing, 404, 1},
		{"pending", "193.232.64.150:1", "", "/callbacks/zvonok?ct_call_id=123", "GET", verification.ErrUnconfirmed, 400, 1},
		{"mismatch", "193.232.64.150:1", "", "/callbacks/zvonok?ct_call_id=123", "GET", verification.ErrProof, 400, 1},
		{"db-failed", "193.232.64.150:1", "", "/callbacks/zvonok?ct_call_id=123", "GET", errors.New("private"), 503, 1},
		{"double-id", "193.232.64.150:1", "", "/callbacks/zvonok?ct_call_id=123&ct_call_id=1", "GET", nil, 400, 0},
		{"invalid-id", "193.232.64.150:1", "", "/callbacks/zvonok?ct_call_id=-1", "GET", nil, 400, 0},
		{"wrong-method", "193.232.64.150:1", "", "/callbacks/zvonok?ct_call_id=123", "POST", nil, 404, 0},
		{"ops-hidden", "193.232.64.150:1", "", "/metrics", "GET", nil, 404, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			h, err := NewPostback(callbackFunc(func(ctx context.Context, id string) error {
				calls++
				if id != "123" {
					t.Fatal(id)
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("unbounded")
				}
				return tt.err
			}), []netip.Prefix{netip.MustParsePrefix("10.0.0.0/24")}, prometheus.NewRegistry())
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(tt.method, tt.path, nil)
			r.RemoteAddr = tt.peer
			r.Header.Set("X-Forwarded-For", tt.forwarded)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tt.want || calls != tt.calls || w.Body.Len() != 0 {
				t.Fatal(w.Code, calls, w.Body.String())
			}
		})
	}
	if _, err := NewPostback(nil, nil, prometheus.NewRegistry()); err == nil {
		t.Fatal("nil callback")
	}
}
