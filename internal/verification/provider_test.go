package verification

import (
	"strings"
	"testing"
	"time"
)

func TestUnitCheckedProviderResponse(t *testing.T) {
	now := time.Date(2026, 10, 23, 22, 0, 0, 0, time.UTC)
	a := Attempt{ID: 1, Phone: "+79001234567", CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(4 * time.Minute), FirstDispatchAt: now.Add(-time.Minute), DispatchNotAfter: now.Add(4 * time.Minute)}
	good := func() Call {
		return Call{ID: "call-1", Phone: a.Phone, StartedAt: a.CreatedAt, State: Pending, ReceivedAt: now, Numbers: []string{"+74951234567", "+18005550123"}}
	}
	if err := validateCall(a, good(), now); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Call){
		"foreign phone": func(c *Call) { c.Phone = "+79007654321" },
		"empty ID":      func(c *Call) { c.ID = "" },
		"long ID":       func(c *Call) { c.ID = strings.Repeat("x", 129) },
		"old call":      func(c *Call) { c.StartedAt = a.CreatedAt.Add(-time.Second) },
		"future":        func(c *Call) { c.StartedAt = now.Add(time.Second) },
		"empty pool":    func(c *Call) { c.Numbers = nil },
		"large pool":    func(c *Call) { c.Numbers = make([]string, 1001) },
		"bad number":    func(c *Call) { c.Numbers = []string{"+012345"} },
	} {
		t.Run(name, func(t *testing.T) {
			c := good()
			mutate(&c)
			if validateCall(a, c, now) == nil {
				t.Fatal("invalid response accepted")
			}
		})
	}
	a.CallID = "call-1"
	c := good()
	c.StartedAt = now
	if validateCall(a, c, now) == nil {
		t.Fatal("same call lifetime extended")
	}
	c.ID = "replacement"
	if err := validateCall(a, c, now); err == nil {
		t.Fatal("replacement accepted for existing binding")
	}
	c = good()
	if validateCall(a, c, a.ExpiresAt) == nil {
		t.Fatal("expired call accepted")
	}
}

func TestUnitCallbackNumberFormat(t *testing.T) {
	for _, s := range []string{"", "+1", "123", "+01", "+7a", "+1234567890123456", "+7\n"} {
		if callbackNumber(s) {
			t.Fatalf("accepted %q", s)
		}
	}
	for _, s := range []string{"+12", "+123456789012345", "+74951234567"} {
		if !callbackNumber(s) {
			t.Fatalf("rejected %q", s)
		}
	}
}
