package config

import (
	"strings"
	"testing"
	"time"
)

func TestOutboxConfiguration(t *testing.T) {
	get, list := env(base())
	c, err := load(get, noFiles, list)
	if err != nil || strings.Join(c.Outbox.TimezoneTargets, ",") != "bank-statement-data,assistant" || c.Outbox.MaxConns != 8 || c.Outbox.PollInterval != 3*time.Second {
		t.Fatalf("%+v %v", c.Outbox, err)
	}
	for name, values := range map[string][]string{"OUTBOX_TIMEZONE_TARGETS": {"assistant,assistant", "assistant, bank-statement-data", "USER", "a/secret", strings.Repeat("a,", 64) + "z"}, "OUTBOX_DB_MAX_CONNS": {"0", "4", "-1", "4294967304", "bad"}, "OUTBOX_POLL_INTERVAL": {"0s", "-1s", "bad"}} {
		for _, v := range values {
			vars := base()
			vars[name] = v
			get, list := env(vars)
			if _, err := load(get, noFiles, list); err == nil {
				t.Fatalf("%s=%q accepted", name, v)
			}
		}
	}
	c.Outbox.TimezoneTargets = nil
	if err := c.Outbox.Validate(); err == nil {
		t.Fatal("empty recipients accepted")
	}
}
