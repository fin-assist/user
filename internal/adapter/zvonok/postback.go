package zvonok

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/verification"
)

// Callback verifies a local call through the authenticated provider API.
type Callback interface {
	CheckCall(context.Context, string) error
}

// Postback accepts the provider's fixed IP, optionally behind explicitly trusted
// proxies. Forwarding headers from every other peer are ignored.
type Postback struct {
	callback Callback
	proxies  []netip.Prefix
	requests *prometheus.CounterVec
}

// NewPostback exposes only the Zvonok route; it never logs a request URL or body.
func NewPostback(callback Callback, proxies []netip.Prefix, reg prometheus.Registerer) (*Postback, error) {
	if callback == nil {
		return nil, errors.New("zvonok: callback required")
	}
	h := &Postback{callback: callback, proxies: append([]netip.Prefix(nil), proxies...), requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "finassist_zvonok_postbacks_total", Help: "Postback outcomes after source-IP and authenticated status checks."}, []string{"result"})}
	if err := reg.Register(h.requests); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *Postback) trusted(ip netip.Addr) bool {
	for _, p := range h.proxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func (h *Postback) source(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	ip = ip.Unmap()
	if !h.trusted(ip) {
		return ip
	}
	// Repeated header lines form one list (RFC 9110, 5.3): a proxy may append its
	// entry as a new line, so the rightmost hop is in the last line, not the first.
	chain := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	if len(chain) > 16 {
		return netip.Addr{}
	}
	for i := len(chain) - 1; i >= 0; i-- {
		ip, err = netip.ParseAddr(strings.TrimSpace(chain[i]))
		if err != nil {
			return netip.Addr{}
		}
		ip = ip.Unmap()
		if !h.trusted(ip) {
			return ip
		}
	}
	return netip.Addr{}
}

// ServeHTTP acknowledges a verified event only after common confirmation commits.
func (h *Postback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	result := "invalid"
	defer func() { h.requests.WithLabelValues(result).Inc() }()
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet || r.URL.Path != "/callbacks/zvonok" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if h.source(r) != netip.MustParseAddr("193.232.64.150") {
		result = "foreign_ip"
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if len(r.URL.RawQuery) > 8192 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	query := r.URL.Query()
	id := query.Get("ct_call_id")
	if !digits.MatchString(id) || len(query["ct_call_id"]) != 1 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	err := h.callback.CheckCall(ctx, id)
	switch {
	case err == nil:
		result = "accepted"
		w.WriteHeader(http.StatusOK)
	case errors.Is(err, verification.ErrMissing):
		result = "unknown_call"
		w.WriteHeader(http.StatusNotFound)
	case errors.Is(err, verification.ErrProof), errors.Is(err, verification.ErrUnconfirmed):
		result = "unconfirmed"
		w.WriteHeader(http.StatusBadRequest)
	default:
		result = "unavailable"
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}
