package config

import (
	"errors"
	"math"
	"regexp"
	"strings"
	"time"
)

// OutboxConfig sets the timezone event routes and a dedicated primary pool.
type OutboxConfig struct {
	TimezoneTargets []string
	MaxConns        int32
	PollInterval    time.Duration
}

var targetName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

func (r *reader) outbox() OutboxConfig {
	targets := strings.Split(r.str("OUTBOX_TIMEZONE_TARGETS"), ",")
	maxConns := r.int("OUTBOX_DB_MAX_CONNS")
	if maxConns < 1 || int64(maxConns) > math.MaxInt32 {
		r.errs = append(r.errs, errors.New("OUTBOX_DB_MAX_CONNS outside 1..2147483647"))
		maxConns = 0
	}
	c := OutboxConfig{TimezoneTargets: targets, MaxConns: int32(maxConns), PollInterval: r.positiveDuration("OUTBOX_POLL_INTERVAL")}
	if err := c.Validate(); err != nil {
		r.errs = append(r.errs, err)
	}
	return c
}

// Validate rejects duplicate/unbounded routes and a pool too small for the dispatcher.
func (c OutboxConfig) Validate() error {
	if len(c.TimezoneTargets) < 1 || len(c.TimezoneTargets) > 64 || c.PollInterval <= 0 || c.MaxConns < int32(len(c.TimezoneTargets)+3) {
		return errors.New("outbox: invalid routes, poll interval or pool capacity")
	}
	seen := map[string]bool{}
	for _, target := range c.TimezoneTargets {
		if !targetName.MatchString(target) || seen[target] {
			return errors.New("outbox: invalid or duplicate target")
		}
		seen[target] = true
	}
	return nil
}
