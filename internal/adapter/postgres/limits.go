package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/fin-assist/user/internal/ratelimit"
)

// Limits stores authentication counters on the primary, in the service transaction.
type Limits struct{ db *DB }

// NewLimits shares the transaction with the caller's other effects.
func NewLimits(db *DB) *Limits { return &Limits{db: db} }

// Lock returns current counts while holding every window until transaction completion.
// The limiter supplies windows in a fixed scope order to avoid lock-order cycles.
func (r *Limits) Lock(ctx context.Context, windows []ratelimit.Window) ([]int, error) {
	tx, ok := r.db.Tx(ctx)
	if !ok {
		return nil, errors.New("rate limits: primary transaction required")
	}
	if len(windows) == 0 {
		return nil, errors.New("rate limits: windows required")
	}
	counts := make([]int, len(windows))
	for i, w := range windows {
		// Different configured durations must not reuse the same row when their
		// starts coincide. Metric labels remain the seven fixed policy names.
		storageScope := string(w.Scope) + ":" + strconv.FormatInt(int64(w.End.Sub(w.Start)/time.Second), 10)
		// An upsert locks an existing row, including the absent-row race. Do not
		// increment yet: another window may already be full.
		err := tx.QueryRow(ctx, `INSERT INTO rate_limit_window(scope,subject,window_start,window_end,hits)
			VALUES ($1,$2,$3,$4,0) ON CONFLICT (scope,subject,window_start)
			DO UPDATE SET hits=rate_limit_window.hits RETURNING hits`, storageScope, w.Subject, w.Start, w.End).Scan(&counts[i])
		if err != nil {
			return nil, fmt.Errorf("lock rate-limit window: %w", err)
		}
	}
	return counts, nil
}

// Increment consumes one attempt in each previously locked window in this transaction.
func (r *Limits) Increment(ctx context.Context, windows []ratelimit.Window) error {
	tx, ok := r.db.Tx(ctx)
	if !ok {
		return errors.New("rate limits: primary transaction required")
	}
	for _, w := range windows {
		storageScope := string(w.Scope) + ":" + strconv.FormatInt(int64(w.End.Sub(w.Start)/time.Second), 10)
		tag, err := tx.Exec(ctx, `UPDATE rate_limit_window SET hits=hits+1 WHERE scope=$1 AND subject=$2 AND window_start=$3`, storageScope, w.Subject, w.Start)
		if err != nil {
			return fmt.Errorf("increment rate-limit window: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return errors.New("rate limits: locked window missing")
		}
	}
	return nil
}

// Cleanup removes at most batch windows ending no later than before. Concurrent
// workers skip each other's rows; active or retained windows remain untouched.
func (r *Limits) Cleanup(ctx context.Context, before time.Time, batch int) (int64, error) {
	if batch < 1 || batch > 10000 {
		return 0, errors.New("rate limits: cleanup batch outside 1..10000")
	}
	tag, err := r.db.Writer(ctx).Exec(ctx, `DELETE FROM rate_limit_window WHERE ctid IN
		(SELECT ctid FROM rate_limit_window WHERE window_end <= $1 ORDER BY window_end LIMIT $2 FOR UPDATE SKIP LOCKED)`, before, batch)
	if err != nil {
		return 0, fmt.Errorf("cleanup rate-limit windows: %w", err)
	}
	return tag.RowsAffected(), nil
}
