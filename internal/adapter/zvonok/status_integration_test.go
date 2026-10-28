package zvonok

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/verification"
)

type readOnlyHTTPCounts struct {
	reads, posts atomic.Int32
}

func readOnlyTLSProvider(t *testing.T, operation, body string, query url.Values, now func() time.Time) (*Client, *readOnlyHTTPCounts) {
	t.Helper()
	counts := new(readOnlyHTTPCounts)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			counts.posts.Add(1)
		}
		if r.Method != http.MethodGet {
			t.Error("read-only provider operation attempted a write")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		counts.reads.Add(1)
		if r.TLS == nil || r.URL.Path != "/manager/cabapi_external/api/v1/"+operation+"/" || r.URL.Query().Get("public_key") != "secret-key" {
			t.Error("provider read lost TLS, authentication or operation")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if len(r.URL.Query()) != len(query)+1 {
			t.Error("unexpected provider query fields")
		}
		for key, values := range query {
			if len(r.URL.Query()[key]) != 1 || r.URL.Query().Get(key) != values[0] {
				t.Errorf("provider read changed %s", key)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "zvonok.com" {
			return nil, errors.New("provider endpoint was not pinned")
		}
		copy := r.Clone(r.Context())
		endpoint := *r.URL
		endpoint.Scheme, endpoint.Host = target.Scheme, target.Host
		copy.URL = &endpoint
		return server.Client().Transport.RoundTrip(copy)
	})
	c, err := New("secret-key", "123", gateFunc(func(context.Context) error { return nil }), transport, prometheus.NewRegistry(), now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, counts
}

func TestIntegrationStatusBoundCreationUsesAuthenticatedTLS(t *testing.T) {
	created := time.Now().UTC().Truncate(time.Microsecond)
	for _, tc := range []struct {
		name        string
		remoteDelta time.Duration
		missingDate bool
		want        error
		reads       int32
	}{
		{name: "same-created-verified", reads: 1},
		{name: "changed-created-microsecond", remoteDelta: time.Microsecond, want: verification.ErrProof, reads: 1},
		{name: "missing-bound-created", missingDate: true, want: verification.ErrProof},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, counts := readOnlyTLSProvider(t, "phones/call_by_id", "["+remote("42", created.Add(tc.remoteDelta), "pincode_ok")+"]", url.Values{"call_id": {"42"}}, func() time.Time { return created.Add(10 * time.Second) })
			a := attempt()
			a.CallID, a.CreatedAt = "42", created
			if tc.missingDate {
				a.CreatedAt = time.Time{}
			}
			out, err := c.Status(t.Context(), a)
			if !errors.Is(err, tc.want) {
				t.Fatal("unexpected status proof result", err)
			}
			if tc.want == nil && (out.ID != a.CallID || out.Phone != a.Phone || out.State != verification.Verified) {
				t.Fatal("authenticated confirmation lost its bound identity or state")
			}
			if tc.want != nil && (out.ID != "" || out.Phone != "") {
				t.Fatal("invalid creation timestamp returned an identity proof")
			}
			if counts.reads.Load() != tc.reads || counts.posts.Load() != 0 {
				t.Fatal("status used unexpected HTTP operations", counts.reads.Load(), counts.posts.Load())
			}
		})
	}
}

func TestIntegrationRecoveryIncompleteTLSResponsesNeverRepeatConfirm(t *testing.T) {
	created := time.Now().UTC().Truncate(time.Millisecond)
	for _, tc := range []struct {
		name, body string
		want       error
	}{
		{"missing-pool", "[" + strings.Replace(remote("42", created, "in_process"), `["+74951234567","+74957654321"]`, `[]`, 1) + "]", verification.ErrProviderUnavailable},
		{"terminal-unbound-candidate", "[" + remote("42", created, "pincode_ok") + "]", verification.ErrProviderTerminal},
		{"empty-history", "[]", verification.ErrProviderUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := attempt()
			a.CreatedAt, a.FirstDispatchAt, a.DispatchNotAfter = created, created, created.Add(time.Minute)
			c, counts := readOnlyTLSProvider(t, "phones/calls_by_phone", tc.body, url.Values{
				"campaign_id": {a.Campaign}, "phone": {a.Phone},
				"from_created_date": {created.Add(-24 * time.Hour).Format("2006-01-02 15:04:05")},
			}, func() time.Time { return created.Add(10 * time.Second) })
			out, err := c.Recover(t.Context(), a)
			if !errors.Is(err, tc.want) || out.ID != "" || len(out.Numbers) != 0 {
				t.Fatal("incomplete read created or adopted a call", err)
			}
			if counts.reads.Load() != 1 || counts.posts.Load() != 0 {
				t.Fatal("recovery repeated provider creation", counts.reads.Load(), counts.posts.Load())
			}
		})
	}
}

func TestIntegrationRecoveryUsesInjectedAdmissionClockAndElapsedTimeout(t *testing.T) {
	// Both a past and future business date must work with an authenticated
	// provider response on that same time scale. A wall-clock absolute context
	// deadline would cancel the past-date read before it reaches HTTP.
	for _, year := range []int{2020, 2032} {
		t.Run(time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).Format("2006"), func(t *testing.T) {
			created := time.Date(year, time.February, 29, 23, 59, 0, 0, time.UTC)
			now := func() time.Time { return created.Add(10 * time.Second) }
			a := attempt()
			a.CreatedAt, a.FirstDispatchAt, a.DispatchNotAfter = created, created, created.Add(time.Minute)
			c, counts := readOnlyTLSProvider(t, "phones/calls_by_phone", "["+remote("42", created, "in_process")+"]", url.Values{
				"campaign_id": {a.Campaign}, "phone": {a.Phone},
				"from_created_date": {created.Add(-24 * time.Hour).Format("2006-01-02 15:04:05")},
			}, now)
			out, err := c.Recover(t.Context(), a)
			if err != nil || out.ID != "42" || !out.StartedAt.Equal(created) || !out.ReceivedAt.Equal(now()) || !out.PublicExpiry().Equal(created.Add(verification.RequestTTL-ClockSkewTolerance)) {
				t.Fatal("constructor clock did not govern trusted recovery", out, err)
			}
			if counts.reads.Load() != 1 || counts.posts.Load() != 0 {
				t.Fatal("clock override changed read-only dispatch semantics", counts.reads.Load(), counts.posts.Load())
			}
			latency := prometheus.NewRegistry()
			latency.MustRegister(c.latency)
			assertPositiveRequestLatency(t, latency)
		})
	}
}
