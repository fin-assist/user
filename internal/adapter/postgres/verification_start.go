package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/verification"
)

const startPhoneVerificationEndpoint = "/finassist.user.v1.AuthService/StartPhoneVerification"

// VerificationStarts persists creation ownership in the request on primary.
type VerificationStarts struct {
	db  *DB
	now func() time.Time
}

// NewVerificationStarts shares the caller's primary transaction.
func NewVerificationStarts(db *DB) *VerificationStarts {
	return &VerificationStarts{db: db, now: time.Now}
}

// WithClock pins key-window reads to the same application clock.
func (r *VerificationStarts) WithClock(now func() time.Time) *VerificationStarts {
	r.now = now
	return r
}

func (r *VerificationStarts) Find(ctx context.Context, key string) (verification.StartRecord, error) {
	var out verification.StartRecord
	var hash, windows []byte
	var id int32
	err := r.db.Writer(ctx).QueryRow(ctx, `SELECT e.start_key::text,CASE WHEN v.id IS NULL OR v.start_request_hash=e.request_hash THEN e.request_hash ELSE '\x00'::bytea END,e.attempt_id,CASE WHEN v.id IS NULL THEN '[]'::jsonb ELSE v.start_reserved_windows END,e.consent_version,e.client_ip,e.consent_accepted_at,e.key_expires_at,e.phone,e.attempt_end FROM phone_consent_receipt e LEFT JOIN phone_verification v ON v.id=e.attempt_id WHERE e.start_key=$1 AND e.key_expires_at>$2 ORDER BY e.key_expires_at DESC LIMIT 1`, key, r.now()).Scan(&out.Key, &hash, &id, &windows, &out.Consent.Version, &out.ClientIP, &out.AcceptedAt, &out.KeyExpiresAt, &out.Request.Phone, &out.Request.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, verification.ErrMissing
	}
	if err != nil {
		return out, fmt.Errorf("verification start: receipt: %w", err)
	}
	decoded, err := decodeStartRecord(out.Key, hash, windows)
	if err != nil {
		return out, err
	}
	out.Hash = decoded.Hash
	out.Windows = decoded.Windows
	out.Consent.Accepted = true
	out.Consent.AgeConfirmed = true
	phone, end := out.Request.Phone, out.Request.ExpiresAt
	out.Request, err = scanVerification(r.db.Writer(ctx).QueryRow(ctx, verificationSQL+` WHERE id=$1`, id))
	if errors.Is(err, verification.ErrMissing) {
		out.Request = verification.Request{ID: id, Phone: phone, ExpiresAt: end, Closed: true, Released: true, StartTerminal: true, Evidence: true}
		err = nil
	}
	return out, err
}

// Busy checks both working claims and independently authenticated active-call facts.
func (r *VerificationStarts) Busy(ctx context.Context, phone string, now time.Time) (int32, time.Time, error) {
	if _, ok := Tx(ctx); !ok {
		return 0, time.Time{}, errVerificationTx
	}
	q := r.db.Writer(ctx)
	if _, err := q.Exec(ctx, `UPDATE phone_verification SET released_at=$2,closed_at=CASE WHEN provider_call_id IS NULL THEN COALESCE(closed_at,$2) ELSE closed_at END,start_terminal=CASE WHEN provider_call_id IS NULL THEN true ELSE start_terminal END WHERE phone=$1 AND released_at IS NULL AND COALESCE(known_busy_until,expires_at)<=$2`, phone, now); err != nil {
		return 0, time.Time{}, err
	}
	var id int32
	var until time.Time
	err := q.QueryRow(ctx, `SELECT id,COALESCE(known_busy_until,expires_at) FROM phone_verification WHERE phone=$1 AND released_at IS NULL FOR KEY SHARE`, phone).Scan(&id, &until)
	if err == nil {
		return id, until, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, until, err
	}
	err = q.QueryRow(ctx, `SELECT COALESCE((SELECT id FROM phone_verification WHERE phone=$1 ORDER BY id DESC LIMIT 1),0),busy_until FROM phone_known_busy WHERE phone=$1 AND busy_until>$2`, phone, now).Scan(&id, &until)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, nil
	}
	return id, until, err
}
func (r *VerificationStarts) CurrentPD(ctx context.Context) (string, error) {
	return NewAuth(r.db).CurrentPD(ctx)
}

// Dispatch commits the single external-effect marker with its timestamp. A crash
// after this commit grants only read-only recovery, even if no packet was sent.
func (r *VerificationStarts) Dispatch(ctx context.Context, v verification.StartRecord, now time.Time) (verification.StartRecord, bool, error) {
	if _, ok := Tx(ctx); !ok {
		return v, false, errVerificationTx
	}
	q := r.db.Writer(ctx)
	var at, until time.Time
	err := q.QueryRow(ctx, `UPDATE phone_verification SET first_dispatch_at=$4,dispatch_not_after=unknown_deadline WHERE id=$1 AND start_key=$2 AND start_request_hash=$3 AND first_dispatch_at IS NULL AND NOT start_terminal AND closed_at IS NULL AND released_at IS NULL AND unknown_deadline>$4 AND consent_receipt_id IS NOT NULL RETURNING first_dispatch_at,dispatch_not_after`, v.Request.ID, v.Key, v.Hash[:], now).Scan(&at, &until)
	first := err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		err = q.QueryRow(ctx, `SELECT first_dispatch_at,dispatch_not_after FROM phone_verification WHERE id=$1 AND start_key=$2 AND start_request_hash=$3 AND first_dispatch_at IS NOT NULL AND NOT start_terminal AND closed_at IS NULL AND released_at IS NULL AND unknown_deadline>$4`, v.Request.ID, v.Key, v.Hash[:], now).Scan(&at, &until)
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return v, false, verification.ErrMissing
		}
		return v, false, fmt.Errorf("verification start: dispatch: %w", err)
	}
	v.Request.FirstDispatchAt = at
	v.Request.DispatchNotAfter = until
	return v, first, nil
}
func (r *VerificationStarts) Terminate(ctx context.Context, v verification.StartRecord, now time.Time) error {
	if _, ok := Tx(ctx); !ok {
		return errVerificationTx
	}
	_, err := r.db.Writer(ctx).Exec(ctx, `UPDATE phone_verification SET start_terminal=true,closed_at=COALESCE(closed_at,$2),released_at=COALESCE(released_at,$2),start_reserved_windows='[]'::jsonb WHERE id=$1 AND provider_call_id IS NULL`, v.Request.ID, now)
	return err
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
	// Expired key lookup is fresh even while an older receipt is still retained.
	if _, err = q.Exec(ctx, `UPDATE phone_verification v SET start_key=NULL,start_request_hash=NULL,start_reserved_windows=NULL WHERE v.start_key=$1 AND EXISTS(SELECT 1 FROM phone_consent_receipt e WHERE e.attempt_id=v.id AND e.key_expires_at<=$2)`, v.Key, v.AcceptedAt); err != nil {
		return v, time.Time{}, err
	}
	for range 3 {
		if _, err := q.Exec(ctx, `UPDATE phone_verification SET released_at=$2,start_terminal=CASE WHEN provider_call_id IS NULL THEN true ELSE start_terminal END WHERE phone=$1 AND released_at IS NULL AND COALESCE(known_busy_until,expires_at)<=$2`, v.Request.Phone, v.Request.CreatedAt); err != nil {
			return v, time.Time{}, fmt.Errorf("verification start: release expired: %w", err)
		}
		err := q.QueryRow(ctx, `INSERT INTO phone_verification(phone,token_hash,provider,created_at,expires_at,start_key,start_request_hash,start_reserved_windows,unknown_deadline,campaign) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$5,$9) ON CONFLICT(phone) WHERE released_at IS NULL DO NOTHING RETURNING id`, v.Request.Phone, v.Request.Hash[:], v.Request.Provider, v.Request.CreatedAt, v.Request.ExpiresAt, v.Key, v.Hash[:], windows, v.Request.Campaign).Scan(&v.Request.ID)
		if err == nil {
			_, err = q.Exec(ctx, `INSERT INTO phone_consent_receipt(attempt_id,phone,client_ip,consent_version,consent_accepted_at,age_confirmed_at,attempt_end,retain_until,start_key,request_hash,key_expires_at) VALUES($1,$2,$3,$4,$5,$5,$6,$7,$8,$9,$10)`, v.Request.ID, v.Request.Phone, v.ClientIP, v.Consent.Version, v.AcceptedAt, v.Request.UnknownDeadline, v.Request.UnknownDeadline.Add(24*time.Hour), v.Key, v.Hash[:], v.KeyExpiresAt)
			if err != nil {
				return v, time.Time{}, err
			}
			_, err = q.Exec(ctx, `UPDATE phone_verification SET consent_receipt_id=id WHERE id=$1`, v.Request.ID)
			if err != nil {
				return v, time.Time{}, err
			}
			return v, time.Time{}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return v, time.Time{}, fmt.Errorf("verification start: claim phone: %w", err)
		}
		// Serialize a busy link with deletion's parent lock before its key purge.
		var holder int32
		var expires time.Time
		err = q.QueryRow(ctx, `SELECT id,COALESCE(known_busy_until,expires_at) FROM phone_verification WHERE phone=$1 AND released_at IS NULL FOR KEY SHARE`, v.Request.Phone).Scan(&holder, &expires)
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
	q := r.db.Writer(ctx)
	now := r.now().UTC()
	if c.Campaign != v.Request.Campaign || !c.PublicExpiry().After(now) {
		return verification.ErrProviderUnavailable
	}
	// Row transition and unique binding share one transaction and roll back together.
	tag, err := q.Exec(ctx, `UPDATE phone_verification v SET provider_call_id=$4,token_hash=$5,created_at=$6,expires_at=$7,declared_provider_expiry=$10,known_busy_until=$11,start_reserved_windows='[]'::jsonb WHERE id=$1 AND start_key=$2 AND start_request_hash=$3 AND provider=$8 AND token_hash=$9 AND provider_call_id IS NULL AND verified_at IS NULL AND closed_at IS NULL AND released_at IS NULL AND NOT start_terminal AND consent_receipt_id IS NOT NULL AND first_dispatch_at IS NOT NULL AND unknown_deadline>$12`, v.Request.ID, v.Key, v.Hash[:], c.ID, h[:], c.StartedAt, c.PublicExpiry(), v.Request.Provider, v.Request.Hash[:], c.StartedAt.Add(verification.RequestTTL), c.BusyUntil(), now)
	if err != nil {
		if pgstore.IsViolation(err, "23505", "phone_verification_provider_call_key") {
			return &verification.ProviderDuplicateError{BusyUntil: c.BusyUntil()}
		}
		return fmt.Errorf("verification start: complete: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return verification.ErrMissing
	}
	tag, err = q.Exec(ctx, `INSERT INTO phone_provider_binding(provider,campaign,call_id,attempt_id,provider_created_at,declared_expiry,public_deadline,busy_until) VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(provider,campaign,call_id) DO NOTHING`, v.Request.Provider, c.Campaign, c.ID, v.Request.ID, c.StartedAt, c.StartedAt.Add(verification.RequestTTL), c.PublicExpiry(), c.BusyUntil())
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return &verification.ProviderDuplicateError{BusyUntil: c.BusyUntil()}
	}
	_, err = q.Exec(ctx, `UPDATE phone_consent_receipt SET attempt_end=$2,retain_until=CASE WHEN linked_user_id IS NULL THEN $3 ELSE retain_until END WHERE attempt_id=$1`, v.Request.ID, c.PublicExpiry(), c.PublicExpiry().Add(24*time.Hour))
	return err
}

func (r *VerificationStarts) Reject(ctx context.Context, v verification.StartRecord, now time.Time) error {
	if _, ok := Tx(ctx); !ok {
		return errVerificationTx
	}
	tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE phone_verification v SET released_at=$4,start_terminal=true,closed_at=$4,start_reserved_windows='[]'::jsonb WHERE v.id=$1 AND v.start_key=$2 AND v.start_request_hash=$3 AND v.provider_call_id IS NULL AND v.verified_at IS NULL AND NOT v.start_terminal AND v.closed_at IS NULL AND v.released_at IS NULL AND v.unknown_deadline>$4`, v.Request.ID, v.Key, v.Hash[:], now)
	if err != nil {
		return fmt.Errorf("verification start: reject: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return verification.ErrMissing
	}
	// The receipt preserves the terminal key even after library reservation release.
	return nil
}

func (r *VerificationStarts) Duplicate(ctx context.Context, v verification.StartRecord, now time.Time) error {
	if _, ok := Tx(ctx); !ok {
		return errVerificationTx
	}
	tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE phone_verification v SET released_at=$4,start_terminal=true,closed_at=$4,start_reserved_windows='[]'::jsonb WHERE v.id=$1 AND v.start_key=$2 AND v.start_request_hash=$3 AND v.provider_call_id IS NULL AND v.verified_at IS NULL AND NOT v.start_terminal AND v.closed_at IS NULL AND v.released_at IS NULL AND v.unknown_deadline>$4`, v.Request.ID, v.Key, v.Hash[:], now)
	if err != nil {
		return fmt.Errorf("verification start: duplicate: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return verification.ErrMissing
	}
	if v.Request.KnownBusyUntil.After(now) {
		_, err = r.db.Writer(ctx).Exec(ctx, `INSERT INTO phone_known_busy(phone,busy_until) VALUES($1,$2) ON CONFLICT(phone) DO UPDATE SET busy_until=GREATEST(phone_known_busy.busy_until,EXCLUDED.busy_until)`, v.Request.Phone, v.Request.KnownBusyUntil)
		if err != nil {
			return err
		}
	}
	return nil
}

// FinalizeKey keeps the library's successful cache inside the original key window.
func (r *VerificationStarts) FinalizeKey(ctx context.Context, v verification.StartRecord) error {
	if _, ok := Tx(ctx); !ok {
		return errVerificationTx
	}
	_, err := r.db.Writer(ctx).Exec(ctx, `UPDATE idempotency_keys SET expires_at=LEAST(expires_at,$3) WHERE key=$1 AND scope='00000000-0000-0000-0000-000000000000'::uuid AND endpoint=$2 AND done`, v.Key, startPhoneVerificationEndpoint, v.KeyExpiresAt)
	return err
}
