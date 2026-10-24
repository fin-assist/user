// Package zvonok implements the authenticated Zvonok V1 boundary. Remote error
// text and URL errors must never escape this package: both can contain secrets.
package zvonok

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/verification"
)

const baseURL = "https://zvonok.com/manager/cabapi_external/api/v1/"
const maxResponse = 1 << 20

var errUnavailable = verification.ErrProviderUnavailable
var digits = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
var phone = regexp.MustCompile(`^\+[1-9][0-9]{1,14}$`)

// Gate reserves one account-wide API slot in primary storage before network I/O.
type Gate interface{ Wait(context.Context) error }

// Client never retries a write in its transport. Only Recover may repeat confirm
// after looking up this durable attempt and checking the returned call identity.
type Client struct {
	key, campaign string
	http          *http.Client
	gate          Gate
	now           func() time.Time
	requests      *prometheus.CounterVec
	latency       *prometheus.HistogramVec
	balance       prometheus.Gauge
	balanceKnown  prometheus.Gauge
}

// New pins the API endpoint, bounds HTTP work and disables redirects. A supplied
// transport is a trusted assembly seam for HTTP integration tests, never a URL setting.
func New(key, campaign string, gate Gate, transport http.RoundTripper, reg prometheus.Registerer) (*Client, error) {
	if len(key) == 0 || len(key) > 4096 || strings.ContainsAny(key, "\r\n") || !digits.MatchString(campaign) || gate == nil {
		return nil, errors.New("zvonok: invalid credentials, campaign or API gate")
	}
	if transport == nil {
		transport = http.DefaultTransport.(*http.Transport).Clone()
	}
	c := &Client{key: key, campaign: campaign, gate: gate, now: time.Now,
		http:         &http.Client{Transport: transport, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		requests:     prometheus.NewCounterVec(prometheus.CounterOpts{Name: "finassist_zvonok_requests_total", Help: "Zvonok V1 HTTP requests by fixed operation and sanitized outcome."}, []string{"operation", "outcome"}),
		latency:      prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "finassist_zvonok_request_duration_seconds", Help: "Zvonok V1 request latency including the shared API gate.", Buckets: prometheus.DefBuckets}, []string{"operation"}),
		balance:      prometheus.NewGauge(prometheus.GaugeOpts{Name: "finassist_zvonok_balance_rubles", Help: "Last authenticated Zvonok prepaid balance in rubles."}),
		balanceKnown: prometheus.NewGauge(prometheus.GaugeOpts{Name: "finassist_zvonok_balance_last_success_timestamp_seconds", Help: "Time of the last balance observation in an authenticated confirm response; zero means unknown."}),
	}
	for _, metric := range []prometheus.Collector{c.requests, c.latency, c.balance, c.balanceKnown} {
		if err := reg.Register(metric); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Name is the stored provider identity, independent of campaign configuration.
func (*Client) Name() string { return "zvonok" }

// Close releases the client's idle HTTP connections during application shutdown.
func (c *Client) Close() { c.http.CloseIdleConnections() }

type response struct {
	status int
	body   []byte
}

func (c *Client) request(ctx context.Context, operation string, values url.Values, post bool) (response, error) {
	started := c.now()
	outcome := "capacity_unavailable"
	defer func() {
		c.requests.WithLabelValues(operation, outcome).Inc()
		c.latency.WithLabelValues(operation).Observe(c.now().Sub(started).Seconds())
	}()
	if err := c.gate.Wait(ctx); err != nil {
		return response{}, errUnavailable
	}
	values.Set("public_key", c.key)
	endpoint := baseURL + operation + "/"
	method := http.MethodGet
	var body bytes.Buffer
	contentType := ""
	if post {
		method = http.MethodPost
		w := multipart.NewWriter(&body)
		for k, list := range values {
			for _, v := range list {
				if err := w.WriteField(k, v); err != nil {
					return response{}, errUnavailable
				}
			}
		}
		if err := w.Close(); err != nil {
			return response{}, errUnavailable
		}
		contentType = w.FormDataContentType()
	} else {
		// Owner-approved FIN-139 exception: V1 documents read credentials in query.
		// Never log requests, redirects, raw bodies or errors returned by net/http.
		endpoint += "?" + values.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, &body)
	if err != nil {
		return response{}, errUnavailable
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	outcome = "unavailable"
	r, err := c.http.Do(req)
	if err != nil {
		return response{}, errUnavailable
	}
	defer func() { _ = r.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(r.Body, maxResponse+1))
	if err != nil || len(b) > maxResponse {
		return response{}, errUnavailable
	}
	switch {
	case r.StatusCode == 200:
		outcome = "ok"
	case r.StatusCode == 429:
		outcome = "rate_limited"
	case r.StatusCode >= 400 && r.StatusCode < 500:
		outcome = "rejected"
	}
	return response{r.StatusCode, b}, nil
}

type callID string

func (id *callID) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(s) > 0 && s[0] == '"' {
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
	}
	if !digits.MatchString(s) {
		return errUnavailable
	}
	*id = callID(s)
	return nil
}

type remoteCall struct {
	ID           callID     `json:"call_id"`
	Phone        string     `json:"phone"`
	Created      time.Time  `json:"created"`
	Status       string     `json:"call_status"`
	LegacyStatus string     `json:"status"`
	Completed    *time.Time `json:"completed"`
	Balance      string     `json:"balance"`
	Numbers      []string   `json:"allowed_phones_for_call"`
}

func (r remoteCall) state() verification.State {
	s := r.Status
	if s == "" {
		s = r.LegacyStatus
	}
	if s == "pincode_ok" {
		return verification.Verified
	}
	if s == "in_process" || s == "new" || s == "wait" {
		return verification.Pending
	}
	return verification.Expired // unknown states never authorize a confirmation
}

func (r remoteCall) valid() bool {
	return digits.MatchString(string(r.ID)) && phone.MatchString(r.Phone) && !r.Created.IsZero()
}
