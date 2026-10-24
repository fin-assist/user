package postgres

import (
	"context"

	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// A context from another pool must never silently fall back to autocommit here.
type rejectedQuerier struct{}

func (rejectedQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, pgstore.ErrForeignTransaction
}
func (rejectedQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, pgstore.ErrForeignTransaction
}
func (rejectedQuerier) QueryRow(context.Context, string, ...any) pgx.Row { return rejectedRow{} }
func (rejectedQuerier) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	return rejectedBatch{}
}

type rejectedRow struct{}

func (rejectedRow) Scan(...any) error { return pgstore.ErrForeignTransaction }

type rejectedBatch struct{}

func (rejectedBatch) Exec() (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, pgstore.ErrForeignTransaction
}
func (rejectedBatch) Query() (pgx.Rows, error) { return nil, pgstore.ErrForeignTransaction }
func (rejectedBatch) QueryRow() pgx.Row        { return rejectedRow{} }
func (rejectedBatch) Close() error             { return pgstore.ErrForeignTransaction }
