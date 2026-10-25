// Package postgres holds the connection pools and the transaction manager. Repositories
// of the service live next to it (internal/adapter/postgres/<feature>.go) and take their
// connection from DB, so that a usecase decides where a transaction starts and ends.
package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	pgstore "github.com/fin-assist/go-postgres"

	"github.com/fin-assist/go-grpc/reqctx"
)

// Querier is what a repository needs: implemented by a pool and by a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// Options configures the shared pool lifecycle.
type Options = pgstore.PoolOptions

// DB holds the pool on the primary (-rw) and the pool on the replicas (-ro). On the stand
// both DSNs point to one PostgreSQL; with an empty replica DSN one pool serves both.
type DB struct {
	pools        *pgstore.Pools
	primary      *pgxpool.Pool
	replica      *pgxpool.Pool
	transactions *pgstore.Manager
}

// Open creates the pools and checks that the primary answers.
func Open(ctx context.Context, o Options) (*DB, error) {
	pools, err := pgstore.OpenPools(ctx, o)
	if err != nil {
		return nil, err
	}
	return &DB{pools: pools, primary: pools.Primary(), replica: pools.Replica(), transactions: pgstore.New(pools.Primary())}, nil
}

// Close closes the pools owned by the shared lifecycle.
func (db *DB) Close() { db.pools.Close() }

// Ping checks the primary; readiness uses it.
func (db *DB) Ping(ctx context.Context) error { return db.pools.Ping(ctx) }

// Run executes fn in a transaction using go-postgres. Nested calls join the outer
// transaction. Callbacks must propagate errors and must not call remote services.
func (db *DB) Run(ctx context.Context, fn func(context.Context) error) error {
	return db.transactions.Run(ctx, fn)
}

// RunWith is Run with explicit transaction options; an outer transaction's options win.
func (db *DB) RunWith(ctx context.Context, opts pgx.TxOptions, fn func(context.Context) error) error {
	return db.transactions.RunWith(ctx, opts, fn)
}

// RunMapped runs an outer business transaction with an operation mapper defined by
// its adapter. A nested non-nil mapper is rejected; nested work must use Run.
func (db *DB) RunMapped(ctx context.Context, opts pgx.TxOptions, mapper pgstore.ErrorMapper, fn func(context.Context) error) error {
	return db.transactions.RunMapped(ctx, opts, mapper, fn)
}

// Tx supplies this database's transaction to repositories, idempotency and outbox.
func (db *DB) Tx(ctx context.Context) (pgx.Tx, bool) { return db.transactions.Tx(ctx) }

// Tx returns the transaction carried by ctx. The owner alone completes it.
func Tx(ctx context.Context) (pgx.Tx, bool) { return pgstore.Tx(ctx) }

// Writer returns the transaction of ctx or the primary pool.
func (db *DB) Writer(ctx context.Context) Querier {
	if tx, ok := db.Tx(ctx); ok {
		return tx
	}
	if _, ok := Tx(ctx); ok {
		return rejectedQuerier{}
	}
	return db.primary
}

// Reader returns where to read: the transaction of ctx, the primary when the request
// asked to read its own write (reqctx.ReadPrimary), otherwise the replicas. Replicas lag:
// a read that decides a write (a check before an update) belongs in Run, not here.
func (db *DB) Reader(ctx context.Context) Querier {
	if tx, ok := db.Tx(ctx); ok {
		return tx
	}
	if _, ok := Tx(ctx); ok {
		return rejectedQuerier{}
	}
	if reqctx.ReadPrimary(ctx) {
		return db.primary
	}
	return db.replica
}

// Primary is the primary pool, for libraries that manage their own transactions
// (the outbox dispatcher) and for LISTEN.
func (db *DB) Primary() *pgxpool.Pool { return db.primary }
