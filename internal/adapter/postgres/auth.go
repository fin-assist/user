package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fin-assist/user/internal/auth"
	"github.com/fin-assist/user/internal/token"
)

// Auth stores primary-only authentication state in the caller's transaction.
type Auth struct{ db *DB }

var errAuthTx = errors.New("auth: primary transaction required")

// NewAuth assembles authentication queries without starting jobs or transactions.
func NewAuth(db *DB) *Auth { return &Auth{db: db} }

const linkSQL = `SELECT r.session_id,r.generation,s.user_id::text,s.phone_verification_id,r.issued_at,r.expires_at,r.rotated_at,s.revoked_at
FROM refresh_token r JOIN auth_session s ON s.id=r.session_id WHERE r.token_hash=$1 AND r.expires_at>$2`

// FindLink learns the guard key before taking any session or user row lock.
func (r *Auth) FindLink(ctx context.Context, hash token.Hash, now time.Time) (auth.Link, error) {
	return scanLink(r.db.Writer(ctx).QueryRow(ctx, linkSQL, hash[:], now))
}

// LockLink serializes rotations and reuse of the same chain after its user guard.
func (r *Auth) LockLink(ctx context.Context, hash token.Hash, now time.Time) (auth.Link, error) {
	if _, ok := Tx(ctx); !ok {
		return auth.Link{}, errAuthTx
	}
	var session int32
	err := r.db.Writer(ctx).QueryRow(ctx, `SELECT id FROM auth_session WHERE id=(SELECT session_id FROM refresh_token WHERE token_hash=$1 AND expires_at>$2) FOR UPDATE`, hash[:], now).Scan(&session)
	if err != nil {
		return auth.Link{}, authError(err)
	}
	return scanLink(r.db.Writer(ctx).QueryRow(ctx, linkSQL+` FOR UPDATE OF r`, hash[:], now))
}

func scanLink(row pgx.Row) (auth.Link, error) {
	var l auth.Link
	err := row.Scan(&l.SessionID, &l.Generation, &l.UserID, &l.VerificationID, &l.IssuedAt, &l.ExpiresAt, &l.RotatedAt, &l.RevokedAt)
	return l, authError(err)
}

// Rotate replaces the sole live link; both operations belong to one transaction.
func (r *Auth) Rotate(ctx context.Context, old, newHash token.Hash, l auth.Link, now, expires time.Time) error {
	if _, ok := Tx(ctx); !ok {
		return errAuthTx
	}
	tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE refresh_token SET rotated_at=$3 WHERE token_hash=$1 AND session_id=$2 AND rotated_at IS NULL`, old[:], l.SessionID, now)
	if err != nil {
		return authError(err)
	}
	if tag.RowsAffected() != 1 {
		return auth.ErrMissing
	}
	_, err = r.db.Writer(ctx).Exec(ctx, `INSERT INTO refresh_token(token_hash,session_id,generation,issued_at,expires_at) VALUES($1,$2,$3,$4,$5)`, newHash[:], l.SessionID, l.Generation+1, now, expires)
	return authError(err)
}

// Revoke marks only this user's chain and keeps its old links for reuse detection.
func (r *Auth) Revoke(ctx context.Context, l auth.Link, now time.Time) error {
	if _, ok := Tx(ctx); !ok {
		return errAuthTx
	}
	_, err := r.db.Writer(ctx).Exec(ctx, `UPDATE auth_session SET revoked_at=$3 WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL`, l.SessionID, l.UserID, now)
	return authError(err)
}

// Logout deletes a chain selected by an unguessable refresh hash, including old links.
func (r *Auth) Logout(ctx context.Context, hash token.Hash) error {
	if _, ok := Tx(ctx); !ok {
		return errAuthTx
	}
	_, err := r.db.Writer(ctx).Exec(ctx, `DELETE FROM auth_session WHERE id=(SELECT session_id FROM refresh_token WHERE token_hash=$1)`, hash[:])
	return authError(err)
}

// Registration reads an immutable verified result, optionally locking it for consumption.
func (r *Auth) Registration(ctx context.Context, hash token.Hash, lock bool) (auth.Registration, error) {
	q := `SELECT id,phone,verified_at,COALESCE(is_new_user,false),closed_at IS NOT NULL FROM phone_verification WHERE registration_token_hash=$1`
	if lock {
		if _, ok := Tx(ctx); !ok {
			return auth.Registration{}, errAuthTx
		}
		q += ` FOR UPDATE`
	}
	var v auth.Registration
	err := r.db.Writer(ctx).QueryRow(ctx, q, hash[:]).Scan(&v.ID, &v.Phone, &v.VerifiedAt, &v.IsNewUser, &v.Closed)
	return v, authError(err)
}

// CurrentPD reads the current published personal-data document from primary.
func (r *Auth) CurrentPD(ctx context.Context) (string, error) {
	var version string
	err := r.db.Writer(ctx).QueryRow(ctx, `SELECT version FROM consent_document WHERE type='personal_data' AND is_current FOR SHARE`).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return version, authError(err)
}

// CreateUser returns the inserted ID or the phone's concurrently created account.
// Existing settings and consent history are never updated on this path.
func (r *Auth) CreateUser(ctx context.Context, phone, zone string, now time.Time) (string, bool, error) {
	if _, ok := Tx(ctx); !ok {
		return "", false, errAuthTx
	}
	var id string
	err := r.db.Writer(ctx).QueryRow(ctx, `INSERT INTO app_user(phone,timezone,created_at,updated_at) VALUES($1,$2,$3,$3) ON CONFLICT(phone) DO NOTHING RETURNING id::text`, phone, zone, now).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, authError(err)
	}
	err = r.db.Writer(ctx).QueryRow(ctx, `SELECT id::text FROM app_user WHERE phone=$1`, phone).Scan(&id)
	return id, false, authError(err)
}

// LockUser preserves the guard -> user -> verification/session lock order.
func (r *Auth) LockUser(ctx context.Context, id string) error {
	if _, ok := Tx(ctx); !ok {
		return errAuthTx
	}
	var found string
	return authError(r.db.Writer(ctx).QueryRow(ctx, `SELECT id::text FROM app_user WHERE id=$1 FOR UPDATE`, id).Scan(&found))
}

// GrantPD records only the accepted document of a newly-created account.
func (r *Auth) GrantPD(ctx context.Context, id, version string, now time.Time) error {
	if _, ok := Tx(ctx); !ok {
		return errAuthTx
	}
	_, err := r.db.Writer(ctx).Exec(ctx, `INSERT INTO user_consent(user_id,type,version,granted_at) VALUES($1,'personal_data',$2,$3)`, id, version, now)
	return authError(err)
}

// CreateSession writes a chain and its first link together.
func (r *Auth) CreateSession(ctx context.Context, id string, verification int32, hash token.Hash, now, expires time.Time) error {
	if _, ok := Tx(ctx); !ok {
		return errAuthTx
	}
	var session int32
	err := r.db.Writer(ctx).QueryRow(ctx, `INSERT INTO auth_session(user_id,phone_verification_id,created_at) VALUES($1,$2,$3) RETURNING id`, id, verification, now).Scan(&session)
	if err != nil {
		return authError(err)
	}
	_, err = r.db.Writer(ctx).Exec(ctx, `INSERT INTO refresh_token(token_hash,session_id,generation,issued_at,expires_at) VALUES($1,$2,1,$3,$4)`, hash[:], session, now, expires)
	return authError(err)
}

// CloseVerification consumes a result once and wipes every re-deliverable secret.
func (r *Auth) CloseVerification(ctx context.Context, id int32, user string, now time.Time) error {
	if _, ok := Tx(ctx); !ok {
		return errAuthTx
	}
	tag, err := r.db.Writer(ctx).Exec(ctx, `UPDATE phone_verification SET user_id=$2,closed_at=GREATEST($3,verified_at),result_ciphertext=NULL,result_key_id=NULL WHERE id=$1 AND closed_at IS NULL AND verified_at IS NOT NULL AND (user_id IS NULL OR user_id=$2)`, id, user, now)
	if err != nil {
		return authError(err)
	}
	if tag.RowsAffected() == 0 {
		// Later rotations may refer to an already closed verification. Check that
		// the result belongs to the same user rather than accepting another ID.
		var owned bool
		err = r.db.Writer(ctx).QueryRow(ctx, `SELECT closed_at IS NOT NULL AND user_id=$2 FROM phone_verification WHERE id=$1`, id, user).Scan(&owned)
		if err != nil {
			return authError(err)
		}
		if !owned {
			return auth.ErrMissing
		}
	}
	return nil
}

// CleanupSessions removes expired links and then only chains with no links left.
// Rotated links survive until their own expiry, preserving reuse detection.
func (r *Auth) CleanupSessions(ctx context.Context, now time.Time, batch int) (int64, error) {
	if _, ok := Tx(ctx); !ok {
		return 0, errAuthTx
	}
	links, err := r.db.Writer(ctx).Exec(ctx, `DELETE FROM refresh_token WHERE token_hash IN (SELECT token_hash FROM refresh_token WHERE expires_at<=$1 LIMIT $2 FOR UPDATE SKIP LOCKED)`, now, batch)
	if err != nil {
		return 0, authError(err)
	}
	chains, err := r.db.Writer(ctx).Exec(ctx, `DELETE FROM auth_session WHERE id IN (SELECT s.id FROM auth_session s WHERE NOT EXISTS(SELECT 1 FROM refresh_token r WHERE r.session_id=s.id) LIMIT $1 FOR UPDATE OF s SKIP LOCKED)`, batch)
	return links.RowsAffected() + chains.RowsAffected(), authError(err)
}

func authError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.ErrMissing
	}
	var e *pgconn.PgError
	if errors.As(err, &e) {
		switch e.ConstraintName {
		case "user_consent_document_fkey":
			return finerr.ConsentOutdated()
		case "auth_session_user_fkey", "auth_session_phone_verification_fkey", "phone_verification_user_fkey", "user_consent_user_fkey":
			return auth.ErrMissing
		}
	}
	return fmt.Errorf("auth repository: %w", err)
}
