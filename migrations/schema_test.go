package migrations_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fin-assist/user/internal/adapter/postgres/migrate"
	"github.com/fin-assist/user/internal/adapter/postgres/migrate/migratetest"
	"github.com/fin-assist/user/migrations"
)

// The tests below check, with real statements against PostgreSQL, what the schema keeps by
// itself — primary keys, unique indexes, foreign keys and their cascades, NOT NULL and
// defaults — and the shape of the expected queries the service will run (docs/schema.md).
// Formats and state rules are the service's code (docs/schema.md, «Инварианты в коде
// сервиса») and are tested with it.

var t0 = time.Date(2026, 10, 19, 22, 12, 0, 0, time.UTC)

const (
	phoneA = "+79001234567"
	phoneB = "+79007654321"
)

func source() migrate.Source {
	return migrate.Source{Name: "service", Table: "schema_migrations", FS: migrations.FS}
}

func config(dsn string) migrate.Config {
	cfg := migrate.DefaultConfig(dsn)
	cfg.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return cfg
}

func schema(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := migratetest.DSN(t)
	if err := migrate.Up(context.Background(), config(dsn), []migrate.Source{source()}); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, dsn
}

func hash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func exec(t *testing.T, db *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// violates asserts that the statement fails on the named constraint or unique index.
func violates(t *testing.T, db *pgxpool.Pool, constraint, sql string, args ...any) {
	t.Helper()
	_, err := db.Exec(context.Background(), sql, args...)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: error %v, want a violation of %s", sql, err, constraint)
	}
	if pgErr.ConstraintName != constraint {
		t.Fatalf("%s: violated %q (%s), want %q", sql, pgErr.ConstraintName, pgErr.Message, constraint)
	}
}

// notNull asserts that the statement fails because the column is NOT NULL.
func notNull(t *testing.T, db *pgxpool.Pool, column, sql string, args ...any) {
	t.Helper()
	_, err := db.Exec(context.Background(), sql, args...)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23502" || pgErr.ColumnName != column {
		t.Fatalf("%s: error %v, want NOT NULL of %s", sql, err, column)
	}
}

func newUser(t *testing.T, db *pgxpool.Pool, phone string) string {
	t.Helper()
	var id string
	if err := db.QueryRow(context.Background(),
		`INSERT INTO app_user (phone) VALUES ($1) RETURNING id::text`, phone).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func documents(t *testing.T, db *pgxpool.Pool) {
	t.Helper()
	exec(t, db, `INSERT INTO consent_document (type, version, title, url) VALUES
		('personal_data', '2026-09-28', 'ПДн', 'https://example.ru/pd/2026-09-28'),
		('assistant', '2026-09-28', 'Помощник', 'https://example.ru/a/2026-09-28'),
		('assistant', '2026-10-20', 'Помощник', 'https://example.ru/a/2026-10-20')`)
	exec(t, db, `INSERT INTO consent_current (type, version) VALUES
		('personal_data', '2026-09-28'), ('assistant', '2026-10-20')`)
}

// querier is a pool or a transaction.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// startVerification is the database part of StartPhoneVerification for a new key, as the
// service runs it (docs/schema.md): release the holder of the phone whose term has passed,
// then insert; an empty RETURNING means the phone is busy, and retry_at comes from the
// holder found by the same index. It returns the id of the new request, or 0 and the
// expires_at of the holder.
func startVerification(ctx context.Context, q querier, phone, token string, now time.Time) (int64, time.Time, error) {
	if _, err := q.Exec(ctx, `
		UPDATE phone_verification SET released_at = $2
		WHERE phone = $1 AND released_at IS NULL AND expires_at <= $2`, phone, now); err != nil {
		return 0, time.Time{}, err
	}
	var id int64
	err := q.QueryRow(ctx, `
		INSERT INTO phone_verification (phone, token_hash, provider, created_at, expires_at)
		VALUES ($1, $2, 'zvonok', $3, $4)
		ON CONFLICT (phone) WHERE released_at IS NULL DO NOTHING
		RETURNING id`, phone, hash(token), now, now.Add(5*time.Minute)).Scan(&id)
	if err == nil {
		return id, time.Time{}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, err
	}
	var holder time.Time
	if err := q.QueryRow(ctx, `
		SELECT expires_at FROM phone_verification WHERE phone = $1 AND released_at IS NULL`, phone).Scan(&holder); err != nil {
		return 0, time.Time{}, err
	}
	return 0, holder, nil
}

// verification starts a request on db and fails the test unless it was created.
func verification(t *testing.T, db *pgxpool.Pool, phone, token string, now time.Time) int64 {
	t.Helper()
	id, _, err := startVerification(context.Background(), db, phone, token, now)
	if err != nil {
		t.Fatal(err)
	}
	if id == 0 {
		t.Fatalf("request of %s at %v: the phone is busy", phone, now)
	}
	return id
}

func TestPhoneIsUniqueAmongUsers(t *testing.T) {
	db, _ := schema(t)
	id := newUser(t, db, phoneA)
	newUser(t, db, phoneB)

	violates(t, db, "app_user_phone_key", `INSERT INTO app_user (phone) VALUES ($1)`, phoneA)
	notNull(t, db, "phone", `INSERT INTO app_user (phone) VALUES (NULL)`)

	// The expected lookup "live user by phone" finds the user; after the account is
	// deleted the phone is free for a new registration.
	var got string
	if err := db.QueryRow(context.Background(),
		`SELECT id::text FROM app_user WHERE phone = $1`, phoneA).Scan(&got); err != nil || got != id {
		t.Fatalf("lookup by phone: %q, %v; want %q", got, err, id)
	}
	exec(t, db, `DELETE FROM app_user WHERE id = $1`, id)
	if again := newUser(t, db, phoneA); again == id {
		t.Fatal("a new user got the id of the deleted one")
	}
}

func TestProfileDefaults(t *testing.T) {
	db, _ := schema(t)
	id := newUser(t, db, phoneA)

	var theme, tz string
	var version int
	var created, updated time.Time
	if err := db.QueryRow(context.Background(),
		`SELECT theme, timezone, settings_version, created_at, updated_at FROM app_user WHERE id = $1`, id).
		Scan(&theme, &tz, &version, &created, &updated); err != nil {
		t.Fatal(err)
	}
	if theme != "system" || tz != "Europe/Moscow" || version != 1 || !updated.Equal(created) {
		t.Fatalf("defaults %q %q %d %v %v, want system Europe/Moscow 1 and updated_at = created_at",
			theme, tz, version, created, updated)
	}
	// The id is a UUIDv7 (Confluence «Идентификаторы»: IDs that leave the database).
	var uuidVersion int
	if err := db.QueryRow(context.Background(),
		`SELECT uuid_extract_version(id) FROM app_user WHERE id = $1`, id).Scan(&uuidVersion); err != nil || uuidVersion != 7 {
		t.Fatalf("uuid version %d, %v; want 7", uuidVersion, err)
	}

	// SetUserTimezone: a new time zone bumps the version in the same statement; updated_at
	// is set by the service.
	if err := db.QueryRow(context.Background(), `
		UPDATE app_user SET timezone = $2, settings_version = settings_version + 1, updated_at = $3
		WHERE id = $1 RETURNING settings_version`, id, "Asia/Yekaterinburg", t0).Scan(&version); err != nil || version != 2 {
		t.Fatalf("settings_version %d, %v; want 2", version, err)
	}
	notNull(t, db, "theme", `UPDATE app_user SET theme = NULL WHERE id = $1`, id)
}

func TestConsents(t *testing.T) {
	db, _ := schema(t)
	documents(t, db)
	id := newUser(t, db, phoneA)

	violates(t, db, "consent_current_document_fkey",
		`UPDATE consent_current SET version = '2099-01-01' WHERE type = 'assistant'`)
	violates(t, db, "user_consent_document_fkey",
		`INSERT INTO user_consent (user_id, type, version, granted_at) VALUES ($1, 'assistant', '2099-01-01', $2)`, id, t0)

	exec(t, db, `INSERT INTO user_consent (user_id, type, version, granted_at) VALUES ($1, 'assistant', '2026-09-28', $2)`, id, t0)

	// Documents: a version in use stays, the current version is one per type.
	violates(t, db, "user_consent_document_fkey",
		`DELETE FROM consent_document WHERE type = 'assistant' AND version = '2026-09-28'`)
	violates(t, db, "consent_current_document_fkey",
		`DELETE FROM consent_document WHERE type = 'assistant' AND version = '2026-10-20'`)
	violates(t, db, "consent_current_pkey",
		`INSERT INTO consent_current (type, version) VALUES ('assistant', '2026-09-28')`)
	violates(t, db, "consent_document_pkey", `
		INSERT INTO consent_document (type, version, title, url)
		VALUES ('assistant', '2026-10-20', 'Помощник', 'https://example.ru/a/2026-10-20')`)

	// One active grant per type: a second grant without revoking the first fails.
	violates(t, db, "user_consent_active_key",
		`INSERT INTO user_consent (user_id, type, version, granted_at) VALUES ($1, 'assistant', '2026-10-20', $2)`, id, t0.Add(time.Hour))

	// GetAssistantConsentStatus: the active grant and the current version in one query.
	status := func() (granted bool, version *string, current string) {
		t.Helper()
		err := db.QueryRow(context.Background(), `
			SELECT c.version IS NOT NULL, c.version, d.version
			FROM consent_current d
			LEFT JOIN user_consent c
			       ON c.user_id = $1 AND c.type = d.type AND c.revoked_at IS NULL
			WHERE d.type = 'assistant'`, id).Scan(&granted, &version, &current)
		if err != nil {
			t.Fatal(err)
		}
		return granted, version, current
	}
	if granted, version, current := status(); !granted || version == nil || *version != "2026-09-28" || current != "2026-10-20" {
		t.Fatalf("status %v %v %q; want granted 2026-09-28, current 2026-10-20", granted, version, current)
	}

	// Revoke, then grant the current version: history keeps both rows.
	exec(t, db, `UPDATE user_consent SET revoked_at = $2 WHERE user_id = $1 AND type = 'assistant' AND revoked_at IS NULL`, id, t0.Add(time.Hour))
	if granted, version, _ := status(); granted || version != nil {
		t.Fatalf("after revoke: granted %v, version %v", granted, version)
	}
	exec(t, db, `INSERT INTO user_consent (user_id, type, version, granted_at) VALUES ($1, 'assistant', '2026-10-20', $2)`, id, t0.Add(2*time.Hour))
	var rows int
	if err := db.QueryRow(context.Background(), `SELECT count(*) FROM user_consent WHERE user_id = $1`, id).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("%d rows of history, %v; want 2", rows, err)
	}
}

func TestOneHoldingVerificationPerPhone(t *testing.T) {
	db, _ := schema(t)
	ctx := context.Background()

	first := verification(t, db, phoneA, "vt-1", t0)

	// A second holding request of the phone: the partial unique index refuses it.
	violates(t, db, "phone_verification_phone_held_key", `
		INSERT INTO phone_verification (phone, token_hash, provider, created_at, expires_at)
		VALUES ($1, $2, 'zvonok', $3, $4)`, phoneA, hash("vt-x"), t0.Add(time.Minute), t0.Add(6*time.Minute))

	// Busy until expires_at, closed or not: the service gets no row and the holder's term.
	exec(t, db, `UPDATE phone_verification SET closed_at = $2 WHERE id = $1`, first, t0.Add(time.Minute))
	id, holder, err := startVerification(ctx, db, phoneA, "vt-2", t0.Add(4*time.Minute+59*time.Second))
	if err != nil || id != 0 || !holder.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("busy phone: id %d, holder %v, %v; want 0 and %v", id, holder, err, t0.Add(5*time.Minute))
	}
	// Another phone in the same minutes is independent.
	verification(t, db, phoneB, "vt-3", t0.Add(time.Minute))

	// At expires_at the holder is released in the same transaction and the request passes.
	second := verification(t, db, phoneA, "vt-4", t0.Add(5*time.Minute))
	var released *time.Time
	if err := db.QueryRow(ctx, `SELECT released_at FROM phone_verification WHERE id = $1`, first).Scan(&released); err != nil ||
		released == nil || !released.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("released_at of the first %v, %v; want %v", released, err, t0.Add(5*time.Minute))
	}
	if second == first {
		t.Fatal("the same id twice")
	}

	// The insert names its conflict target: a repeated token hash on a free phone stays an
	// error instead of looking like a busy phone.
	violates(t, db, "phone_verification_token_hash_key", `
		INSERT INTO phone_verification (phone, token_hash, provider, created_at, expires_at)
		VALUES ($1, $2, 'zvonok', $3, $4)
		ON CONFLICT (phone) WHERE released_at IS NULL DO NOTHING`,
		"+79005550000", hash("vt-1"), t0.Add(time.Hour), t0.Add(time.Hour+5*time.Minute))
	notNull(t, db, "token_hash", `
		INSERT INTO phone_verification (phone, provider, created_at, expires_at)
		VALUES ('+79005550001', 'zvonok', $1, $2)`, t0, t0.Add(5*time.Minute))
}

func TestConcurrentVerificationsOfOnePhone(t *testing.T) {
	db, _ := schema(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// An expired holder, so both transactions first release it and then race to insert.
	verification(t, db, phoneA, "vt-0", t0)
	now := t0.Add(10 * time.Minute)

	const n = 8
	ids := make(chan int64, n)
	errs := make(chan error, n)
	var start sync.WaitGroup
	start.Add(1)
	for i := range n {
		go func() {
			start.Wait()
			err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
				id, _, err := startVerification(ctx, tx, phoneA, "vt-"+strconv.Itoa(i+1), now)
				ids <- id
				return err
			})
			errs <- err
		}()
	}
	start.Done()
	created := 0
	for range n {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if <-ids != 0 {
			created++
		}
	}
	var holding int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM phone_verification WHERE phone = $1 AND released_at IS NULL`, phoneA).Scan(&holding); err != nil {
		t.Fatal(err)
	}
	if created != 1 || holding != 1 {
		t.Fatalf("%d created, %d holding; want exactly one", created, holding)
	}
}

func TestVerificationLookups(t *testing.T) {
	db, _ := schema(t)
	ctx := context.Background()
	id := verification(t, db, phoneA, "vt-1", t0)
	exec(t, db, `UPDATE phone_verification SET provider_call_id = 'call-1' WHERE id = $1`, id)

	// The read path of WatchPhoneVerification: the request by the hash of its token.
	var found int64
	if err := db.QueryRow(ctx, `SELECT id FROM phone_verification WHERE token_hash = $1`, hash("vt-1")).Scan(&found); err != nil || found != id {
		t.Fatalf("by token hash: %d, %v; want %d", found, err, id)
	}
	// The postback by call_id: one request per provider call.
	other := verification(t, db, phoneB, "vt-2", t0)
	violates(t, db, "phone_verification_provider_call_key",
		`UPDATE phone_verification SET provider_call_id = 'call-1' WHERE id = $1`, other)
	// The same call_id of another provider is another call.
	exec(t, db, `UPDATE phone_verification SET provider = 'fake', provider_call_id = 'call-1' WHERE id = $1`, other)

	// A new user: the registration token hash is unique, Register finds the request by it
	// and closes it only once.
	exec(t, db, `
		UPDATE phone_verification SET verified_at = $2, is_new_user = true, registration_token_hash = $3,
		       result_ciphertext = $4, result_key_id = 'k1'
		WHERE id = $1`, other, t0.Add(time.Minute), hash("reg-2"), bytes.Repeat([]byte{1}, 64))
	violates(t, db, "phone_verification_registration_token_hash_key", `
		UPDATE phone_verification SET verified_at = $2, is_new_user = true, registration_token_hash = $3
		WHERE id = $1`, id, t0.Add(time.Minute), hash("reg-2"))
	registered := newUser(t, db, phoneB)
	closeByRegistration := func() int64 {
		t.Helper()
		tag, err := db.Exec(ctx, `
			UPDATE phone_verification SET user_id = $2, closed_at = $3, result_ciphertext = NULL, result_key_id = NULL
			WHERE registration_token_hash = $1 AND closed_at IS NULL AND verified_at > $3::timestamptz - interval '15 minutes'`,
			hash("reg-2"), registered, t0.Add(3*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		return tag.RowsAffected()
	}
	if n := closeByRegistration(); n != 1 {
		t.Fatalf("register closed %d requests, want 1", n)
	}
	if n := closeByRegistration(); n != 0 {
		t.Fatalf("a second register closed %d requests, want 0", n)
	}
}

// The verification and its notification are one transaction of the service (there is no
// trigger): WatchPhoneVerification sees the notification only after the commit, and a
// rolled-back verification notifies nobody.
func TestVerificationNotifiesOnCommit(t *testing.T) {
	db, dsn := schema(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	user := newUser(t, db, phoneA)
	id := verification(t, db, phoneA, "vt-1", t0)

	listener, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close(context.Background()) }()
	if _, err := listener.Exec(ctx, `LISTEN phone_verification`); err != nil {
		t.Fatal(err)
	}
	verify := func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE phone_verification SET provider_call_id = 'c', verified_at = $2, is_new_user = false, user_id = $3
			WHERE id = $1 AND verified_at IS NULL`, id, t0.Add(time.Minute), user); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT pg_notify('phone_verification', $1::bigint::text)`, id)
		return err
	}

	boom := errors.New("boom")
	if err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if err := verify(tx); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("rolled back with %v", err)
	}
	short, cancelShort := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancelShort()
	if n, err := listener.WaitForNotification(short); err == nil {
		t.Fatalf("notification %q from a rolled-back verification", n.Payload)
	}

	if err := pgx.BeginFunc(ctx, db, verify); err != nil {
		t.Fatal(err)
	}
	n, err := listener.WaitForNotification(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n.Channel != "phone_verification" || n.Payload != strconv.FormatInt(id, 10) {
		t.Fatalf("notification %q %q, want phone_verification %d", n.Channel, n.Payload, id)
	}
}

func session(t *testing.T, db *pgxpool.Pool, user, token string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(context.Background(),
		`INSERT INTO auth_session (user_id) VALUES ($1) RETURNING id`, user).Scan(&id); err != nil {
		t.Fatal(err)
	}
	exec(t, db, `INSERT INTO refresh_token (token_hash, session_id, generation, issued_at, expires_at)
		VALUES ($1, $2, 1, $3, $4)`, hash(token), id, t0, t0.AddDate(0, 0, 60))
	return id
}

// rotate is RefreshTokens after its idempotency step, in one transaction: the presented
// link locked by its hash; a live link is replaced by the next one, a rotated link revokes
// the whole session. It returns "rotated", "reuse", "revoked" or "unknown".
func rotate(ctx context.Context, db *pgxpool.Pool, presented, next string, now time.Time) (string, error) {
	var outcome string
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		var session, generation int64
		var rotated, revoked *time.Time
		err := tx.QueryRow(ctx, `
			SELECT r.session_id, r.generation, r.rotated_at, s.revoked_at
			FROM refresh_token r JOIN auth_session s ON s.id = r.session_id
			WHERE r.token_hash = $1 AND r.expires_at > $2
			FOR UPDATE OF r, s`, hash(presented), now).Scan(&session, &generation, &rotated, &revoked)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			outcome = "unknown"
			return nil
		case err != nil:
			return err
		case revoked != nil:
			outcome = "revoked"
			return nil
		case rotated != nil:
			outcome = "reuse"
			_, err := tx.Exec(ctx, `UPDATE auth_session SET revoked_at = $2 WHERE id = $1`, session, now)
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE refresh_token SET rotated_at = $2 WHERE token_hash = $1`, hash(presented), now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO refresh_token (token_hash, session_id, generation, issued_at, expires_at)
			VALUES ($1, $2, $3, $4, $5)`, hash(next), session, generation+1, now, now.AddDate(0, 0, 60)); err != nil {
			return err
		}
		outcome = "rotated"
		return nil
	})
	return outcome, err
}

func TestRefreshRotationAndReuse(t *testing.T) {
	db, _ := schema(t)
	ctx := context.Background()
	user := newUser(t, db, phoneA)
	s := session(t, db, user, "rt-1")

	// The schema refuses a second live link by itself.
	violates(t, db, "refresh_token_one_live_key", `
		INSERT INTO refresh_token (token_hash, session_id, generation, issued_at, expires_at)
		VALUES ($1, $2, 2, $3, $4)`, hash("rt-x"), s, t0, t0.Add(time.Hour))
	violates(t, db, "refresh_token_session_fkey", `
		INSERT INTO refresh_token (token_hash, session_id, generation, issued_at, expires_at)
		VALUES ($1, -1, 1, $2, $3)`, hash("rt-y"), t0, t0.Add(time.Hour))

	// A reused generation, checked apart from the one-live-link index: after rt-1 -> rt-2 the
	// duplicate is inserted already rotated, so only (session_id, generation) can refuse it.
	if got, err := rotate(ctx, db, "rt-1", "rt-2", t0.Add(time.Minute)); err != nil || got != "rotated" {
		t.Fatalf("rt-1: %s, %v; want rotated", got, err)
	}
	violates(t, db, "refresh_token_generation_key", `
		INSERT INTO refresh_token (token_hash, session_id, generation, issued_at, expires_at, rotated_at)
		VALUES ($1, $2, 1, $3, $4, $3)`, hash("rt-dup"), s, t0, t0.Add(time.Hour))
	// The same token hash twice is the primary key.
	violates(t, db, "refresh_token_pkey", `
		INSERT INTO refresh_token (token_hash, session_id, generation, issued_at, expires_at, rotated_at)
		VALUES ($1, $2, 9, $3, $4, $3)`, hash("rt-1"), s, t0, t0.Add(time.Hour))

	steps := []struct{ presented, next, want string }{
		{"rt-2", "rt-3", "rotated"},
		{"rt-1", "rt-x", "reuse"},   // an old link: the whole chain is revoked
		{"rt-3", "rt-4", "revoked"}, // the live link of the revoked chain no longer works
		{"rt-404", "rt-5", "unknown"},
	}
	for _, st := range steps {
		got, err := rotate(ctx, db, st.presented, st.next, t0.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if got != st.want {
			t.Fatalf("%s: %s, want %s", st.presented, got, st.want)
		}
	}
	var links int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM refresh_token WHERE session_id = $1`, s).Scan(&links); err != nil || links != 3 {
		t.Fatalf("%d links, %v; want 3 (the reuse added none)", links, err)
	}
	// An expired link is unknown, not reuse.
	session(t, db, user, "rt-old")
	if got, err := rotate(ctx, db, "rt-old", "rt-new", t0.AddDate(0, 0, 61)); err != nil || got != "unknown" {
		t.Fatalf("expired link: %s, %v; want unknown", got, err)
	}
}

func TestConcurrentRotationOfOneLink(t *testing.T) {
	db, _ := schema(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	user := newUser(t, db, phoneA)
	session(t, db, user, "rt-1")

	// Two requests with the same refresh token and different idempotency keys: the second
	// waits on the row lock, then sees the link rotated and revokes the chain.
	type result struct {
		outcome string
		err     error
	}
	results := make(chan result, 2)
	for _, next := range []string{"rt-a", "rt-b"} {
		go func() {
			o, err := rotate(ctx, db, "rt-1", next, t0.Add(time.Minute))
			results <- result{o, err}
		}()
	}
	got := map[string]int{}
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		got[r.outcome]++
	}
	if got["rotated"] != 1 || got["reuse"] != 1 {
		t.Fatalf("outcomes %v, want one rotated and one reuse", got)
	}
}

func TestAccountDeletionRemovesPersonalData(t *testing.T) {
	db, _ := schema(t)
	ctx := context.Background()
	documents(t, db)
	user := newUser(t, db, phoneA)
	keep := newUser(t, db, phoneB)
	exec(t, db, `INSERT INTO user_consent (user_id, type, version, granted_at) VALUES ($1, 'personal_data', '2026-09-28', $2)`, user, t0)
	id := verification(t, db, phoneA, "vt-1", t0)
	exec(t, db, `UPDATE phone_verification SET provider_call_id = 'c', verified_at = $2, is_new_user = false, user_id = $3 WHERE id = $1`,
		id, t0.Add(time.Minute), user)
	s := session(t, db, user, "rt-1")
	exec(t, db, `UPDATE auth_session SET phone_verification_id = $2 WHERE id = $1`, s, id)
	session(t, db, keep, "rt-keep")

	exec(t, db, `DELETE FROM app_user WHERE id = $1`, user)

	for _, q := range []string{
		`SELECT count(*) FROM user_consent WHERE user_id = $1`,
		`SELECT count(*) FROM phone_verification WHERE user_id = $1`,
		`SELECT count(*) FROM auth_session WHERE user_id = $1`,
	} {
		var n int
		if err := db.QueryRow(ctx, q, user).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s: %d rows, %v; want 0", q, n, err)
		}
	}
	var links int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM refresh_token`).Scan(&links); err != nil || links != 1 {
		t.Fatalf("%d refresh links, %v; want 1 (another user's)", links, err)
	}
	if got, err := rotate(ctx, db, "rt-1", "rt-2", t0.Add(time.Minute)); err != nil || got != "unknown" {
		t.Fatalf("refresh after deletion: %s, %v; want unknown", got, err)
	}
	// A late write for the deleted user (a transaction that read the user before the
	// deletion) fails on the foreign key instead of resurrecting data.
	violates(t, db, "auth_session_user_fkey", `INSERT INTO auth_session (user_id) VALUES ($1)`, user)
	violates(t, db, "user_consent_user_fkey",
		`INSERT INTO user_consent (user_id, type, version, granted_at) VALUES ($1, 'assistant', '2026-10-20', $2)`, user, t0)
	late := verification(t, db, phoneA, "vt-late", t0.Add(time.Hour))
	violates(t, db, "phone_verification_user_fkey", `
		UPDATE phone_verification SET provider_call_id = 'c-late', verified_at = $2, is_new_user = false, user_id = $3
		WHERE id = $1`, late, t0.Add(time.Hour+time.Minute), user)
}

// Cleanup of an old verification request keeps the session it signed in: the link becomes
// NULL (ON DELETE SET NULL), the session and its refresh links stay.
func TestVerificationCleanupKeepsTheSession(t *testing.T) {
	db, _ := schema(t)
	ctx := context.Background()
	user := newUser(t, db, phoneA)
	id := verification(t, db, phoneA, "vt-1", t0)
	exec(t, db, `UPDATE phone_verification SET provider_call_id = 'c', verified_at = $2, is_new_user = false, user_id = $3 WHERE id = $1`,
		id, t0.Add(time.Minute), user)
	s := session(t, db, user, "rt-1")
	exec(t, db, `UPDATE auth_session SET phone_verification_id = $2 WHERE id = $1`, s, id)

	exec(t, db, `DELETE FROM phone_verification WHERE id = $1`, id)

	var link *int64
	if err := db.QueryRow(ctx, `SELECT phone_verification_id FROM auth_session WHERE id = $1`, s).Scan(&link); err != nil || link != nil {
		t.Fatalf("session after cleanup: link %v, %v; want the session with NULL", link, err)
	}
	if got, err := rotate(ctx, db, "rt-1", "rt-2", t0.Add(2*time.Minute)); err != nil || got != "rotated" {
		t.Fatalf("refresh after cleanup: %s, %v; want rotated", got, err)
	}
}

// DeleteAccount (owner's decision of 19.10.2026, docs/schema.md question 6): in one
// transaction the user's row goes, its consents, sessions and requests with its user_id go
// by cascade, and every other request of the phone — unverified, or verified for a new
// user who never registered — goes by DELETE ... WHERE phone. Requests of other phones stay.
func TestAccountDeletionRemovesAllRequestsOfThePhone(t *testing.T) {
	db, _ := schema(t)
	ctx := context.Background()
	user := newUser(t, db, phoneA)
	ciphertext := bytes.Repeat([]byte{1}, 64)

	// Verified for a new user, the result still deliverable, no Register: no user_id.
	unregistered := verification(t, db, phoneA, "vt-0", t0)
	exec(t, db, `
		UPDATE phone_verification
		SET provider_call_id = 'c0', verified_at = $2, is_new_user = true, registration_token_hash = $3,
		    result_ciphertext = $4, result_key_id = 'k1'
		WHERE id = $1`, unregistered, t0.Add(time.Minute), hash("reg-0"), ciphertext)
	// Verified for a new user and closed by Register: user_id is set by the registration.
	registered := verification(t, db, phoneA, "vt-1", t0.Add(10*time.Minute))
	exec(t, db, `
		UPDATE phone_verification
		SET provider_call_id = 'c1', verified_at = $2, is_new_user = true, registration_token_hash = $3,
		    user_id = $4, closed_at = $5
		WHERE id = $1`, registered, t0.Add(11*time.Minute), hash("reg-1"), user, t0.Add(12*time.Minute))
	// Verified for the existing user: user_id is set by the verification.
	signedIn := verification(t, db, phoneA, "vt-2", t0.Add(20*time.Minute))
	exec(t, db, `UPDATE phone_verification SET provider_call_id = 'c2', verified_at = $2, is_new_user = false, user_id = $3 WHERE id = $1`,
		signedIn, t0.Add(21*time.Minute), user)
	verification(t, db, phoneA, "vt-3", t0.Add(30*time.Minute)) // expired, no user_id
	verification(t, db, phoneA, "vt-4", t0.Add(40*time.Minute)) // pending, holds the phone
	// Requests of other phones, one of them verified for a new user.
	otherPending := verification(t, db, phoneB, "vt-5", t0.Add(40*time.Minute))
	otherVerified := verification(t, db, "+79005550000", "vt-6", t0)
	exec(t, db, `
		UPDATE phone_verification SET provider_call_id = 'c6', verified_at = $2, is_new_user = true, registration_token_hash = $3
		WHERE id = $1`, otherVerified, t0.Add(time.Minute), hash("reg-6"))

	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM app_user WHERE id = $1`, user); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM phone_verification WHERE phone = $1`, phoneA)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	var ofPhone int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM phone_verification WHERE phone = $1`, phoneA).Scan(&ofPhone); err != nil || ofPhone != 0 {
		t.Fatalf("%d requests of the deleted user's phone, %v; want 0", ofPhone, err)
	}
	rows, err := db.Query(ctx, `SELECT id FROM phone_verification ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	left, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 || left[0] != otherPending || left[1] != otherVerified {
		t.Fatalf("requests left %v, want the other phones' %d and %d", left, otherPending, otherVerified)
	}
	// The phone is free again: a new request of it is accepted at once.
	verification(t, db, phoneA, "vt-7", t0.Add(41*time.Minute))
}

func TestRateLimitWindow(t *testing.T) {
	db, _ := schema(t)
	ctx := context.Background()

	// One statement per attempt: the counter of the current window, created on first use.
	hit := func(subject string, at time.Time) int64 {
		t.Helper()
		start := at.Truncate(time.Hour)
		var hits int64
		if err := db.QueryRow(ctx, `
			INSERT INTO rate_limit_window (scope, subject, window_start, window_end, hits)
			VALUES ('verification_phone_1h', $1, $2, $3, 1)
			ON CONFLICT (scope, subject, window_start) DO UPDATE SET hits = rate_limit_window.hits + 1
			RETURNING hits`, subject, start, start.Add(time.Hour)).Scan(&hits); err != nil {
			t.Fatal(err)
		}
		return hits
	}
	for want := int64(1); want <= 3; want++ {
		if got := hit(phoneA, t0.Add(time.Duration(want)*time.Minute)); got != want {
			t.Fatalf("hit %d counted %d", want, got)
		}
	}
	if got := hit(phoneB, t0); got != 1 {
		t.Fatalf("another subject counted %d, want 1", got)
	}
	if got := hit(phoneA, t0.Add(time.Hour)); got != 1 {
		t.Fatalf("the next window counted %d, want 1", got)
	}
}

// The schema has no CHECK or EXCLUDE constraints, triggers or functions (owner's decision of
// 19.10.2026, docs/schema.md): such rules are the service's code. The test keeps a new
// migration from bringing them back unnoticed.
func TestSchemaHasNoRulesOrLogic(t *testing.T) {
	db, _ := schema(t)
	var checks, triggers, functions int
	if err := db.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace
		        WHERE n.nspname = 'public' AND c.contype IN ('c', 'x')),
		       (SELECT count(*) FROM pg_trigger t JOIN pg_class r ON r.oid = t.tgrelid
		        JOIN pg_namespace n ON n.oid = r.relnamespace
		        WHERE n.nspname = 'public' AND NOT t.tgisinternal),
		       (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		        WHERE n.nspname = 'public')`).Scan(&checks, &triggers, &functions); err != nil {
		t.Fatal(err)
	}
	if checks != 0 || triggers != 0 || functions != 0 {
		t.Fatalf("%d CHECK/EXCLUDE, %d triggers, %d functions; want none", checks, triggers, functions)
	}
}

// The down file drops the tables and leaves nothing behind; up applies again.
func TestDownLeavesNothing(t *testing.T) {
	db, dsn := schema(t)
	ctx := context.Background()
	if err := migrate.Down(ctx, config(dsn), source(), 1); err != nil {
		t.Fatal(err)
	}
	var tables int
	if err := db.QueryRow(ctx, `
		SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename <> 'schema_migrations'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 0 {
		t.Fatalf("after down: %d tables; want 0", tables)
	}
	if err := migrate.Up(ctx, config(dsn), []migrate.Source{source()}); err != nil {
		t.Fatalf("up again: %v", err)
	}
}
