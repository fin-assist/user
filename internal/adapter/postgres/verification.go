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
type Verifications struct {
	db       *DB
	campaign string
}

func NewVerifications(db *DB, campaign ...string) *Verifications {
	r := &Verifications{db: db}
	if len(campaign) > 0 {
		r.campaign = campaign[0]
	}
	return r
}

const verificationSQL = `SELECT id,phone,token_hash,provider,COALESCE(provider_call_id,''),created_at,expires_at,verified_at,COALESCE(is_new_user,false),COALESCE(user_id::text,''),registration_token_hash,result_ciphertext,COALESCE(result_key_id,''),closed_at IS NOT NULL,released_at IS NOT NULL,consent_receipt_id IS NOT NULL,campaign,COALESCE(unknown_deadline,expires_at),COALESCE(first_dispatch_at,created_at),COALESCE(dispatch_not_after,expires_at),COALESCE(declared_provider_expiry,expires_at),COALESCE(known_busy_until,expires_at),start_terminal FROM phone_verification`

func (r *Verifications) ByToken(ctx context.Context, h token.Hash) (verification.Request, error) {
	return scanVerification(r.db.Writer(ctx).QueryRow(ctx, verificationSQL+` WHERE token_hash=$1`, h[:]))
}
func (r *Verifications) ByCall(ctx context.Context, provider, call string) (verification.Request, error) {
	return scanVerification(r.db.Writer(ctx).QueryRow(ctx, verificationSQL+` WHERE provider=$1 AND provider_call_id=$2 AND campaign=$3`, provider, call, r.campaign))
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
	err := row.Scan(&r.ID, &r.Phone, &hash, &r.Provider, &r.CallID, &r.CreatedAt, &r.ExpiresAt, &r.VerifiedAt, &r.IsNewUser, &r.UserID, &registration, &r.Ciphertext, &r.KeyID, &r.Closed, &r.Released, &r.Evidence, &r.Campaign, &r.UnknownDeadline, &r.FirstDispatchAt, &r.DispatchNotAfter, &r.DeclaredExpiry, &r.KnownBusyUntil, &r.StartTerminal)
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
	tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE phone_verification SET verified_at=$2,is_new_user=$3,user_id=NULLIF($4,'')::uuid,registration_token_hash=$5,result_ciphertext=$6,result_key_id=$7 WHERE id=$1 AND verified_at IS NULL AND closed_at IS NULL AND released_at IS NULL AND provider_call_id IS NOT NULL AND created_at<=$2 AND expires_at>$2 AND consent_receipt_id IS NOT NULL`, v.ID, now, o.IsNewUser, o.UserID, registrationBytes, ciphertext, key)
	if err != nil {
		return fmt.Errorf("verification: confirm: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return verification.ErrMissing
	}
	if o.UserID != "" {
		if err := linkPhoneReceipt(ctx, r.db, v.ID, o.UserID, now); err != nil {
			return err
		}
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
	released, err := q.Exec(ctx, `UPDATE phone_verification SET released_at=$1,start_terminal=CASE WHEN provider_call_id IS NULL THEN true ELSE start_terminal END,closed_at=CASE WHEN provider_call_id IS NULL THEN COALESCE(closed_at,$1) ELSE closed_at END WHERE id IN (SELECT id FROM phone_verification WHERE released_at IS NULL AND COALESCE(known_busy_until,expires_at)<=$1 LIMIT $2 FOR UPDATE SKIP LOCKED)`, now, batch)
	if err != nil {
		return 0, fmt.Errorf("verification: release holders: %w", err)
	}
	if _, err = q.Exec(ctx, `DELETE FROM phone_known_busy WHERE phone IN(SELECT phone FROM phone_known_busy WHERE busy_until<=$1 LIMIT $2 FOR UPDATE SKIP LOCKED)`, now, batch); err != nil {
		return 0, err
	}
	if _, err = q.Exec(ctx, `UPDATE phone_consent_receipt SET start_key=NULL,request_hash=NULL WHERE attempt_id IN(SELECT attempt_id FROM phone_consent_receipt WHERE key_expires_at<=$1 AND start_key IS NOT NULL LIMIT $2 FOR UPDATE SKIP LOCKED)`, now, batch); err != nil {
		return 0, err
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
	tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE auth_session SET phone_verification_id=NULL WHERE id IN (SELECT s.id FROM auth_session s JOIN phone_verification v ON v.id=s.phone_verification_id WHERE v.expires_at<=$1 AND v.expires_at<=$2 AND (v.verified_at IS NULL OR v.closed_at IS NOT NULL OR v.verified_at<=$3) LIMIT $4 FOR UPDATE OF s SKIP LOCKED)`, now.Add(-24*time.Hour), now, now.Add(-verification.ResultTTL), batch)
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
	tag, err := r.db.Writer(ctx).Exec(ctx, `DELETE FROM phone_verification WHERE id IN (SELECT v.id FROM phone_verification v WHERE v.expires_at<=$1 AND v.expires_at<=$2 AND (v.verified_at IS NULL OR v.closed_at IS NOT NULL OR v.verified_at<=$3) AND NOT EXISTS (SELECT 1 FROM auth_session s WHERE s.phone_verification_id=v.id) AND NOT EXISTS (SELECT 1 FROM idempotency_keys k WHERE k.resource_type='phone_verification' AND k.resource_id=v.id::text AND k.scope='00000000-0000-0000-0000-000000000000'::uuid AND k.endpoint=$5 AND k.expires_at>$2) LIMIT $4 FOR UPDATE OF v SKIP LOCKED)`, now.Add(-24*time.Hour), now, now.Add(-verification.ResultTTL), batch, startPhoneVerificationEndpoint)
	if err != nil {
		return 0, fmt.Errorf("verification: remove old requests: %w", err)
	}
	erased, err := r.db.Writer(ctx).Exec(ctx, `DELETE FROM phone_consent_receipt WHERE attempt_id IN(SELECT e.attempt_id FROM phone_consent_receipt e WHERE e.retain_until<=$1 AND NOT EXISTS(SELECT 1 FROM phone_verification v WHERE v.consent_receipt_id=e.attempt_id) LIMIT $2 FOR UPDATE OF e SKIP LOCKED)`, now, batch)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected() + erased.RowsAffected(), nil
}

// linkPhoneReceipt runs only after verified phone ownership and under the request lock.
func linkPhoneReceipt(ctx context.Context, db *DB, attempt int32, user string, now time.Time) error {
	tag, err := db.Writer(ctx).Exec(ctx, `UPDATE phone_consent_receipt SET linked_user_id=$2::uuid,linked_at=COALESCE(linked_at,$3),retain_until=NULL WHERE attempt_id=$1 AND (linked_user_id IS NULL OR linked_user_id=$2::uuid)`, attempt, user, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return verification.ErrMissing
	}
	return nil
}
