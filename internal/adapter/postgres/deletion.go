package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fin-assist/user/internal/deletion"
)

// Accounts removes user-owned data inside the tombstone transaction on primary.
type Accounts struct {
	db  *DB
	now func() time.Time
}

// NewAccounts shares the transaction manager with deletion and outbox publication.
func NewAccounts(db *DB) *Accounts { return &Accounts{db: db, now: time.Now} }

// WithClock supplies the application clock for consent-evidence retention.
func (r *Accounts) WithClock(now func() time.Time) *Accounts {
	r.now = now
	return r
}

// Remove requires the caller to hold go-tombstone's user guard. NOWAIT avoids
// inversion with auth replay (key lock, then user guard): the usecase rolls back
// and retries rather than holding the guard while waiting for that key.
func (r *Accounts) Remove(ctx context.Context, id string) error {
	tx, ok := r.db.Tx(ctx)
	if !ok {
		return errors.New("deletion: primary transaction required")
	}
	rows, err := tx.Query(ctx, `SELECT 1 FROM idempotency_keys WHERE scope=$1::uuid OR owner_id=$1::uuid FOR UPDATE NOWAIT`, id)
	if err != nil {
		return deletionError(err)
	}
	for rows.Next() { /* Drain locks without retaining keys or payloads. */
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return deletionError(err)
	}
	var phone string
	err = tx.QueryRow(ctx, `SELECT phone FROM app_user WHERE id=$1 FOR UPDATE`, id).Scan(&phone)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("deletion: read account: %w", err)
	}
	if phone != "" {
		if err = r.purgeStartKeys(ctx, phone); err != nil {
			return err
		}
	}
	// An absent local account without a tombstone still needs remote purges; a
	// signed identity can outlive a partially restored database.
	if _, err = tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE scope=$1::uuid OR owner_id=$1::uuid`, id); err != nil {
		return deletionError(err)
	}
	// Independent evidence survives deletion without keeping an account FK.
	// Each receipt starts its policy retention once; repeated deletion cannot extend it.
	if _, err = tx.Exec(ctx, `UPDATE phone_consent_receipt SET retain_until=$2 WHERE linked_user_id=$1::uuid AND retain_until IS NULL`, id, r.now().UTC().AddDate(3, 0, 0)); err != nil {
		return fmt.Errorf("deletion: retain phone evidence: %w", err)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM app_user WHERE id=$1`, id); err != nil {
		return fmt.Errorf("deletion: remove account: %w", err)
	}
	if phone != "" {
		if _, err = tx.Exec(ctx, `DELETE FROM phone_verification WHERE phone=$1`, phone); err != nil {
			return fmt.Errorf("deletion: remove phone requests: %w", err)
		}
	}
	// Superseded settings events are no longer needed. In-flight RPCs run without
	// row locks and may still finish; receivers fence delayed writes with tombstones.
	if _, err = tx.Exec(ctx, `DELETE FROM outbox_events WHERE topic='user.timezone_changed' AND event_key=$1`, id); err != nil {
		return fmt.Errorf("deletion: remove settings events: %w", err)
	}
	return nil
}

// Start keys may predate account creation and therefore have no OwnerID. The
// library links every secret reply AND cached busy refusal to its request resource.
func (r *Accounts) purgeStartKeys(ctx context.Context, phone string) error {
	q := r.db.Writer(ctx)
	// Lock parents before selecting keys. Busy claims take KEY SHARE, so a link
	// cannot commit behind our key snapshot while we delete its request.
	parents, err := q.Query(ctx, `SELECT id FROM phone_verification WHERE phone=$1 FOR UPDATE`, phone)
	if err != nil {
		return deletionError(err)
	}
	for parents.Next() {
	}
	err = parents.Err()
	parents.Close()
	if err != nil {
		return deletionError(err)
	}
	rows, err := q.Query(ctx, `SELECT 1 FROM idempotency_keys k JOIN phone_verification v ON k.resource_type='phone_verification' AND k.resource_id=v.id::text WHERE v.phone=$1 AND k.scope='00000000-0000-0000-0000-000000000000'::uuid AND k.endpoint=ANY($2::text[]) FOR UPDATE OF k NOWAIT`, phone, verificationKeyEndpoints())
	if err != nil {
		return deletionError(err)
	}
	for rows.Next() {
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return deletionError(err)
	}
	_, err = q.Exec(ctx, `DELETE FROM idempotency_keys k USING phone_verification v WHERE k.resource_type='phone_verification' AND k.resource_id=v.id::text AND v.phone=$1 AND k.scope='00000000-0000-0000-0000-000000000000'::uuid AND k.endpoint=ANY($2::text[])`, phone, verificationKeyEndpoints())
	if err != nil {
		return deletionError(err)
	}
	return nil
}
func deletionError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "55P03" {
		return deletion.ErrReplayBusy
	}
	return fmt.Errorf("deletion: cached replies: %w", err)
}
