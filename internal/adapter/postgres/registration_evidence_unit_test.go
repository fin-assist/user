package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestUnitEvidenceMarkerHasOneOwnerAndRequiresPrimaryTransaction(t *testing.T) {
	db := &DB{}
	repo := NewVerificationStarts(db)
	now := time.Now().UTC()
	record := startRecord(startKeyA, "+79001234567", now)
	tx := &unitTx{}
	for _, f := range []func(context.Context) error{
		func(c context.Context) error { _, _, e := repo.Busy(c, record.Request.Phone, now); return e },
		func(c context.Context) error { _, _, e := repo.Dispatch(c, record, now); return e },
		func(c context.Context) error { return repo.Terminate(c, record, now) },
		func(c context.Context) error { return repo.FinalizeKey(c, record) },
	} {
		if f(t.Context()) == nil {
			t.Fatal("mutation outside primary transaction")
		}
	}
	withUnitTransaction(t, db, tx, func(ctx context.Context) {
		tx.row = func(string, []any) pgx.Row { return unitRow{values: []any{now, record.Request.UnknownDeadline}} }
		got, first, e := repo.Dispatch(ctx, record, now)
		if e != nil || !first || !got.Request.FirstDispatchAt.Equal(now) || !got.Request.DispatchNotAfter.Equal(record.Request.UnknownDeadline) {
			t.Fatal("initial dispatch identity", first, e)
		}
		calls := 0
		tx.row = func(string, []any) pgx.Row {
			calls++
			if calls == 1 {
				return unitRow{err: pgx.ErrNoRows}
			}
			return unitRow{values: []any{now, record.Request.UnknownDeadline}}
		}
		got, first, e = repo.Dispatch(ctx, record, now.Add(time.Second))
		if e != nil || first || !got.Request.FirstDispatchAt.Equal(now) {
			t.Fatal("existing marker granted new dispatch", first, e)
		}
		tx.row = func(string, []any) pgx.Row { return unitRow{err: errUnitStorage} }
		if _, _, e := repo.Dispatch(ctx, record, now); e == nil {
			t.Fatal("storage error granted dispatch")
		}
		if e := repo.Terminate(ctx, record, now); e != nil {
			t.Fatal(e)
		}
		if e := repo.FinalizeKey(ctx, record); e != nil {
			t.Fatal(e)
		}
		tx.exec = func(string, []any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, errUnitStorage }
		for _, f := range []func() error{func() error { return repo.Terminate(ctx, record, now) }, func() error { return repo.FinalizeKey(ctx, record) }} {
			if !errors.Is(f(), errUnitStorage) {
				t.Fatal("storage failure hidden")
			}
		}
	})
}

func TestUnitKnownBusyLookupKeepsFiniteDeadlineWithoutProviderOwnership(t *testing.T) {
	db := &DB{}
	repo := NewVerificationStarts(db)
	now := time.Now().UTC()
	tx := &unitTx{}
	withUnitTransaction(t, db, tx, func(ctx context.Context) {
		for _, fromFact := range []bool{false, true} {
			calls := 0
			tx.row = func(string, []any) pgx.Row {
				calls++
				if fromFact && calls == 1 {
					return unitRow{err: pgx.ErrNoRows}
				}
				return unitRow{values: []any{int32(7), now.Add(time.Minute)}}
			}
			id, until, e := repo.Busy(ctx, "+79001234567", now)
			if e != nil || id != 7 || !until.Equal(now.Add(time.Minute)) {
				t.Fatal("busy deadline changed", id, until, e)
			}
		}
		tx.row = func(string, []any) pgx.Row { return unitRow{err: pgx.ErrNoRows} }
		if _, until, e := repo.Busy(ctx, "+79001234567", now); e != nil || !until.IsZero() {
			t.Fatal("missing fact manufactured fence", until, e)
		}
		tx.row = func(string, []any) pgx.Row { return unitRow{err: errUnitStorage} }
		if _, _, e := repo.Busy(ctx, "+79001234567", now); !errors.Is(e, errUnitStorage) {
			t.Fatal("read failure hidden", e)
		}
		tx.exec = func(string, []any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, errUnitStorage }
		if _, _, e := repo.Busy(ctx, "+79001234567", now); !errors.Is(e, errUnitStorage) {
			t.Fatal("write failure hidden", e)
		}
	})
}
