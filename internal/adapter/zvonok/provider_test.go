package zvonok

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/verification"
)

func TestUnitProviderAdmissionClockStatusAndPoolBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		created time.Time
		status  string
		mutate  func(*remoteCall)
		want    error
	}{
		{"lower-skew-bound", attempt().FirstDispatchAt.Truncate(time.Millisecond).Add(-ClockSkewTolerance), "wait", nil, nil},
		{"below-lower-skew", attempt().FirstDispatchAt.Truncate(time.Millisecond).Add(-ClockSkewTolerance - time.Nanosecond), "wait", nil, verification.ErrProviderDuplicate},
		{"upper-skew-bound", testNow.Add(ClockSkewTolerance), "new", nil, nil},
		{"above-upper-skew", testNow.Add(ClockSkewTolerance + time.Nanosecond), "new", nil, errUnavailable},
		{"terminal-pincode", testNow, "pincode_ok", nil, verification.ErrProviderTerminal},
		{"terminal-expired", testNow, "expires", nil, verification.ErrProviderTerminal},
		{"completed-pending", testNow, "in_process", func(r *remoteCall) { r.Completed = &testNow }, verification.ErrProviderTerminal},
		{"zero-completed", testNow, "in_process", func(r *remoteCall) { r.Completed = new(time.Time) }, nil},
		{"unknown-state", testNow, "unknown", nil, errUnavailable},
		{"empty-pool", testNow, "in_process", func(r *remoteCall) { r.Numbers = nil }, errUnavailable},
		{"duplicate-pool", testNow, "in_process", func(r *remoteCall) { r.Numbers = []string{"+74951234567", "+74951234567"} }, errUnavailable},
		{"bad-pool", testNow, "in_process", func(r *remoteCall) { r.Numbers = []string{"74951234567"} }, errUnavailable},
		{"too-large-pool", testNow, "in_process", func(r *remoteCall) { r.Numbers = make([]string, verification.MaxCallbackPoolNumbers+1) }, errUnavailable},
		{"wrong-phone", testNow, "in_process", func(r *remoteCall) { r.Phone = "+79997654321" }, errUnavailable},
		{"invalid-id", testNow, "in_process", func(r *remoteCall) { r.ID = "bad" }, errUnavailable},
		{"no-created", time.Time{}, "in_process", nil, errUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := client(t, nil)
			r := remoteCall{ID: "1", Phone: attempt().Phone, Created: tc.created, Status: tc.status, Numbers: []string{"+74951234567"}}
			if tc.mutate != nil {
				tc.mutate(&r)
			}
			out, err := c.admit(attempt(), r, testNow)
			if !errors.Is(err, tc.want) {
				t.Fatal(out, err, tc.want)
			}
			if errors.Is(tc.want, verification.ErrProviderDuplicate) {
				var duplicate *verification.ProviderDuplicateError
				if !errors.As(err, &duplicate) || !duplicate.BusyUntil.Equal(tc.created.Add(verification.RequestTTL+ClockSkewTolerance)) {
					t.Fatal("busy bound was fabricated", err)
				}
			}
			if err == nil && (!out.StartedAt.Equal(tc.created) || !out.ReceivedAt.Equal(testNow) || out.ClockSkewTolerance != time.Second || out.Campaign != "123") {
				t.Fatal("provider timestamps/configuration not preserved", out)
			}
		})
	}
}

func TestUnitProviderPublicRemainderUsesStrictDuration(t *testing.T) {
	for _, remaining := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond, 500 * time.Millisecond} {
		t.Run(remaining.String(), func(t *testing.T) {
			c := client(t, nil)
			a := attempt()
			created := testNow.Add(-verification.RequestTTL + ClockSkewTolerance + remaining)
			a.FirstDispatchAt = created
			r := remoteCall{ID: "1", Phone: a.Phone, Created: created, Status: "in_process", Numbers: []string{"+74951234567"}}
			out, err := c.admit(a, r, testNow)
			if remaining > 0 {
				if err != nil || !out.StartedAt.Equal(created) {
					t.Fatalf("positive subsecond remainder rejected: %v %v", out, err)
				}
			} else if !errors.Is(err, verification.ErrProviderDuplicate) {
				t.Fatalf("nonpositive public deadline must refuse, retaining only the future busy bound: %v", err)
			}
		})
	}
	// An expired external call must not create a past 429 or an invented fence.
	c := client(t, nil)
	a := attempt()
	r := remoteCall{ID: "1", Phone: a.Phone, Created: testNow.Add(-verification.RequestTTL - ClockSkewTolerance), Status: "in_process", Numbers: []string{"+74951234567"}}
	if _, err := c.admit(a, r, testNow); !errors.Is(err, verification.ErrProviderTerminal) {
		t.Fatal(err)
	}
}

func TestUnitCreateMissingStatusUsesAuthenticatedSameCallRead(t *testing.T) {
	for _, mismatch := range []string{"", "id", "phone", "created", "terminal", "read-error"} {
		t.Run(mismatch, func(t *testing.T) {
			posts, byID := 0, 0
			c := client(t, func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "calls_by_phone/") {
					return reply(200, `[]`), nil
				}
				if r.Method == http.MethodPost {
					posts++
					data := strings.Replace(remote("1", testNow, "in_process"), `,"call_status":"in_process"`, "", 1)
					return reply(200, `{"status":"ok","data":`+data+`}`), nil
				}
				byID++
				if !strings.HasSuffix(r.URL.Path, "call_by_id/") || r.URL.Query().Get("call_id") != "1" {
					t.Fatal("wrong authenticated read")
				}
				if mismatch == "read-error" {
					return reply(503, "private response"), nil
				}
				id, created, state := "1", testNow, "in_process"
				if mismatch == "id" {
					id = "2"
				}
				if mismatch == "created" {
					created = created.Add(time.Millisecond)
				}
				if mismatch == "terminal" {
					state = "pincode_ok"
				}
				data := strings.Replace(remote(id, created, state), `,"allowed_phones_for_call":["+74951234567","+74957654321"]`, "", 1)
				if mismatch == "phone" {
					data = strings.ReplaceAll(data, "+79991234567", "+79997654321")
				}
				return reply(200, "["+data+"]"), nil
			})
			out, err := c.Create(t.Context(), attempt())
			if posts != 1 || byID != 1 {
				t.Fatal("wrong number of writes/reads", posts, byID)
			}
			if mismatch == "" {
				if err != nil || out.ID != "1" || len(out.Numbers) != 2 {
					t.Fatal(out, err)
				}
			} else if mismatch == "terminal" {
				if !errors.Is(err, verification.ErrProviderTerminal) {
					t.Fatal(out, err)
				}
			} else if !errors.Is(err, errUnavailable) {
				t.Fatal(out, err)
			}
		})
	}
}

func TestUnitCreateMarkerCampaignAndDeadlineCannotBeBypassed(t *testing.T) {
	for _, mutation := range []string{"no-marker", "no-deadline", "at-deadline", "other-campaign"} {
		t.Run(mutation, func(t *testing.T) {
			c := client(t, func(*http.Request) (*http.Response, error) {
				t.Fatal("HTTP after invalid dispatch metadata")
				return nil, nil
			})
			a := attempt()
			switch mutation {
			case "no-marker":
				a.FirstDispatchAt = time.Time{}
			case "no-deadline":
				a.DispatchNotAfter = time.Time{}
			case "at-deadline":
				a.DispatchNotAfter = testNow
			case "other-campaign":
				a.Campaign = "456"
			}
			if _, err := c.Create(t.Context(), a); !errors.Is(err, errUnavailable) {
				t.Fatal(err)
			}
			if _, err := c.Recover(t.Context(), a); !errors.Is(err, errUnavailable) {
				t.Fatal(err)
			}
		})
	}
	// The shared gate may consume the last admissible dispatch time. Check again
	// after obtaining its slot, before the HTTP transport can receive the phone.
	c := client(t, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatal("POST after dispatch_not_after")
		}
		return reply(200, `[]`), nil
	})
	a, gates := attempt(), 0
	c.gate = gateFunc(func(_ context.Context) error {
		gates++
		if gates == 2 {
			c.now = func() time.Time { return a.DispatchNotAfter }
		}
		return nil
	})
	if _, err := c.Create(t.Context(), a); !errors.Is(err, errUnavailable) || gates != 2 {
		t.Fatal(err, gates)
	}
}

func TestUnitClockDiagnosticsAreBoundedAndContainNoIdentifiers(t *testing.T) {
	registry := prometheus.NewRegistry()
	c, err := New("secret-key", "123", gateFunc(func(context.Context) error { return nil }), nil, registry, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	c.now = func() time.Time { return testNow }
	a := attempt()
	for _, created := range []time.Time{a.FirstDispatchAt.Add(-24 * time.Hour), testNow.Add(24 * time.Hour)} {
		_, _ = c.admit(a, remoteCall{ID: "1", Phone: a.Phone, Created: created, Status: "in_process", Numbers: []string{"+74951234567"}}, testNow)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, family := range families {
		if family.GetName() != "finassist_zvonok_clock_distance_seconds" {
			continue
		}
		for _, metric := range family.GetMetric() {
			seen++
			if len(metric.GetLabel()) != 1 || metric.GetLabel()[0].GetName() != "direction" || metric.GetHistogram().GetSampleCount() != 1 || metric.GetHistogram().GetSampleSum() != 300 {
				t.Fatal("unbounded/private diagnostics", metric)
			}
		}
	}
	if seen != 2 {
		t.Fatal("missing interval diagnostics", seen)
	}
}

func TestIntegrationReadOnlyRecoveryUsesRealAuthenticatedHTTP(t *testing.T) {
	posts, reads := 0, 0
	created := time.Now().UTC().Truncate(time.Millisecond)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
			if err := r.ParseMultipartForm(8192); err != nil || r.PostForm.Get("public_key") != "secret-key" || r.PostForm.Get("campaign_id") != "123" || r.PostForm.Get("phone") != attempt().Phone {
				t.Error("invalid creation parameters", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"status":"ok","data":` + remote("42", created, "in_process") + `}`))
			return
		}
		reads++
		if r.URL.Query().Get("public_key") != "secret-key" {
			t.Error("unauthenticated recovery read")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if posts == 0 {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte("[" + remote("42", created, "in_process") + "]"))
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := transportFunc(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		endpoint := *r.URL
		endpoint.Scheme, endpoint.Host = target.Scheme, target.Host
		copy.URL = &endpoint
		return server.Client().Transport.RoundTrip(copy)
	})
	c, err := New("secret-key", "123", gateFunc(func(context.Context) error { return nil }), transport, prometheus.NewRegistry(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	a := attempt()
	a.FirstDispatchAt, a.CreatedAt, a.DispatchNotAfter = created, created, created.Add(time.Minute)
	first, err := c.Create(t.Context(), a)
	if err != nil || first.ID != "42" {
		t.Fatal(first, err)
	}
	// Losing the initial local completion has no impact on the provider write count.
	recovered, err := c.Recover(t.Context(), a)
	if err != nil || recovered.ID != first.ID || !recovered.StartedAt.Equal(first.StartedAt) || posts != 1 || reads != 2 {
		t.Fatal("recovery changed call or repeated confirm", recovered, err, posts, reads)
	}
	a.CallID = first.ID
	if out, err := c.Recover(t.Context(), a); err != nil || out.ID != first.ID || posts != 1 || reads != 3 {
		t.Fatal("bound recovery wrote to provider", out, err, posts, reads)
	}
}

func TestUnitStatusRequiresImmutableBoundCreationTimestamp(t *testing.T) {
	for _, delta := range []time.Duration{0, time.Nanosecond, time.Microsecond, time.Millisecond, time.Second} {
		t.Run(delta.String(), func(t *testing.T) {
			c := client(t, func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet {
					t.Fatal("status wrote to provider")
				}
				return reply(200, "["+remote("1", testNow.Add(delta), "pincode_ok")+"]"), nil
			})
			a := attempt()
			a.CallID, a.CreatedAt = "1", testNow
			out, err := c.Status(t.Context(), a)
			if delta < time.Microsecond {
				if err != nil || out.State != verification.Verified {
					t.Fatal("stored microsecond precision rejected", out, err)
				}
			} else if !errors.Is(err, verification.ErrProof) {
				t.Fatal("changed created inherited bound confirmation", out, err)
			}
		})
	}
	c := client(t, func(*http.Request) (*http.Response, error) {
		t.Fatal("status queried provider without a bound timestamp")
		return nil, nil
	})
	a := attempt()
	a.CallID, a.CreatedAt = "1", time.Time{}
	if _, err := c.Status(t.Context(), a); !errors.Is(err, verification.ErrProof) {
		t.Fatal(err)
	}
}
