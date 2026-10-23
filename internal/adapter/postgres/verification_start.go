package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/user/internal/ratelimit"
	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/verification"
)

// VerificationStarts persists creation ownership and deletion links on primary.
type VerificationStarts struct{ db *DB }

func NewVerificationStarts(db *DB) *VerificationStarts { return &VerificationStarts{db} }

func (r *VerificationStarts) Find(ctx context.Context, key string) (verification.StartRecord, error) {
	var out verification.StartRecord
	var hash, windows []byte
	var id int32
	err := r.db.Writer(ctx).QueryRow(ctx, `SELECT idempotency_key::text,request_hash,verification_id,reserved_windows FROM phone_verification_start WHERE idempotency_key=$1`, key).Scan(&out.Key, &hash, &id, &windows)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, verification.ErrMissing
	}
	if err != nil {
		return out, fmt.Errorf("verification start: read key: %w", err)
	}
	if len(hash) != 32 {
		return out, errors.New("verification start: invalid stored hash")
	}
	copy(out.Hash[:], hash)
	if err = json.Unmarshal(windows, &out.Windows); err != nil || out.Windows == nil || (len(out.Windows) != 0 && len(out.Windows) != 4) {
		return out, errors.New("verification start: invalid reserved windows")
	}
	out.Request, err = scanVerification(r.db.Writer(ctx).QueryRow(ctx, verificationSQL+` WHERE id=$1`, id))
	return out, err
}
func (r *VerificationStarts) Forget(ctx context.Context, key string) error {
	if _, ok := Tx(ctx); !ok {
		return errVerificationTx
	}
	// Parent precedes child, also when cleanup concurrently cascades the link.
	var id int32
	err := r.db.Writer(ctx).QueryRow(ctx, `SELECT v.id FROM phone_verification v JOIN phone_verification_start s ON s.verification_id=v.id WHERE s.idempotency_key=$1 FOR UPDATE OF v`, key).Scan(&id)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("verification start: lock key parent: %w", err)
	}
	_, err = r.db.Writer(ctx).Exec(ctx, `DELETE FROM phone_verification_start WHERE idempotency_key=$1`, key)
	if err != nil {
		return fmt.Errorf("verification start: forget key: %w", err)
	}
	return nil
}
func (r *VerificationStarts) link(ctx context.Context, v verification.StartRecord) error {
	windows := v.Windows
	if windows == nil {
		windows = []ratelimit.Window{}
	}
	b, err := json.Marshal(windows)
	if err != nil {
		return err
	}
	_, err = r.db.Writer(ctx).Exec(ctx, `INSERT INTO phone_verification_start(idempotency_key,request_hash,verification_id,reserved_windows) VALUES($1,$2,$3,$4)`, v.Key, v.Hash[:], v.Request.ID, b)
	if err != nil {
		return fmt.Errorf("verification start: link key: %w", err)
	}
	return nil
}

func (r *VerificationStarts) Claim(ctx context.Context, v verification.StartRecord) (verification.StartRecord, time.Time, error) {
	if _, ok := Tx(ctx); !ok {
		return v, time.Time{}, errVerificationTx
	}
	q := r.db.Writer(ctx)
	for range 3 {
		if _, err := q.Exec(ctx, `UPDATE phone_verification SET released_at=$2 WHERE phone=$1 AND released_at IS NULL AND expires_at<=$2`, v.Request.Phone, v.Request.CreatedAt); err != nil {
			return v, time.Time{}, fmt.Errorf("verification start: release expired: %w", err)
		}
		err := q.QueryRow(ctx, `INSERT INTO phone_verification(phone,token_hash,provider,created_at,expires_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT(phone) WHERE released_at IS NULL DO NOTHING RETURNING id`, v.Request.Phone, v.Request.Hash[:], v.Request.Provider, v.Request.CreatedAt, v.Request.ExpiresAt).Scan(&v.Request.ID)
		if err == nil {
			return v, time.Time{}, r.link(ctx, v)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return v, time.Time{}, fmt.Errorf("verification start: claim phone: %w", err)
		}
		// The holder can disappear during concurrent account deletion. FOR KEY SHARE
		// protects the FK until this short transaction commits; no external call here.
		var holder int32
		var expires time.Time
		err = q.QueryRow(ctx, `SELECT id,expires_at FROM phone_verification WHERE phone=$1 AND released_at IS NULL FOR KEY SHARE`, v.Request.Phone).Scan(&holder, &expires)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return v, time.Time{}, fmt.Errorf("verification start: busy phone: %w", err)
		}
		busy := v
		busy.Request.ID = holder
		busy.Windows = nil
		return busy, expires, r.link(ctx, busy)
	}
	return v, time.Time{}, errors.New("verification start: phone changed during claim")
}
func (r *VerificationStarts) Complete(ctx context.Context, v verification.StartRecord, c verification.Call, h token.Hash) error {
	if _, ok := Tx(ctx); !ok {
		return errVerificationTx
	}
	q := r.db.Writer(ctx)
	tag, err := q.Exec(ctx, `UPDATE phone_verification v SET provider_call_id=$4,token_hash=$5,created_at=$6,expires_at=$7 WHERE v.id=$1 AND v.provider=$8 AND v.token_hash=$9 AND v.provider_call_id IS NULL AND v.verified_at IS NULL AND v.closed_at IS NULL AND v.released_at IS NULL AND EXISTS(SELECT 1 FROM phone_verification_start s WHERE s.verification_id=v.id AND s.idempotency_key=$2 AND s.request_hash=$3)`, v.Request.ID, v.Key, v.Hash[:], c.ID, h[:], c.StartedAt, c.StartedAt.Add(verification.RequestTTL), v.Request.Provider, v.Request.Hash[:])
	if err != nil {
		return fmt.Errorf("verification start: complete: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return verification.ErrMissing
	}
	_, err = q.Exec(ctx, `UPDATE phone_verification_start SET reserved_windows='[]'::jsonb WHERE idempotency_key=$1`, v.Key)
	if err != nil {
		return fmt.Errorf("verification start: clear reservation: %w", err)
	}
	return nil
}
func (r *VerificationStarts) Reject(ctx context.Context, v verification.StartRecord, now time.Time) error {
	if _, ok := Tx(ctx); !ok {
		return errVerificationTx
	}
	tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE phone_verification v SET released_at=$4 WHERE v.id=$1 AND v.provider_call_id IS NULL AND v.verified_at IS NULL AND EXISTS(SELECT 1 FROM phone_verification_start s WHERE s.verification_id=v.id AND s.idempotency_key=$2 AND s.request_hash=$3)`, v.Request.ID, v.Key, v.Hash[:], now)
	if err != nil {
		return fmt.Errorf("verification start: reject: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return verification.ErrMissing
	}
	// Busy refusals may reference this row. Preserve their deletion links while
	// forgetting only the rejected owner's key; its library reservation was
	// released atomically by the caller.
	return r.Forget(ctx, v.Key)
}
func (r *VerificationStarts) Duplicate(ctx context.Context, v verification.StartRecord, now time.Time) error {
	if _, ok := Tx(ctx); !ok {
		return errVerificationTx
	}
	tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE phone_verification v SET released_at=$4 WHERE v.id=$1 AND v.provider_call_id IS NULL AND v.verified_at IS NULL AND EXISTS(SELECT 1 FROM phone_verification_start s WHERE s.verification_id=v.id AND s.idempotency_key=$2 AND s.request_hash=$3)`, v.Request.ID, v.Key, v.Hash[:], now)
	if err != nil {
		return fmt.Errorf("verification start: duplicate: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return verification.ErrMissing
	}
	_, err = r.db.Writer(ctx).Exec(ctx, `UPDATE phone_verification_start SET reserved_windows='[]'::jsonb WHERE idempotency_key=$1`, v.Key)
	if err != nil {
		return fmt.Errorf("verification start: clear duplicate reservation: %w", err)
	}
	return nil
}
