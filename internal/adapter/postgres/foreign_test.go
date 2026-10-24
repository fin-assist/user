package postgres_test

import (
	"context"
	"errors"
	"testing"

	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/user/internal/adapter/postgres"
)

func TestIntegrationForeignTransactionCannotFallBackToPool(t *testing.T) {
	db, foreign := open(t), open(t)
	err := db.Run(context.Background(), func(ctx context.Context) error {
		for _, q := range []postgres.Querier{foreign.Writer(ctx), foreign.Reader(ctx)} {
			if _, err := q.Exec(ctx, `INSERT INTO item VALUES (9)`); !errors.Is(err, pgstore.ErrForeignTransaction) {
				t.Fatalf("Exec: %v", err)
			}
			rows, err := q.Query(ctx, `SELECT 1`)
			if rows != nil {
				rows.Close()
				t.Fatal("foreign Query returned rows")
			}
			if !errors.Is(err, pgstore.ErrForeignTransaction) {
				t.Fatalf("Query: %v", err)
			}
			if err := q.QueryRow(ctx, `SELECT 1`).Scan(new(int)); !errors.Is(err, pgstore.ErrForeignTransaction) {
				t.Fatalf("QueryRow: %v", err)
			}
			batch := q.SendBatch(ctx, &pgx.Batch{})
			if _, err := batch.Exec(); !errors.Is(err, pgstore.ErrForeignTransaction) {
				t.Fatalf("batch Exec: %v", err)
			}
			rows, err = batch.Query()
			if rows != nil {
				rows.Close()
				t.Fatal("foreign batch Query returned rows")
			}
			if !errors.Is(err, pgstore.ErrForeignTransaction) {
				t.Fatalf("batch Query: %v", err)
			}
			if err := batch.QueryRow().Scan(new(int)); !errors.Is(err, pgstore.ErrForeignTransaction) {
				t.Fatalf("batch QueryRow: %v", err)
			}
			if err := batch.Close(); !errors.Is(err, pgstore.ErrForeignTransaction) {
				t.Fatalf("batch Close: %v", err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count(t, foreign) != 0 {
		t.Fatal("foreign pool received an autocommit write")
	}
}

func TestIntegrationRunWithPreservesTransactionOptions(t *testing.T) {
	db := open(t)
	err := db.RunWith(context.Background(), pgx.TxOptions{IsoLevel: pgx.Serializable, AccessMode: pgx.ReadOnly}, func(ctx context.Context) error {
		var isolation, readOnly string
		if err := db.Reader(ctx).QueryRow(ctx, `SHOW transaction_isolation`).Scan(&isolation); err != nil {
			return err
		}
		if err := db.Reader(ctx).QueryRow(ctx, `SHOW transaction_read_only`).Scan(&readOnly); err != nil {
			return err
		}
		if isolation != "serializable" || readOnly != "on" {
			t.Fatalf("transaction options: %s %s", isolation, readOnly)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
