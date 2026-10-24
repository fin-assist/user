package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/fin-assist/user/internal/ratelimit"
)

// ZvonokGate shares the API account's twenty-per-second allowance across all
// replicas/campaigns. Only the key digest is stored; no credential enters SQL.
type ZvonokGate struct {
	db      *DB
	subject string
}

// NewZvonokGate reuses the service's bounded-window counters, not a new table.
func NewZvonokGate(db *DB, key string) *ZvonokGate {
	h := sha256.Sum256([]byte(key))
	return &ZvonokGate{db: db, subject: hex.EncodeToString(h[:])}
}

// Wait reserves before HTTP and commits without retaining a DB connection during
// network I/O or backoff. A cancelled wait never invokes the remote operation.
func (g *ZvonokGate) Wait(ctx context.Context) error {
	store := NewLimits(g.db)
	for {
		start := time.Now().UTC().Truncate(time.Second)
		windows := []ratelimit.Window{{Scope: "zvonok_api", Subject: g.subject, Start: start, End: start.Add(time.Second), Limit: 20}}
		allowed := false
		err := g.db.Run(ctx, func(ctx context.Context) error {
			counts, err := store.Lock(ctx, windows)
			if err != nil {
				return err
			}
			if counts[0] >= 20 {
				return nil
			}
			if err = store.Increment(ctx, windows); err != nil {
				return err
			}
			allowed = true
			return nil
		})
		if err != nil {
			return err
		}
		if allowed && time.Now().Before(start.Add(time.Second)) {
			return nil
		}
		t := time.NewTimer(time.Until(start.Add(time.Second)))
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}
