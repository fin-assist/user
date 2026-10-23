package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestOutboxConfiguration(t *testing.T) {
	get, list := env(base())
	c, err := load(get, noFiles, list)
	if err != nil || strings.Join(c.Outbox.TimezoneTargets, ",") != "bank-statement-data,assistant" || strings.Join(c.Outbox.DeletionTargets, ",") != "bank-statement-data,analytics,assistant" || c.Outbox.MaxConns != 8 || c.Outbox.PollInterval != 3*time.Second {
		t.Fatalf("%+v %v", c.Outbox, err)
	}
	for name, values := range map[string][]string{"OUTBOX_TIMEZONE_TARGETS": {"assistant,assistant", "assistant, bank-statement-data", "USER", "a/secret", strings.Repeat("a,", 64) + "z"}, "OUTBOX_DELETION_TARGETS": {"assistant", "bank-statement-data,assistant,other", "bank-statement-data,assistant,assistant", "bank-statement-data,analytics,assistant,USER"}, "OUTBOX_DB_MAX_CONNS": {"0", "4", "-1", "4294967304", "bad"}, "OUTBOX_POLL_INTERVAL": {"0s", "-1s", "bad"}} {
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

func TestOutboxCapacityUsesUnionOfTopics(t *testing.T) {
	c := OutboxConfig{TimezoneTargets: []string{"assistant", "other"}, DeletionTargets: []string{"bank-statement-data", "analytics", "assistant", "other"}, MaxConns: 7, PollInterval: time.Second}
	if err := c.Validate(); err != nil {
		t.Fatal("shared recipients counted twice", err)
	}
	c.MaxConns = 6
	if err := c.Validate(); err == nil {
		t.Fatal("pool has no room for unique recipient")
	}
	c.MaxConns = 100
	c.TimezoneTargets = nil
	for n := 0; n < 64; n++ {
		c.TimezoneTargets = append(c.TimezoneTargets, fmt.Sprintf("peer%d", n))
	}
	if err := c.Validate(); err == nil {
		t.Fatal("union over 64 accepted")
	}
}
