package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/adapter/postgres/migrate/migratetest"
	"github.com/fin-assist/user/internal/reqctx"
)

func open(t *testing.T) *postgres.DB {
	t.Helper()
	dsn := migratetest.DSN(t)
	db, err := postgres.Open(context.Background(), postgres.Options{PrimaryDSN: dsn, AppName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	if _, err := db.Writer(context.Background()).Exec(context.Background(), `CREATE TABLE item (id int PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	return db
}

func count(t *testing.T, db *postgres.DB) int {
	t.Helper()
	var n int
	if err := db.Reader(context.Background()).QueryRow(context.Background(), `SELECT count(*) FROM item`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func insert(ctx context.Context, db *postgres.DB, id int) error {
	_, err := db.Writer(ctx).Exec(ctx, `INSERT INTO item (id) VALUES ($1)`, id)
	return err
}

func TestIntegrationRunCommitsAndRollsBack(t *testing.T) {
	db := open(t)
	ctx := context.Background()

	if err := db.Run(ctx, func(ctx context.Context) error { return insert(ctx, db, 1) }); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	err := db.Run(ctx, func(ctx context.Context) error {
		if err := insert(ctx, db, 2); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("error %v, want boom", err)
	}
	if n := count(t, db); n != 1 {
		t.Fatalf("%d rows, want 1: the failed Run must roll back", n)
	}
}

func TestIntegrationRunRollsBackOnPanic(t *testing.T) {
	db := open(t)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic must propagate")
			}
		}()
		_ = db.Run(context.Background(), func(ctx context.Context) error {
			_ = insert(ctx, db, 1)
			panic("handler bug")
		})
	}()
	if n := count(t, db); n != 0 {
		t.Fatalf("%d rows after panic, want 0", n)
	}
}

func TestIntegrationNestedRunJoinsOuter(t *testing.T) {
	db := open(t)
	err := db.Run(context.Background(), func(ctx context.Context) error {
		if err := db.Run(ctx, func(ctx context.Context) error { return insert(ctx, db, 1) }); err != nil {
			return err
		}
		return errors.New("outer fails after inner succeeded")
	})
	if err == nil {
		t.Fatal("want the outer error")
	}
	if n := count(t, db); n != 0 {
		t.Fatalf("%d rows, want 0: the inner Run is part of the outer transaction", n)
	}
}

func TestIntegrationTxIsVisibleOnlyInsideRun(t *testing.T) {
	db := open(t)
	if _, ok := db.Tx(context.Background()); ok {
		t.Fatal("no transaction outside Run")
	}
	_ = db.Run(context.Background(), func(ctx context.Context) error {
		tx, ok := db.Tx(ctx)
		if !ok {
			t.Fatal("transaction expected inside Run")
		}
		// The library writes through the same transaction as the effect.
		_, err := tx.Exec(ctx, `INSERT INTO item (id) VALUES (7)`)
		return err
	})
	if n := count(t, db); n != 1 {
		t.Fatalf("%d rows, want 1", n)
	}
}

func TestIntegrationReaderUsesTransactionAndPrimaryHint(t *testing.T) {
	db := open(t)
	// Reader inside Run sees the uncommitted write of the same transaction.
	_ = db.Run(context.Background(), func(ctx context.Context) error {
		if err := insert(ctx, db, 1); err != nil {
			return err
		}
		var n int
		if err := db.Reader(ctx).QueryRow(ctx, `SELECT count(*) FROM item`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("reader in tx: %d, %v", n, err)
		}
		return nil
	})
	if q := db.Reader(reqctx.WithReadPrimary(context.Background())); q != db.Primary() {
		t.Fatal("x-read-primary must read from the primary pool")
	}
}

func TestIntegrationOpenDoesNotLeakDSN(t *testing.T) {
	_, err := postgres.Open(context.Background(), postgres.Options{PrimaryDSN: "postgres://u:secret@%zz/db"})
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("error %v must not contain the password", err)
	}
}
