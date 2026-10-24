package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/fin-assist/user/internal/verification"
)

// maxCallbackOutcomeDetails caps returned and logged number/outcome groups per
// report. This diagnostic capacity is independent of totals and metric labels;
// it does not limit how many source rows PostgreSQL must aggregate.
const maxCallbackOutcomeDetails = 1024

// SavePool shares Start's completion transaction and cannot select a different
// number after the first committed selection.
func (r *Verifications) SavePool(ctx context.Context, id int32, pool []string, selected string) error {
	if _, ok := Tx(ctx); !ok {
		return errVerificationTx
	}
	tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE phone_verification
		SET allowed_phones_for_call=$1, callback_number=$2
		WHERE id=$3 AND callback_number IS NULL AND provider_call_id IS NOT NULL`, pool, selected, id)
	if err != nil {
		return errors.New("verification: save callback pool failed")
	}
	if tag.RowsAffected() != 1 {
		return verification.ErrMissing
	}
	return nil
}

// PoolOutcomes bounds detail rows to 1024 while retaining exact totals for all
// requests, so rotating pools cannot grow metrics or Go memory without a limit.
func (r *Verifications) PoolOutcomes(ctx context.Context, since time.Time) ([]verification.PoolOutcome, map[string]int64, error) {
	rows, err := r.db.Writer(ctx).Query(ctx, `WITH outcomes AS (
		SELECT callback_number, outcome, count(*) AS requests
		FROM (
			SELECT callback_number,
				CASE WHEN verified_at IS NOT NULL THEN 'confirmed'
					WHEN expires_at<=CURRENT_TIMESTAMP THEN 'expired'
					ELSE 'pending' END AS outcome
			FROM phone_verification
			WHERE created_at >= $1 AND callback_number IS NOT NULL
		) AS requests
		GROUP BY GROUPING SETS ((callback_number,outcome), (outcome))
	)
	SELECT callback_number, outcome, requests
	FROM (
		SELECT callback_number, outcome, requests FROM outcomes WHERE callback_number IS NULL
		UNION ALL
		(SELECT callback_number, outcome, requests FROM outcomes WHERE callback_number IS NOT NULL
			ORDER BY requests DESC, callback_number, outcome LIMIT $2)
	) AS bounded
	ORDER BY callback_number IS NOT NULL, requests DESC, callback_number, outcome`, since, maxCallbackOutcomeDetails)
	if err != nil {
		return nil, nil, errors.New("verification: read callback outcomes failed")
	}
	defer rows.Close()
	var out []verification.PoolOutcome
	totals := map[string]int64{}
	for rows.Next() {
		var number *string
		var state string
		var count int64
		if err = rows.Scan(&number, &state, &count); err != nil {
			return nil, nil, err
		}
		if number == nil {
			totals[state] = count
		} else {
			out = append(out, verification.PoolOutcome{Number: *number, State: state, Count: count})
		}
	}
	return out, totals, rows.Err()
}
