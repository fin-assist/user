package zvonok

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/fin-assist/user/internal/verification"
)

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
	if json.Unmarshal(r.body, &calls) != nil || len(calls) > 200 {
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
	return url.Values{"campaign_id": {c.campaign}, "phone": {a.Phone}, "from_created_date": {a.CreatedAt.Add(-24 * time.Hour).UTC().Format("2006-01-02 15:04:05")}}
}

// Create checks for a foreign unfinished provider call before adding this attempt.
// The local claim is already committed; it grants no right to adopt older calls.
func (c *Client) Create(ctx context.Context, a verification.Attempt) (verification.Call, error) {
	calls, err := c.lookup(ctx, "phones/calls_by_phone", c.phoneHistory(a))
	if err != nil {
		return verification.Call{}, verification.ErrProviderRejected
	} // no write was attempted
	for _, call := range calls {
		if call.Phone != a.Phone {
			return verification.Call{}, errUnavailable
		}
		if !terminal(call) && call.Created.Add(verification.RequestTTL).After(c.now()) {
			return verification.Call{}, verification.ErrProviderDuplicate
		}
	}
	return c.confirm(ctx, a, "", a.CreatedAt)
}

// Recover first establishes ownership from the durable attempt's call ID or its
// creation interval. Unknown statuses and foreign calls never permit a write.
func (c *Client) Recover(ctx context.Context, a verification.Attempt) (verification.Call, error) {
	operation := "phones/calls_by_phone"
	values := c.phoneHistory(a)
	if a.CallID != "" {
		operation = "phones/call_by_id"
		values = url.Values{"call_id": {a.CallID}}
	}
	calls, err := c.lookup(ctx, operation, values)
	if err != nil {
		return verification.Call{}, errUnavailable
	}
	var own *remoteCall
	owned := 0
	for i := range calls {
		r := &calls[i]
		if r.Phone != a.Phone || r.Created.After(c.now()) {
			return verification.Call{}, errUnavailable
		}
		if a.CallID != "" && string(r.ID) != a.CallID {
			return verification.Call{}, errUnavailable
		}
		if r.Created.Before(a.CreatedAt.Truncate(time.Millisecond)) {
			if !terminal(*r) && r.Created.Add(verification.RequestTTL).After(c.now()) {
				return verification.Call{}, verification.ErrProviderDuplicate
			}
			continue
		}
		owned++
		if own != nil {
			// A recovered replacement can coexist with its terminal predecessor
			// in history after another lost response. Two live calls are ambiguous.
			newer, older := r, own
			if newer.Created.Before(older.Created) {
				newer, older = older, newer
			}
			if newer.Created.Equal(older.Created) || (!terminal(*older) && older.Created.Add(verification.RequestTTL).After(c.now())) {
				return verification.Call{}, errUnavailable
			}
			own = newer
		} else {
			own = r
		}
	}
	if own == nil {
		if a.CallID != "" {
			return verification.Call{}, errUnavailable
		}
		return c.confirm(ctx, a, "", c.now().UTC())
	}
	s := own.Status
	if s == "" {
		s = own.LegacyStatus
	}
	if !terminal(*own) && s != "in_process" && s != "new" && s != "wait" {
		return verification.Call{}, errUnavailable
	}
	if owned > 1 {
		a.CreatedAt = own.Created
	}
	// An unfinished call can become terminal during this operation. confirm checks
	// its returned ID; replacement means a fresh local lifetime and no old proof.
	return c.confirm(ctx, a, string(own.ID), c.now().UTC())
}

func (c *Client) confirm(ctx context.Context, a verification.Attempt, previous string, sent time.Time) (verification.Call, error) {
	r, err := c.request(ctx, "phones/confirm", c.phoneHistory(a), true)
	if err != nil {
		return verification.Call{}, errUnavailable
	}
	if r.status == 429 {
		return verification.Call{}, verification.ErrProviderRejected
	}
	var envelope struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
	}
	if json.Unmarshal(r.body, &envelope) != nil {
		return verification.Call{}, errUnavailable
	}
	if r.status >= 400 && r.status < 500 && envelope.Status == "error" {
		return verification.Call{}, verification.ErrProviderRejected
	}
	if r.status != 200 || envelope.Status != "ok" {
		return verification.Call{}, errUnavailable
	}
	var call remoteCall
	if json.Unmarshal(envelope.Data, &call) != nil || !call.valid() || call.Phone != a.Phone || len(call.Numbers) < 1 || len(call.Numbers) > 1000 || call.Created.After(c.now()) {
		return verification.Call{}, errUnavailable
	}
	if call.Created.Before(a.CreatedAt.Truncate(time.Millisecond)) {
		return verification.Call{}, verification.ErrProviderDuplicate
	}
	for _, n := range call.Numbers {
		if !phone.MatchString(n) {
			return verification.Call{}, errUnavailable
		}
	}
	if balance, err := strconv.ParseFloat(call.Balance, 64); err == nil && !math.IsNaN(balance) && !math.IsInf(balance, 0) && balance >= 0 && balance <= 1e12 {
		c.balance.Set(balance)
		c.balanceKnown.Set(float64(c.now().Unix()))
	}
	started := sent
	if call.Created.Before(sent.Truncate(time.Millisecond)) {
		if previous != "" && string(call.ID) != previous {
			return verification.Call{}, errUnavailable
		}
		// A temporarily missing history response does not prove that confirm made
		// a new call. Never extend that existing call's conservative local TTL.
		started = a.CreatedAt
	}
	if previous != "" && string(call.ID) == previous {
		started = a.CreatedAt
	}
	// No fresh call may predate the operation which was allowed to create it.
	if started.Before(a.CreatedAt) {
		return verification.Call{}, errUnavailable
	}
	return verification.Call{ID: string(call.ID), Phone: a.Phone, StartedAt: started, Numbers: call.Numbers}, nil
}

// Status obtains proof only from the authenticated API and validates both ID and
// phone. Provider status hints supplied by a postback are intentionally absent.
func (c *Client) Status(ctx context.Context, a verification.Attempt) (verification.ProviderStatus, error) {
	if !digits.MatchString(a.CallID) {
		return verification.ProviderStatus{}, errUnavailable
	}
	calls, err := c.lookup(ctx, "phones/call_by_id", url.Values{"call_id": {a.CallID}})
	if err != nil || len(calls) != 1 {
		return verification.ProviderStatus{}, errUnavailable
	}
	r := calls[0]
	if string(r.ID) != a.CallID || r.Phone != a.Phone {
		return verification.ProviderStatus{}, verification.ErrProof
	}
	return verification.ProviderStatus{ID: a.CallID, Phone: a.Phone, State: r.state()}, nil
}
