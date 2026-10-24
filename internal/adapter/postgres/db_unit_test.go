package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fin-assist/user/internal/reqctx"
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

func TestUnitPoolConfigurationIsLazyAndCancelledOpenFailsWithoutDial(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// MinConns/MinIdleConns stay zero; construction itself does not dial a server.
	pool, err := newPool(ctx, "postgres://localhost:1/postgres?pool_min_conns=0&pool_min_idle_conns=0", Options{MaxConns: 2, AppName: "unit"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg := pool.Config(); cfg.MaxConns != 2 || cfg.ConnConfig.RuntimeParams["application_name"] != "unit" {
		t.Fatal("pool configuration lost", cfg.MaxConns)
	}
	db := &DB{primary: pool, replica: pool}
	db.Close()
	for _, replica := range []string{"", "postgres://localhost:1/replica?pool_min_conns=0&pool_min_idle_conns=0", "not a valid DSN"} {
		if _, err := Open(ctx, Options{PrimaryDSN: "postgres://localhost:1/postgres?pool_min_conns=0&pool_min_idle_conns=0", ReplicaDSN: replica}); err == nil {
			t.Fatal("cancelled open succeeded")
		}
	}
}
