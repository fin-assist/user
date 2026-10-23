package config

import (
	"errors"
	"math"
	"regexp"
	"strings"
	"time"
)

// OutboxConfig sets event routes and a dedicated primary pool.
type OutboxConfig struct {
	TimezoneTargets []string
	DeletionTargets []string
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
	c := OutboxConfig{TimezoneTargets: targets, DeletionTargets: strings.Split(r.str("OUTBOX_DELETION_TARGETS"), ","), MaxConns: int32(maxConns), PollInterval: r.positiveDuration("OUTBOX_POLL_INTERVAL")}
	if err := c.Validate(); err != nil {
		r.errs = append(r.errs, err)
	}
	return c
}

// Validate rejects duplicate/unbounded routes and a pool too small for the dispatcher.
func (c OutboxConfig) Validate() error {
	if len(c.TimezoneTargets) < 1 || len(c.TimezoneTargets) > 64 || len(c.DeletionTargets) < 3 || len(c.DeletionTargets) > 64 || c.PollInterval <= 0 {
		return errors.New("outbox: invalid routes or poll interval")
	}
	union := map[string]bool{}
	for _, targets := range [][]string{c.TimezoneTargets, c.DeletionTargets} {
		seen := map[string]bool{}
		for _, target := range targets {
			if !targetName.MatchString(target) || seen[target] {
				return errors.New("outbox: invalid or duplicate target")
			}
			seen[target] = true
			union[target] = true
		}
	}
	for _, required := range []string{"bank-statement-data", "analytics", "assistant"} {
		found := false
		for _, target := range c.DeletionTargets {
			if target == required {
				found = true
			}
		}
		if !found {
			return errors.New("outbox: required deletion target missing")
		}
	}
	if len(union) > 64 || c.MaxConns < int32(len(union)+3) {
		return errors.New("outbox: insufficient pool capacity")
	}
	return nil
}
