package zvonok

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"

	"github.com/fin-assist/user/internal/verification"
)

// maxPhoneHistoryCalls bounds decoded history before ownership checks. The
// response-byte cap still applies; this is a local safety limit, not an API quota.
const maxPhoneHistoryCalls = 200

// phoneHistoryLookback includes a full day before an attempt because the provider
// date filter has no documented timezone; it covers every possible 5-minute holder.
const phoneHistoryLookback = 24 * time.Hour

func (c *Client) lookup(ctx context.Context, operation string, values url.Values) ([]remoteCall, error) {
	r, err := c.request(ctx, operation, values, false)
	if err != nil {
		return nil, errUnavailable
	}
	if r.status == http.StatusBadRequest {
		var e struct{ Status, Data string }
		if json.Unmarshal(r.body, &e) == nil && e.Status == "error" && (e.Data == "Phone by val="+values.Get("phone")+" not found" || e.Data == "Call by id="+values.Get("call_id")+" not found") {
			return nil, nil
		}
	}
	if r.status != http.StatusOK {
		return nil, errUnavailable
	}
	var calls []remoteCall
	if bytes.Equal(bytes.TrimSpace(r.body), []byte("null")) || json.Unmarshal(r.body, &calls) != nil || len(calls) > maxPhoneHistoryCalls {
		return nil, errUnavailable
	}
	for _, call := range calls {
		if !call.valid() {
			return nil, errUnavailable
		}
	}
	return calls, nil
}

func terminal(r remoteCall) bool {
	s := r.Status
	if s == "" {
		s = r.LegacyStatus
	}
	return s == "pincode_ok" || s == "expires" || (r.Completed != nil && !r.Completed.IsZero())
}

// phoneHistory includes a full day before the local attempt. This keeps long-lived
// phone histories bounded without relying on an undocumented timezone for the
// provider's naive date filter; every possible five-minute holder is included.
func (c *Client) phoneHistory(a verification.Attempt) url.Values {
	return url.Values{"campaign_id": {c.campaign}, "phone": {a.Phone}, "from_created_date": {a.CreatedAt.Add(-phoneHistoryLookback).UTC().Format("2006-01-02 15:04:05")}}
}

// ClockSkewTolerance is an adapter bound, independent of timestamp precision.
// Public verification ends early by this bound; known busy occupancy ends late.
const ClockSkewTolerance = time.Second

func pending(r remoteCall) bool {
	status := r.Status
	if status == "" {
		status = r.LegacyStatus
	}
	return status == "new" || status == "wait" || status == "in_process"
}

func validPool(numbers []string) bool {
	if len(numbers) < 1 || len(numbers) > verification.MaxCallbackPoolNumbers {
		return false
	}
	seen := make(map[string]struct{}, len(numbers))
	for _, number := range numbers {
		if !phone.MatchString(number) {
			return false
		}
		if _, exists := seen[number]; exists {
			return false
		}
		seen[number] = struct{}{}
	}
	return true
}

// Create runs once after the durable dispatch marker has committed. The history
// preflight can refuse a known holder but cannot adopt it or authorize a retry.
func (c *Client) Create(ctx context.Context, a verification.Attempt) (verification.Call, error) {
	if a.Campaign != c.campaign || a.FirstDispatchAt.IsZero() || a.DispatchNotAfter.IsZero() || !c.now().Before(a.DispatchNotAfter) {
		return verification.Call{}, errUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, a.DispatchNotAfter.Sub(c.now()))
	defer cancel()
	calls, err := c.lookup(ctx, "phones/calls_by_phone", c.phoneHistory(a))
	if err != nil {
		return verification.Call{}, verification.ErrProviderRejected
	}
	received := c.now().UTC()
	var busyUntil time.Time
	for _, call := range calls {
		if call.Phone != a.Phone || call.Created.After(received.Add(ClockSkewTolerance)) {
			return verification.Call{}, verification.ErrProviderRejected
		}
		if !terminal(call) && !pending(call) {
			return verification.Call{}, verification.ErrProviderRejected
		}
		if !terminal(call) && pending(call) {
			until := call.Created.Add(verification.RequestTTL + ClockSkewTolerance)
			if until.After(received) && until.After(busyUntil) {
				busyUntil = until
			}
		}
	}
	if !busyUntil.IsZero() {
		return verification.Call{}, &verification.ProviderDuplicateError{BusyUntil: busyUntil}
	}
	if !c.now().Before(a.DispatchNotAfter) || ctx.Err() != nil {
		return verification.Call{}, verification.ErrProviderRejected
	}
	return c.confirm(ctx, a)
}

// Recover cannot POST even if no call is found. Absence in a read is not proof
// that the original dispatch cannot appear later. Missing pools stay unknown.
func (c *Client) Recover(ctx context.Context, a verification.Attempt) (verification.Call, error) {
	if a.Campaign != c.campaign || a.FirstDispatchAt.IsZero() || a.DispatchNotAfter.IsZero() || !c.now().Before(a.DispatchNotAfter) {
		return verification.Call{}, errUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, a.DispatchNotAfter.Sub(c.now()))
	defer cancel()
	operation, values := "phones/calls_by_phone", c.phoneHistory(a)
	if a.CallID != "" {
		if !digits.MatchString(a.CallID) {
			return verification.Call{}, errUnavailable
		}
		operation, values = "phones/call_by_id", url.Values{"call_id": {a.CallID}}
	}
	calls, err := c.lookup(ctx, operation, values)
	if err != nil || len(calls) == 0 {
		return verification.Call{}, errUnavailable
	}
	received := c.now().UTC()
	var candidate *remoteCall
	var busyUntil time.Time
	sawOwnTerminal := false
	for i := range calls {
		call := &calls[i]
		if call.Phone != a.Phone || (a.CallID != "" && string(call.ID) != a.CallID) || call.Created.After(received.Add(ClockSkewTolerance)) {
			return verification.Call{}, errUnavailable
		}
		if !terminal(*call) && !pending(*call) {
			return verification.Call{}, errUnavailable
		}
		if terminal(*call) {
			if a.CallID != "" || !call.Created.Before(a.FirstDispatchAt.Truncate(time.Millisecond).Add(-ClockSkewTolerance)) {
				sawOwnTerminal = true
			}
			continue
		}
		if call.Created.Before(a.FirstDispatchAt.Truncate(time.Millisecond).Add(-ClockSkewTolerance)) {
			until := call.Created.Add(verification.RequestTTL + ClockSkewTolerance)
			if until.After(received) && until.After(busyUntil) {
				busyUntil = until
			}
			continue
		}
		if candidate != nil {
			return verification.Call{}, errUnavailable
		}
		candidate = call
	}
	if !busyUntil.IsZero() {
		return verification.Call{}, &verification.ProviderDuplicateError{BusyUntil: busyUntil}
	}
	if candidate == nil {
		if sawOwnTerminal {
			return verification.Call{}, verification.ErrProviderTerminal
		}
		return verification.Call{}, errUnavailable
	}
	return c.admit(a, *candidate, received)
}

func (c *Client) confirm(ctx context.Context, a verification.Attempt) (verification.Call, error) {
	r, err := c.request(ctx, "phones/confirm", url.Values{"campaign_id": {c.campaign}, "phone": {a.Phone}}, true, a.DispatchNotAfter)
	received := c.now().UTC()
	if err != nil {
		return verification.Call{}, errUnavailable
	}
	if r.status == http.StatusTooManyRequests {
		return verification.Call{}, verification.ErrProviderRejected
	}
	var envelope struct {
		Status string
		Data   json.RawMessage
	}
	if json.Unmarshal(r.body, &envelope) != nil {
		return verification.Call{}, errUnavailable
	}
	if r.status >= 400 && r.status < 500 && envelope.Status == "error" {
		return verification.Call{}, verification.ErrProviderRejected
	}
	if r.status != http.StatusOK || envelope.Status != "ok" {
		return verification.Call{}, errUnavailable
	}
	var call remoteCall
	if json.Unmarshal(envelope.Data, &call) != nil || !call.valid() || call.Phone != a.Phone {
		return verification.Call{}, errUnavailable
	}
	// V1's confirm schema need not contain a status. A separate authenticated read
	// may establish it for exactly this ID/created/phone, using only its own pool.
	if call.Status == "" && call.LegacyStatus == "" && !terminal(call) {
		checked, err := c.lookup(ctx, "phones/call_by_id", url.Values{"call_id": {string(call.ID)}})
		if err != nil || len(checked) != 1 || checked[0].ID != call.ID || checked[0].Phone != call.Phone || !checked[0].Created.Equal(call.Created) {
			return verification.Call{}, errUnavailable
		}
		originalPool := call.Numbers
		call = checked[0]
		if len(call.Numbers) == 0 {
			call.Numbers = originalPool
		}
		// The initial response's receive instant remains the admission upper bound.
	}
	return c.admit(a, call, received)
}

func (c *Client) admit(a verification.Attempt, call remoteCall, received time.Time) (verification.Call, error) {
	lower := a.FirstDispatchAt.Truncate(time.Millisecond)
	if !call.valid() || call.Phone != a.Phone || a.FirstDispatchAt.IsZero() || received.Before(a.FirstDispatchAt) {
		return verification.Call{}, errUnavailable
	}
	if call.Created.Before(lower) {
		c.observeClockDistance("before_dispatch", lower.Sub(call.Created))
	}
	if call.Created.After(received) {
		c.observeClockDistance("after_receive", call.Created.Sub(received))
	}
	if call.Created.After(received.Add(ClockSkewTolerance)) {
		return verification.Call{}, errUnavailable
	}
	if terminal(call) {
		return verification.Call{}, verification.ErrProviderTerminal
	}
	if !pending(call) {
		return verification.Call{}, errUnavailable
	}
	busyUntil := call.Created.Add(verification.RequestTTL + ClockSkewTolerance)
	if call.Created.Before(lower.Add(-ClockSkewTolerance)) || !call.Created.Add(verification.RequestTTL-ClockSkewTolerance).After(c.now()) {
		if busyUntil.After(c.now()) {
			return verification.Call{}, &verification.ProviderDuplicateError{BusyUntil: busyUntil}
		}
		return verification.Call{}, verification.ErrProviderTerminal
	}
	if !validPool(call.Numbers) {
		return verification.Call{}, errUnavailable
	}
	return verification.Call{ID: string(call.ID), Phone: call.Phone, StartedAt: call.Created, Numbers: call.Numbers,
		Campaign: c.campaign, ReceivedAt: received, ClockSkewTolerance: ClockSkewTolerance, State: verification.Pending}, nil
}

func (c *Client) observeClockDistance(direction string, distance time.Duration) {
	// Bound observations as well as bucket/cardinality, and never label identities.
	seconds := distance.Seconds()
	if seconds > 300 {
		seconds = 300
	}
	c.clockDistance.WithLabelValues(direction).Observe(seconds)
}

// Status obtains proof only from the authenticated API and validates both ID and
// phone. Provider status hints supplied by a postback are intentionally absent.
func (c *Client) Status(ctx context.Context, a verification.Attempt) (verification.ProviderStatus, error) {
	if a.Campaign != c.campaign || !digits.MatchString(a.CallID) {
		return verification.ProviderStatus{}, errUnavailable
	}
	if a.CreatedAt.IsZero() {
		return verification.ProviderStatus{}, verification.ErrProof
	}
	calls, err := c.lookup(ctx, "phones/call_by_id", url.Values{"call_id": {a.CallID}})
	if err != nil || len(calls) != 1 {
		return verification.ProviderStatus{}, errUnavailable
	}
	r := calls[0]
	// PostgreSQL stores microseconds. The provider's millisecond timestamp must
	// still identify the bound call; skew tolerance belongs only to admission.
	if string(r.ID) != a.CallID || r.Phone != a.Phone || !r.Created.Truncate(time.Microsecond).Equal(a.CreatedAt.Truncate(time.Microsecond)) {
		return verification.ProviderStatus{}, verification.ErrProof
	}
	return verification.ProviderStatus{ID: a.CallID, Phone: a.Phone, State: r.state()}, nil
}
