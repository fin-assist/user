package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/verification"
)

const startPhoneVerificationEndpoint = "/finassist.user.v1.AuthService/StartPhoneVerification"

// VerificationStarts persists creation ownership in the request on primary.
type VerificationStarts struct{ db *DB }

// NewVerificationStarts shares the caller's primary transaction.
func NewVerificationStarts(db *DB) *VerificationStarts { return &VerificationStarts{db} }

func (r *VerificationStarts) Find(ctx context.Context, key string) (verification.StartRecord, error) {
	var out verification.StartRecord
	var hash, windows []byte
	var id int32
	err := r.db.Writer(ctx).QueryRow(ctx, `SELECT start_key::text,start_request_hash,id,start_reserved_windows FROM phone_verification WHERE start_key=$1`, key).Scan(&out.Key, &hash, &id, &windows)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, verification.ErrMissing
	}
	if err != nil {
		return out, fmt.Errorf("verification start: read attempt: %w", err)
	}
	out, err = decodeStartRecord(out.Key, hash, windows)
	if err != nil {
		return out, err
	}
	out.Request, err = scanVerification(r.db.Writer(ctx).QueryRow(ctx, verificationSQL+` WHERE id=$1`, id))
	return out, err
}

func decodeStartRecord(key string, hash, windows []byte) (verification.StartRecord, error) {
	out := verification.StartRecord{Key: key}
	if len(hash) != 32 {
		return out, errors.New("verification start: invalid stored hash")
	}
	copy(out.Hash[:], hash)
	if err := json.Unmarshal(windows, &out.Windows); err != nil || out.Windows == nil || (len(out.Windows) != 0 && len(out.Windows) != 4) {
		return out, errors.New("verification start: invalid reserved windows")
	}
	return out, nil
}

func (r *VerificationStarts) Forget(ctx context.Context, key string) error {
	if _, ok := Tx(ctx); !ok {
		return errVerificationTx
	}
	// Clear only the old attempt identity; cached resource links are independent.
	_, err := r.db.Writer(ctx).Exec(ctx, `UPDATE phone_verification SET start_key=NULL,start_request_hash=NULL,start_reserved_windows=NULL WHERE start_key=$1`, key)
	if err != nil {
		return fmt.Errorf("verification start: forget attempt: %w", err)
	}
	return nil
}

func (r *VerificationStarts) Claim(ctx context.Context, v verification.StartRecord) (verification.StartRecord, time.Time, error) {
	if _, ok := Tx(ctx); !ok {
		return v, time.Time{}, errVerificationTx
	}
	windows, err := json.Marshal(v.Windows)
	if err != nil {
		return v, time.Time{}, err
	}
	q := r.db.Writer(ctx)
	for range 3 {
		if _, err := q.Exec(ctx, `UPDATE phone_verification SET released_at=$2 WHERE phone=$1 AND released_at IS NULL AND expires_at<=$2`, v.Request.Phone, v.Request.CreatedAt); err != nil {
			return v, time.Time{}, fmt.Errorf("verification start: release expired: %w", err)
		}
		err := q.QueryRow(ctx, `INSERT INTO phone_verification(phone,token_hash,provider,created_at,expires_at,start_key,start_request_hash,start_reserved_windows) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(phone) WHERE released_at IS NULL DO NOTHING RETURNING id`, v.Request.Phone, v.Request.Hash[:], v.Request.Provider, v.Request.CreatedAt, v.Request.ExpiresAt, v.Key, v.Hash[:], windows).Scan(&v.Request.ID)
		if err == nil {
			return v, time.Time{}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return v, time.Time{}, fmt.Errorf("verification start: claim phone: %w", err)
		}
		// Serialize a busy link with deletion's parent lock before its key purge.
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
		return busy, expires, nil
	}
	return v, time.Time{}, errors.New("verification start: phone changed during claim")
}

func (r *VerificationStarts) Complete(ctx context.Context, v verification.StartRecord, c verification.Call, h token.Hash) error {
	if _, ok := Tx(ctx); !ok {
		return errVerificationTx
	}
	tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE phone_verification v SET provider_call_id=$4,token_hash=$5,created_at=$6,expires_at=$7,start_reserved_windows='[]'::jsonb WHERE v.id=$1 AND v.start_key=$2 AND v.start_request_hash=$3 AND v.provider=$8 AND v.token_hash=$9 AND v.provider_call_id IS NULL AND v.verified_at IS NULL AND v.closed_at IS NULL AND v.released_at IS NULL`, v.Request.ID, v.Key, v.Hash[:], c.ID, h[:], c.StartedAt, c.StartedAt.Add(verification.RequestTTL), v.Request.Provider, v.Request.Hash[:])
	if err != nil {
		return fmt.Errorf("verification start: complete: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return verification.ErrMissing
	}
	return nil
}

func (r *VerificationStarts) Reject(ctx context.Context, v verification.StartRecord, now time.Time) error {
	if _, ok := Tx(ctx); !ok {
		return errVerificationTx
	}
	tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE phone_verification v SET released_at=$4,start_key=NULL,start_request_hash=NULL,start_reserved_windows=NULL WHERE v.id=$1 AND v.start_key=$2 AND v.start_request_hash=$3 AND v.provider_call_id IS NULL AND v.verified_at IS NULL`, v.Request.ID, v.Key, v.Hash[:], now)
	if err != nil {
		return fmt.Errorf("verification start: reject: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return verification.ErrMissing
	}
	// Busy keys remain linked through library metadata until phone purge/TTL.
	return nil
}

func (r *VerificationStarts) Duplicate(ctx context.Context, v verification.StartRecord, now time.Time) error {
	if _, ok := Tx(ctx); !ok {
		return errVerificationTx
	}
	tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE phone_verification v SET released_at=$4,start_reserved_windows='[]'::jsonb WHERE v.id=$1 AND v.start_key=$2 AND v.start_request_hash=$3 AND v.provider_call_id IS NULL AND v.verified_at IS NULL`, v.Request.ID, v.Key, v.Hash[:], now)
	if err != nil {
		return fmt.Errorf("verification start: duplicate: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return verification.ErrMissing
	}
	return nil
}
