package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/verification"
)

// Verifications owns primary-only reads, state transitions and transactional NOTIFY.
type Verifications struct{ db *DB }

func NewVerifications(db *DB) *Verifications { return &Verifications{db: db} }

const verificationSQL = `SELECT id,phone,token_hash,provider,COALESCE(provider_call_id,''),created_at,expires_at,verified_at,COALESCE(is_new_user,false),COALESCE(user_id::text,''),registration_token_hash,result_ciphertext,COALESCE(result_key_id,''),closed_at IS NOT NULL,released_at IS NOT NULL FROM phone_verification`

func (r *Verifications) ByToken(ctx context.Context, h token.Hash) (verification.Request, error) {
	return scanVerification(r.db.Writer(ctx).QueryRow(ctx, verificationSQL+` WHERE token_hash=$1`, h[:]))
}
func (r *Verifications) ByCall(ctx context.Context, provider, call string) (verification.Request, error) {
	return scanVerification(r.db.Writer(ctx).QueryRow(ctx, verificationSQL+` WHERE provider=$1 AND provider_call_id=$2`, provider, call))
}

// Lock follows the user guard and user row, if any; shared locks serialize result
// delivery with consumption without retaining a transaction for the open stream.
func (r *Verifications) Lock(ctx context.Context, id int32, write bool) (verification.Request, error) {
	if _, ok := Tx(ctx); !ok {
		return verification.Request{}, errVerificationTx
	}
	clause := ` FOR SHARE`
	if write {
		clause = ` FOR UPDATE`
	}
	return scanVerification(r.db.Writer(ctx).QueryRow(ctx, verificationSQL+` WHERE id=$1`+clause, id))
}

var errVerificationTx = errors.New("verification: primary transaction required")

func scanVerification(row pgx.Row) (verification.Request, error) {
	var r verification.Request
	var hash, registration []byte
	err := row.Scan(&r.ID, &r.Phone, &hash, &r.Provider, &r.CallID, &r.CreatedAt, &r.ExpiresAt, &r.VerifiedAt, &r.IsNewUser, &r.UserID, &registration, &r.Ciphertext, &r.KeyID, &r.Closed, &r.Released)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, verification.ErrMissing
	}
	if err != nil {
		return r, fmt.Errorf("verification: read: %w", err)
	}
	if len(hash) != len(r.Hash) {
		return r, errors.New("verification: invalid stored hash")
	}
	copy(r.Hash[:], hash)
	if registration != nil {
		if len(registration) != 32 {
			return r, errors.New("verification: invalid stored registration hash")
		}
		h := token.Hash(registration)
		r.RegistrationHash = &h
	}
	return r, nil
}

// Verify writes the complete immutable result and its wakeup in the same commit.
func (r *Verifications) Verify(ctx context.Context, v verification.Request, now time.Time, o verification.Outcome, registration *token.Hash, ciphertext []byte, key string) error {
	if _, ok := Tx(ctx); !ok {
		return errVerificationTx
	}
	var registrationBytes []byte
	if registration != nil {
		registrationBytes = registration[:]
	}
	tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE phone_verification SET verified_at=$2,is_new_user=$3,user_id=NULLIF($4,'')::uuid,registration_token_hash=$5,result_ciphertext=$6,result_key_id=$7 WHERE id=$1 AND verified_at IS NULL AND closed_at IS NULL AND released_at IS NULL AND provider_call_id IS NOT NULL AND created_at<=$2 AND expires_at>$2`, v.ID, now, o.IsNewUser, o.UserID, registrationBytes, ciphertext, key)
	if err != nil {
		return fmt.Errorf("verification: confirm: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return verification.ErrMissing
	}
	_, err = r.db.Writer(ctx).Exec(ctx, `SELECT pg_notify('phone_verification',$1)`, strconv.FormatInt(int64(v.ID), 10))
	if err != nil {
		return fmt.Errorf("verification: notify: %w", err)
	}
	return nil
}

// Cleanup wipes expired secrets, releases expired holders and removes old requests
// in bounded SKIP LOCKED batches. Live replay results survive even inconsistent ages.
func (r *Verifications) CleanupResults(ctx context.Context, now time.Time, batch int) (int64, error) {
	if _, ok := Tx(ctx); !ok {
		return 0, errVerificationTx
	}
	if batch < 1 || batch > 1000 {
		return 0, errors.New("verification: invalid cleanup batch")
	}
	q := r.db.Writer(ctx)
	wiped, err := q.Exec(ctx, `UPDATE phone_verification SET result_ciphertext=NULL,result_key_id=NULL WHERE id IN (SELECT id FROM phone_verification WHERE result_ciphertext IS NOT NULL AND (closed_at IS NOT NULL OR verified_at<=$1) LIMIT $2 FOR UPDATE SKIP LOCKED)`, now.Add(-verification.ResultTTL), batch)
	if err != nil {
		return 0, fmt.Errorf("verification: erase results: %w", err)
	}
	released, err := q.Exec(ctx, `UPDATE phone_verification SET released_at=$1 WHERE id IN (SELECT id FROM phone_verification WHERE released_at IS NULL AND expires_at<=$1 LIMIT $2 FOR UPDATE SKIP LOCKED)`, now, batch)
	if err != nil {
		return 0, fmt.Errorf("verification: release holders: %w", err)
	}
	return wiped.RowsAffected() + released.RowsAffected(), nil
}
func (r *Verifications) DetachOldRequests(ctx context.Context, now time.Time, batch int) (int64, error) {
	if _, ok := Tx(ctx); !ok {
		return 0, errVerificationTx
	}
	if batch < 1 || batch > 1000 {
		return 0, errors.New("verification: invalid cleanup batch")
	}
	tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE auth_session SET phone_verification_id=NULL WHERE id IN (SELECT s.id FROM auth_session s JOIN phone_verification v ON v.id=s.phone_verification_id WHERE v.created_at<=$1 AND v.expires_at<=$2 AND (v.verified_at IS NULL OR v.closed_at IS NOT NULL OR v.verified_at<=$3) LIMIT $4 FOR UPDATE OF s SKIP LOCKED)`, now.Add(-24*time.Hour), now, now.Add(-verification.ResultTTL), batch)
	if err != nil {
		return 0, fmt.Errorf("verification: detach old request links: %w", err)
	}
	return tag.RowsAffected(), nil
}
func (r *Verifications) DeleteOldRequests(ctx context.Context, now time.Time, batch int) (int64, error) {
	if _, ok := Tx(ctx); !ok {
		return 0, errVerificationTx
	}
	if batch < 1 || batch > 1000 {
		return 0, errors.New("verification: invalid cleanup batch")
	}
	tag, err := r.db.Writer(ctx).Exec(ctx, `DELETE FROM phone_verification WHERE id IN (SELECT v.id FROM phone_verification v WHERE v.created_at<=$1 AND v.expires_at<=$2 AND (v.verified_at IS NULL OR v.closed_at IS NOT NULL OR v.verified_at<=$3) AND NOT EXISTS (SELECT 1 FROM auth_session s WHERE s.phone_verification_id=v.id) AND NOT EXISTS (SELECT 1 FROM phone_verification_start p JOIN idempotency_keys k ON k.key=p.idempotency_key WHERE p.verification_id=v.id AND k.scope='00000000-0000-0000-0000-000000000000'::uuid AND k.endpoint='/finassist.user.v1.AuthService/StartPhoneVerification' AND k.expires_at>$2) LIMIT $4 FOR UPDATE OF v SKIP LOCKED)`, now.Add(-24*time.Hour), now, now.Add(-verification.ResultTTL), batch)
	if err != nil {
		return 0, fmt.Errorf("verification: remove old requests: %w", err)
	}
	return tag.RowsAffected(), nil
}
