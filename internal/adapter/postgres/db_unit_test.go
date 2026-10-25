package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fin-assist/go-grpc/reqctx"
)

func TestUnitDatabaseSelectorsAndNestedTransactionsRetainOwner(t *testing.T) {
	primary, replica := &pgxpool.Pool{}, &pgxpool.Pool{}
	db := &DB{primary: primary, replica: replica}
	ctx := t.Context()
	if db.Writer(ctx) != primary || db.Reader(ctx) != replica || db.Reader(reqctx.WithReadPrimary(ctx)) != primary || db.Primary() != primary {
		t.Fatal("wrong pool selection")
	}
	tx := &unitTx{}
	withUnitTransaction(t, db, tx, func(ctx context.Context) {
		if db.Writer(ctx) != tx || db.Reader(ctx) != tx {
			t.Fatal("transaction replaced by pool")
		}
		if got, ok := db.Tx(ctx); !ok || got != tx {
			t.Fatal("transaction owner lost")
		}
		cause := errors.New("domain failure")
		called := false
		if err := db.RunWith(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(got context.Context) error {
			called = true
			if got != ctx {
				t.Fatal("nested transaction changed context")
			}
			return cause
		}); !called || !errors.Is(err, cause) {
			t.Fatal("nested transaction result", called, err)
		}
	})
}

func TestUnitCancelledPoolOpenFailsWithoutDial(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Open(ctx, Options{PrimaryDSN: "postgres://localhost:1/postgres?pool_min_conns=0&pool_min_idle_conns=0"}); err == nil {
		t.Fatal("cancelled open succeeded")
	}
}
