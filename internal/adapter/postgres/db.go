// Package postgres holds the connection pools and the transaction manager. Repositories
// of the service live next to it (internal/adapter/postgres/<feature>.go) and take their
// connection from DB, so that a usecase decides where a transaction starts and ends.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fin-assist/user/internal/reqctx"
)

// Querier is what a repository needs: implemented by a pool and by a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	SendBatch(ctx context.Context, b *pgx.Batch) pgx.BatchResults
}

// Options configures Open.
type Options struct {
	PrimaryDSN string
	ReplicaDSN string // empty: reads use the primary pool
	MaxConns   int32
	// AppName is application_name of the connections (pg_stat_activity, logs).
	AppName string
}

// DB holds the pool on the primary (-rw) and the pool on the replicas (-ro). On the stand
// both DSNs point to one PostgreSQL; with an empty replica DSN one pool serves both.
type DB struct {
	primary *pgxpool.Pool
	replica *pgxpool.Pool
}

// Open creates the pools and checks that the primary answers.
func Open(ctx context.Context, o Options) (*DB, error) {
	primary, err := newPool(ctx, o.PrimaryDSN, o)
	if err != nil {
		return nil, fmt.Errorf("primary pool: %w", err)
	}
	db := &DB{primary: primary, replica: primary}
	if o.ReplicaDSN != "" && o.ReplicaDSN != o.PrimaryDSN {
		if db.replica, err = newPool(ctx, o.ReplicaDSN, o); err != nil {
			primary.Close()
			return nil, fmt.Errorf("replica pool: %w", err)
		}
	}
	if err := primary.Ping(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("primary: %w", err)
	}
	return db, nil
}

func newPool(ctx context.Context, dsn string, o Options) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// pgx errors here may quote the DSN with its password: never wrap them.
		return nil, errors.New("invalid DSN")
	}
	if o.MaxConns > 0 {
		cfg.MaxConns = o.MaxConns
	}
	if o.AppName != "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = o.AppName
	}
	return pgxpool.NewWithConfig(ctx, cfg)
}

// Close closes the pools.
func (db *DB) Close() {
	if db.replica != db.primary {
		db.replica.Close()
	}
	db.primary.Close()
}

// Ping checks the primary; readiness uses it.
func (db *DB) Ping(ctx context.Context) error { return db.primary.Ping(ctx) }

type txKey struct{}

// Run executes fn in a transaction on the primary: commit when fn returns nil, rollback
// when it returns an error or panics. Repositories and libraries called with the ctx
// that fn receives work inside this transaction (Writer, Reader, Tx).
//
// A Run inside another Run joins the outer transaction: the outer one commits or rolls
// back everything. fn must not call other services or wait on anything slow — the
// transaction holds a connection and its locks for the whole time.
func (db *DB) Run(ctx context.Context, fn func(ctx context.Context) error) error {
	return db.RunWith(ctx, pgx.TxOptions{}, fn)
}

// RunWith is Run with explicit transaction options (isolation level, read only).
// Joining an outer transaction ignores opts.
func (db *DB) RunWith(ctx context.Context, opts pgx.TxOptions, fn func(ctx context.Context) error) (err error) {
	if _, ok := Tx(ctx); ok {
		return fn(ctx)
	}
	tx, err := db.primary.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
		if err != nil {
			// Rollback after a cancelled ctx still has to reach the server.
			if rbErr := tx.Rollback(context.WithoutCancel(ctx)); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
				err = errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
			}
		}
	}()
	if err = fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Tx returns the transaction of ctx. A library that must write in the same transaction
// as the effect (idempotency FIN-78, outbox FIN-109) declares an interface with this
// method and fails when ok is false, instead of silently writing outside it.
func (db *DB) Tx(ctx context.Context) (pgx.Tx, bool) { return Tx(ctx) }

// Tx returns the transaction of ctx, if any.
func Tx(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txKey{}).(pgx.Tx)
	return tx, ok
}

// Writer returns the transaction of ctx or the primary pool.
func (db *DB) Writer(ctx context.Context) Querier {
	if tx, ok := Tx(ctx); ok {
		return tx
	}
	return db.primary
}

// Reader returns where to read: the transaction of ctx, the primary when the request
// asked to read its own write (reqctx.ReadPrimary), otherwise the replicas. Replicas lag:
// a read that decides a write (a check before an update) belongs in Run, not here.
func (db *DB) Reader(ctx context.Context) Querier {
	if tx, ok := Tx(ctx); ok {
		return tx
	}
	if reqctx.ReadPrimary(ctx) {
		return db.primary
	}
	return db.replica
}

// Primary is the primary pool, for libraries that manage their own transactions
// (the outbox dispatcher) and for LISTEN.
func (db *DB) Primary() *pgxpool.Pool { return db.primary }
