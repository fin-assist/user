package zvonok

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/verification"
)

type gateFunc func(context.Context) error

func (f gateFunc) Wait(ctx context.Context) error { return f(ctx) }

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

var testNow = time.Date(2026, 10, 24, 12, 0, 20, 0, time.UTC)

func attempt() verification.Attempt {
	return verification.Attempt{ID: 1, Provider: "zvonok", Phone: "+79991234567", CreatedAt: testNow.Add(-10 * time.Second), ExpiresAt: testNow.Add(290 * time.Second)}
}
func remote(id string, created time.Time, state string) string {
	return fmt.Sprintf(`{"call_id":%s,"phone":"+79991234567","created":%q,"call_status":%q,"allowed_phones_for_call":["+74951234567","+74957654321"],"balance":"100.29"}`, id, created.Format(time.RFC3339Nano), state)
}
func client(t *testing.T, f transportFunc) *Client {
	t.Helper()
	c, err := New("secret-key", "123", gateFunc(func(context.Context) error { return nil }), f, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return testNow }
	return c
}

func TestCreateMultipartAndPrivateReads(t *testing.T) {
	var paths []string
	c := client(t, func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.URL.Path)
		if r.URL.Scheme != "https" || r.URL.Host != "zvonok.com" {
			t.Fatal("endpoint", r.URL.Host)
		}
		if strings.HasSuffix(r.URL.Path, "calls_by_phone/") {
			if r.Method != "GET" || r.URL.Query().Get("public_key") != "secret-key" || r.URL.Query().Get("phone") != attempt().Phone || r.URL.Query().Get("from_created_date") == "" {
				t.Fatal("lookup parameters")
			}
			return reply(200, `[]`), nil
		}
		if r.Method != "POST" || r.URL.RawQuery != "" {
			t.Fatal("secret in create URL")
		}
		if err := r.ParseMultipartForm(8192); err != nil {
			t.Fatal(err)
		}
		if len(r.PostForm) != 3 || r.PostFormValue("public_key") != "secret-key" || r.PostFormValue("phone") != attempt().Phone || r.PostFormValue("campaign_id") != "123" {
			t.Fatal("multipart")
		}
		return reply(200, `{"status":"ok","data":`+remote("1", attempt().CreatedAt.Add(time.Millisecond), "in_process")+`}`), nil
	})
	out, err := c.Create(t.Context(), attempt())
	if err != nil || out.ID != "1" || out.StartedAt != attempt().CreatedAt || len(out.Numbers) != 2 || len(paths) != 2 {
		t.Fatal(out, err, paths)
	}
}

func TestCreateAndRecoverIgnoreBalance(t *testing.T) {
	for _, tt := range []struct {
		name, field string
	}{
		{"absent", ""},
		{"number", `,"balance":100.29`},
		{"string", `,"balance":"100.29"`},
	} {
		for _, recoverCall := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/recover=%t", tt.name, recoverCall), func(t *testing.T) {
				data := strings.Replace(remote("1", attempt().CreatedAt, "in_process"), `,"balance":"100.29"`, tt.field, 1)
				c := client(t, func(r *http.Request) (*http.Response, error) {
					if r.Method == http.MethodGet {
						if recoverCall {
							return reply(200, "["+data+"]"), nil
						}
						return reply(200, "[]"), nil
					}
					return reply(200, `{"status":"ok","data":`+data+`}`), nil
				})
				operation := c.Create
				if recoverCall {
					operation = c.Recover
				}
				out, err := operation(t.Context(), attempt())
				if err != nil || out.ID != "1" || out.StartedAt != attempt().CreatedAt || len(out.Numbers) != 2 {
					t.Fatal(out, err)
				}
			})
		}
	}
}

func TestRecoveryChecksIdentityAndTerminalRace(t *testing.T) {
	for _, tt := range []struct {
		name, lookup, returned string
		want                   error
		newCall                bool
		writes                 int
	}{
		{"same", remote("1", attempt().CreatedAt, "in_process"), remote("1", attempt().CreatedAt, "in_process"), nil, false, 1},
		{"lost-replacement", remote("1", attempt().CreatedAt, "pincode_ok") + "," + remote("2", testNow, "in_process"), remote("2", testNow, "in_process"), nil, true, 1},
		{"terminal", remote("1", attempt().CreatedAt, "pincode_ok"), remote("2", testNow, "in_process"), nil, true, 1},
		{"race", remote("1", attempt().CreatedAt, "in_process"), remote("2", testNow, "in_process"), nil, true, 1},
		{"foreign", remote("1", attempt().CreatedAt.Add(-time.Second), "in_process"), "", verification.ErrProviderDuplicate, false, 0},
		{"wrong-phone", strings.ReplaceAll(remote("1", attempt().CreatedAt, "in_process"), "+79991234567", "+79997654321"), "", errUnavailable, false, 0},
		{"ambiguous", remote("1", attempt().CreatedAt, "in_process") + "," + remote("2", testNow, "in_process"), "", errUnavailable, false, 0},
		{"unknown-state", remote("1", attempt().CreatedAt, "future_status"), "", errUnavailable, false, 0},
		{"not-accepted", "", remote("2", testNow, "in_process"), nil, true, 1},
		{"old-terminal", remote("1", attempt().CreatedAt.Add(-time.Minute), "expires"), remote("2", testNow, "in_process"), nil, true, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			writes := 0
			c := client(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == "GET" {
					return reply(200, "["+tt.lookup+"]"), nil
				}
				writes++
				return reply(200, `{"status":"ok","data":`+tt.returned+`}`), nil
			})
			out, err := c.Recover(t.Context(), attempt())
			if !errors.Is(err, tt.want) || writes != tt.writes {
				t.Fatal(out, err, writes)
			}
			if err == nil {
				want := attempt().CreatedAt
				if tt.newCall {
					want = testNow
				}
				if out.StartedAt != want {
					t.Fatal("lifetime", out.StartedAt, want)
				}
			}
		})
	}
}

func TestHTTPFailuresCannotLeakSecretsOrAuthorizeRefunds(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  int
		body    string
		network bool
		want    error
	}{
		{"rate-limit", 429, "secret-key", false, verification.ErrProviderRejected},
		{"rejected", 400, `{"status":"error","data":"+79991234567 secret-key"}`, false, verification.ErrProviderRejected},
		{"server", 503, "secret-key", false, errUnavailable},
		{"bad-json", 200, "secret-key", false, errUnavailable},
		{"lost", 200, "", true, errUnavailable},
		{"redirect", 302, "secret-key", false, errUnavailable},
		{"oversized", 200, strings.Repeat("x", maxResponse+1), false, errUnavailable},
		{"empty-pool", 200, `{"status":"ok","data":` + strings.ReplaceAll(remote("1", testNow, "in_process"), `["+74951234567","+74957654321"]`, `[]`) + `}`, false, errUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := client(t, func(r *http.Request) (*http.Response, error) {
				if tt.network {
					return nil, &url.Error{Op: "POST", URL: "https://zvonok.com?public_key=secret-key", Err: errors.New("private +79991234567")}
				}
				return reply(tt.status, tt.body), nil
			})
			_, err := c.confirm(t.Context(), attempt(), "", attempt().CreatedAt)
			if !errors.Is(err, tt.want) || strings.Contains(err.Error(), "secret-key") || strings.Contains(err.Error(), "7999") {
				t.Fatal(err)
			}
		})
	}
	if _, err := New("", "123", nil, nil, prometheus.NewRegistry()); err == nil {
		t.Fatal("invalid settings")
	}
}

func TestStatusUsesAuthenticatedIdentityAndCurrentStatus(t *testing.T) {
	for _, tt := range []struct {
		data  string
		state verification.State
		want  error
	}{
		{remote("1", testNow, "pincode_ok"), verification.Verified, nil},
		{remote("1", testNow, "in_process"), verification.Pending, nil},
		{strings.Replace(remote("1", testNow, "pincode_ok"), `"call_status":"pincode_ok"`, `"call_status":"expires","status":"pincode_ok"`, 1), verification.Expired, nil},
		{remote("2", testNow, "pincode_ok"), 0, verification.ErrProof},
		{strings.ReplaceAll(remote("1", testNow, "pincode_ok"), "+79991234567", "+79997654321"), 0, verification.ErrProof},
	} {
		c := client(t, func(r *http.Request) (*http.Response, error) {
			if r.URL.Query().Get("call_id") != "1" {
				t.Fatal("call id")
			}
			return reply(200, "["+tt.data+"]"), nil
		})
		a := attempt()
		a.CallID = "1"
		out, err := c.Status(t.Context(), a)
		if !errors.Is(err, tt.want) || out.State != tt.state {
			t.Fatal(out, err)
		}
	}
}

func TestRealHTTPDoesNotFollowRedirect(t *testing.T) {
	var redirected int
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected++; w.WriteHeader(200) }))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	u, _ := url.Parse(source.URL)
	c := client(t, func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		clone := *r.URL
		copy.URL = &clone
		copy.URL.Scheme = u.Scheme
		copy.URL.Host = u.Host
		return source.Client().Transport.RoundTrip(copy)
	})
	_, err := c.confirm(t.Context(), attempt(), "", attempt().CreatedAt)
	if err == nil || redirected != 0 {
		t.Fatal(err, redirected)
	}
}

func TestMalformedCallIDsAndGateCancellation(t *testing.T) {
	for _, raw := range []string{`null`, `0`, `-1`, `1.5`, `1e10`, `"x"`} {
		var id callID
		if json.Unmarshal([]byte(raw), &id) == nil {
			t.Fatal(raw)
		}
	}
	c := client(t, func(*http.Request) (*http.Response, error) { t.Fatal("HTTP after gate denial"); return nil, nil })
	c.gate = gateFunc(func(context.Context) error { return context.Canceled })
	if _, err := c.Status(t.Context(), verification.Attempt{CallID: "1"}); err == nil {
		t.Fatal("gate ignored")
	}
}

func TestRecoveryRefusalCannotRefundPreviouslyAcceptedCall(t *testing.T) {
	for _, status := range []int{400, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			c := client(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == "GET" {
					return reply(200, "["+remote("1", attempt().CreatedAt, "in_process")+"]"), nil
				}
				return reply(status, `{"status":"error","data":"recovery refused"}`), nil
			})
			_, err := c.Recover(t.Context(), attempt())
			if !errors.Is(err, errUnavailable) || errors.Is(err, verification.ErrProviderRejected) {
				t.Fatal(err)
			}
		})
	}
}

func TestHistoryErrorsNeverBecomeBlindWrites(t *testing.T) {
	for _, body := range []string{`null`, `not-json`, `[{"call_id":1,"phone":"bad"}]`, `{"status":"error","data":"authentication failed"}`} {
		writes := 0
		c := client(t, func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodPost {
				writes++
			}
			return reply(200, body), nil
		})
		if _, err := c.Create(t.Context(), attempt()); !errors.Is(err, verification.ErrProviderRejected) || writes != 0 {
			t.Fatal(body, err, writes)
		}
		if _, err := c.Recover(t.Context(), attempt()); !errors.Is(err, errUnavailable) || writes != 0 {
			t.Fatal(body, err, writes)
		}
	}
	c := client(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			return reply(400, `{"status":"error","data":"Phone by val=+79991234567 not found"}`), nil
		}
		return reply(200, `{"status":"ok","data":`+remote("1", testNow, "in_process")+`}`), nil
	})
	if _, err := c.Create(t.Context(), attempt()); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryByKnownIDAndStaleHistoryPreservesCallLifetime(t *testing.T) {
	for _, returned := range []string{"1", "2", "missing"} {
		t.Run(returned, func(t *testing.T) {
			writes := 0
			c := client(t, func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodGet {
					if !strings.HasSuffix(r.URL.Path, "call_by_id/") || r.URL.Query().Get("call_id") != "1" {
						t.Fatal("wrong lookup")
					}
					if returned == "missing" {
						return reply(200, `[]`), nil
					}
					return reply(200, "["+remote(returned, attempt().CreatedAt, "in_process")+"]"), nil
				}
				writes++
				return reply(200, `{"status":"ok","data":`+remote("1", attempt().CreatedAt, "in_process")+`}`), nil
			})
			a := attempt()
			a.CallID = "1"
			out, err := c.Recover(t.Context(), a)
			if returned == "1" {
				if err != nil || writes != 1 || out.StartedAt != a.CreatedAt {
					t.Fatal(out, err, writes)
				}
			} else if err == nil || writes != 0 {
				t.Fatal(out, err, writes)
			}
		})
	}
	// A temporarily empty history can still return the original call from confirm.
	c := client(t, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			return reply(200, `[]`), nil
		}
		return reply(200, `{"status":"ok","data":`+remote("1", attempt().CreatedAt, "in_process")+`}`), nil
	})
	if out, err := c.Recover(t.Context(), attempt()); err != nil || out.StartedAt != attempt().CreatedAt {
		t.Fatal(out, err)
	}
}
