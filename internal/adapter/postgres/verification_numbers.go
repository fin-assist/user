package postgres

import (
	"context"
	"errors"
	"time"

	sq "github.com/Masterminds/squirrel"

	"github.com/fin-assist/user/internal/verification"
)

// SavePool shares Start's completion transaction and cannot select a different
// number after the first committed selection.
func (r *Verifications) SavePool(ctx context.Context, id int32, pool []string, selected string) error {
	if _, ok := Tx(ctx); !ok {
		return errVerificationTx
	}
	q, args, err := sq.Update("phone_verification").Set("allowed_phones_for_call", pool).Set("callback_number", selected).Where(sq.Eq{"id": id, "callback_number": nil}).Where("provider_call_id IS NOT NULL").PlaceholderFormat(sq.Dollar).ToSql()
	if err != nil {
		return err
	}
	tag, err := r.db.Writer(ctx).Exec(ctx, q, args...)
	if err != nil {
		return errors.New("verification: save callback pool failed")
	}
	if tag.RowsAffected() != 1 {
		return verification.ErrMissing
	}
	return nil
}

const poolOutcome = "CASE WHEN verified_at IS NOT NULL THEN 'confirmed' WHEN expires_at<=CURRENT_TIMESTAMP THEN 'expired' ELSE 'pending' END"

// PoolOutcomes bounds detail rows to 1024 while retaining exact totals for all
// requests, so rotating pools cannot grow metrics or Go memory without a limit.
func (r *Verifications) PoolOutcomes(ctx context.Context, since time.Time) ([]verification.PoolOutcome, map[string]int64, error) {
	source := sq.Select("callback_number", poolOutcome+" AS outcome").From("phone_verification").Where(sq.GtOrEq{"created_at": since}).Where("callback_number IS NOT NULL")
	q, args, err := sq.Select("callback_number", "outcome", "count(*)").FromSelect(source, "outcomes").GroupBy("GROUPING SETS ((callback_number,outcome), (outcome))").OrderBy("callback_number IS NOT NULL", "count(*) DESC", "callback_number", "outcome").Limit(1027).PlaceholderFormat(sq.Dollar).ToSql()
	if err != nil {
		return nil, nil, err
	}
	rows, err := r.db.Writer(ctx).Query(ctx, q, args...)
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
