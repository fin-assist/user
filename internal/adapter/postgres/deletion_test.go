package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	idempotency "github.com/fin-assist/go-idempotency"
	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fin-assist/user/internal/adapter/postgres/migrate"
	"github.com/fin-assist/user/internal/deletion"
)

func TestIntegrationDeletionRepositoryRequiresTransactionAndAvoidsReplayDeadlock(t *testing.T) {
	db, _, _ := profileDB(t)
	if err := migrate.Up(t.Context(), migrate.DefaultConfig(db.primary.Config().ConnConfig.ConnString()), []migrate.Source{{Name: idempotency.MigrationsName, Table: idempotency.MigrationsTable, FS: idempotency.Migrations}}); err != nil {
		t.Fatal(err)
	}
	repo := NewAccounts(db)
	if err := repo.Remove(t.Context(), profileID); err == nil {
		t.Fatal("write outside transaction")
	}
	key := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	if _, err := db.Primary().Exec(t.Context(), `INSERT INTO idempotency_keys(scope,endpoint,key,request_hash,owner_id,expires_at) VALUES('00000000-0000-0000-0000-000000000000','auth',$1,$2,$3,$4)`, key, make([]byte, 32), profileID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	locked, err := db.Primary().Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = locked.Rollback(context.Background()) }()
	if _, err = locked.Exec(t.Context(), `SELECT 1 FROM idempotency_keys WHERE key=$1 FOR UPDATE`, key); err != nil {
		t.Fatal(err)
	}
	guard, err := tombstone.New(db, db.Primary(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err = db.Run(ctx, func(ctx context.Context) error { return guard.Purge(ctx, profileID, repo.Remove) })
	if !errors.Is(err, deletion.ErrReplayBusy) {
		t.Fatal("waited with guard instead of rolling back", err)
	}
	// Rollback releases the same advisory lock a cached replay needs; its account
	// and cached response remain intact until deletion can retry successfully.
	if err = db.Run(ctx, func(ctx context.Context) error {
		tx, _ := db.Tx(ctx)
		var free bool
		if err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(hashtextextended($1,110))`, profileID).Scan(&free); err != nil || !free {
			t.Fatal("deletion retained guard", free, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.Primary().QueryRow(ctx, `SELECT count(*) FROM app_user WHERE id=$1`, profileID).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if err = locked.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = db.Run(ctx, func(ctx context.Context) error { return guard.Purge(ctx, profileID, repo.Remove) }); err != nil {
		t.Fatal(err)
	}
	if err = db.Primary().QueryRow(ctx, `SELECT count(*) FROM idempotency_keys WHERE owner_id=$1`, profileID).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
	if err = db.Primary().QueryRow(ctx, `SELECT count(*) FROM app_user WHERE id=$1`, otherProfileID).Scan(&count); err != nil || count != 1 {
		t.Fatal("other account removed", count, err)
	}
	if !errors.Is(deletionError(&pgconn.PgError{Code: "55P03"}), deletion.ErrReplayBusy) {
		t.Fatal("lock error not classified")
	}
	if errors.Is(deletionError(errors.New("other error")), deletion.ErrReplayBusy) {
		t.Fatal("other failure retried")
	}
}
