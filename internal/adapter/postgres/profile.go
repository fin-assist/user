package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/user/internal/profile"
)

// Profiles reads consistent profile/consent snapshots and writes user-scoped data.
type Profiles struct{ db *DB }

// NewProfiles shares the transaction manager with the profile usecase.
func NewProfiles(db *DB) *Profiles { return &Profiles{db: db} }

const profileQuery = `SELECT u.id::text,u.phone,u.theme,u.timezone,u.settings_version,u.created_at,
 c.version,c.granted_at,COALESCE(d.version,'') FROM app_user u
 LEFT JOIN user_consent c ON c.user_id=u.id AND c.type='assistant' AND c.revoked_at IS NULL
 LEFT JOIN consent_document d ON d.type='assistant' AND d.is_current
 WHERE u.id=$1`

// Read uses the transaction, read-primary hint or replica as chosen by DB.Reader.
func (r *Profiles) Read(ctx context.Context, id string) (profile.Record, error) {
	return readProfile(ctx, r.db.Reader(ctx), id)
}

// Current always uses primary, including the service transaction when present.
func (r *Profiles) Current(ctx context.Context, id string) (profile.Record, error) {
	return readProfile(ctx, r.db.Writer(ctx), id)
}

func readProfile(ctx context.Context, q Querier, id string) (profile.Record, error) {
	var p profile.Record
	err := q.QueryRow(ctx, profileQuery, id).Scan(&p.ID, &p.Phone, &p.Theme, &p.Timezone, &p.SettingsVersion, &p.CreatedAt, &p.Consent.Version, &p.Consent.GrantedAt, &p.Consent.CurrentVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return profile.Record{}, finerr.NotFound()
	}
	if err != nil {
		return profile.Record{}, fmt.Errorf("read profile: %w", err)
	}
	return p, nil
}

// Lock locks the user's row before settings/consent changes. All writes require a transaction.
func (r *Profiles) Lock(ctx context.Context, id string) error {
	tx, ok := r.db.Tx(ctx)
	if !ok {
		return errors.New("profile: primary transaction required")
	}
	var found string
	err := tx.QueryRow(ctx, `SELECT id::text FROM app_user WHERE id=$1 FOR UPDATE`, id).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return finerr.NotFound()
	}
	if err != nil {
		return fmt.Errorf("lock profile: %w", err)
	}
	return nil
}

// Update writes the complete settings chosen under the user's lock.
func (r *Profiles) Update(ctx context.Context, id, theme, zone string, version int32, now time.Time) error {
	tx, ok := r.db.Tx(ctx)
	if !ok {
		return errors.New("profile: primary transaction required")
	}
	tag, err := tx.Exec(ctx, `UPDATE app_user SET theme=$2,timezone=$3,settings_version=$4,updated_at=$5 WHERE id=$1`, id, theme, zone, version, now)
	if err != nil {
		return fmt.Errorf("update profile: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return finerr.NotFound()
	}
	return nil
}

// Grant inserts a history row for an existing document and user.
func (r *Profiles) Grant(ctx context.Context, id, version string, now time.Time) error {
	tx, ok := r.db.Tx(ctx)
	if !ok {
		return errors.New("profile: primary transaction required")
	}
	_, err := tx.Exec(ctx, `INSERT INTO user_consent(user_id,type,version,granted_at) VALUES($1,'assistant',$2,$3)`, id, version, now)
	if err != nil {
		return fmt.Errorf("grant assistant consent: %w", err)
	}
	return nil
}

// Revoke closes only the active assistant history row; an absent grant is a no-op.
func (r *Profiles) Revoke(ctx context.Context, id string, now time.Time) error {
	tx, ok := r.db.Tx(ctx)
	if !ok {
		return errors.New("profile: primary transaction required")
	}
	_, err := tx.Exec(ctx, `UPDATE user_consent SET revoked_at=$2 WHERE user_id=$1 AND type='assistant' AND revoked_at IS NULL`, id, now)
	if err != nil {
		return fmt.Errorf("revoke assistant consent: %w", err)
	}
	return nil
}
