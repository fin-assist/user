package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fin-assist/user/internal/ratelimit"
	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/verification"
)

func unitVerificationValues(now time.Time) []any {
	h := token.Sum("secret")
	return []any{int32(7), "+79001234567", h[:], "test", "call", now, now.Add(time.Minute), nil, false, "", nil, []byte("cipher"), "key", false, false}
}

func TestUnitVerificationRepositoryMapsRowsAndEnforcesTransactionGuard(t *testing.T) {
	db := &DB{}
	r := NewVerifications(db)
	tx := &unitTx{}
	withUnitTransaction(t, db, tx, func(ctx context.Context) {
		now := time.Now().UTC()
		values := unitVerificationValues(now)
		tx.row = func(q string, args []any) pgx.Row { return unitRow{values: values} }
		if v, err := r.ByToken(ctx, token.Sum("secret")); err != nil || v.ID != 7 || v.Hash != token.Sum("secret") {
			t.Fatalf("token lookup: %+v %v", v, err)
		}
		if v, err := r.ByCall(ctx, "test", "call"); err != nil || v.CallID != "call" {
			t.Fatalf("call lookup: %+v %v", v, err)
		}
		for _, write := range []bool{false, true} {
			tx.row = func(q string, args []any) pgx.Row {
				if !strings.Contains(q, "FOR SHARE") && !strings.Contains(q, "FOR UPDATE") || args[0] != int32(7) {
					t.Fatalf("lock scope: %s %v", q, args)
				}
				return unitRow{values: values}
			}
			if v, err := r.Lock(ctx, 7, write); err != nil || v.ID != 7 {
				t.Fatal(v, err)
			}
		}
		registration := token.Sum("registration")
		values[10] = registration[:]
		tx.row = func(string, []any) pgx.Row { return unitRow{values: values} }
		if v, err := r.ByToken(ctx, token.Hash{}); err != nil || v.RegistrationHash == nil || *v.RegistrationHash != registration {
			t.Fatalf("registration hash: %+v %v", v, err)
		}
		for _, index := range []int{2, 10} {
			saved := values[index]
			values[index] = []byte("corrupt")
			if _, err := r.ByToken(ctx, token.Hash{}); err == nil {
				t.Fatal("corrupt hash accepted")
			}
			values[index] = saved
		}
		for _, cause := range []error{pgx.ErrNoRows, errUnitStorage} {
			tx.row = func(string, []any) pgx.Row { return unitRow{err: cause} }
			if _, err := r.ByCall(ctx, "test", "call"); err == nil {
				t.Fatal("lookup failure hidden")
			}
		}
		for _, tag := range []string{"UPDATE 0", "UPDATE 1"} {
			tx.exec = func(q string, args []any) (pgconn.CommandTag, error) { return pgconn.NewCommandTag(tag), nil }
			err := r.Verify(ctx, verification.Request{ID: 7}, now, verification.Outcome{UserID: "owner"}, &registration, []byte("encrypted"), "key")
			if (err == nil) != (tag == "UPDATE 1") {
				t.Fatalf("confirm missing row: %s %v", tag, err)
			}
			err = r.SavePool(ctx, 7, []string{"+74951234567"}, "+74951234567")
			if (err == nil) != (tag == "UPDATE 1") {
				t.Fatalf("pool missing row: %s %v", tag, err)
			}
		}
		for _, failAt := range []int{1, 2} {
			calls := 0
			tx.exec = func(string, []any) (pgconn.CommandTag, error) {
				calls++
				if calls == failAt {
					return pgconn.CommandTag{}, errUnitStorage
				}
				return pgconn.NewCommandTag("UPDATE 1"), nil
			}
			if err := r.Verify(ctx, verification.Request{ID: 7}, now, verification.Outcome{}, nil, nil, ""); err == nil {
				t.Fatal("confirmation error hidden")
			}
		}
		for _, call := range []func(context.Context) error{func(c context.Context) error { _, e := r.Lock(c, 7, true); return e }, func(c context.Context) error {
			return r.Verify(c, verification.Request{}, now, verification.Outcome{}, nil, nil, "")
		}, func(c context.Context) error { return r.SavePool(c, 7, nil, "") }, func(c context.Context) error { _, e := r.CleanupResults(c, now, 1); return e }, func(c context.Context) error { _, e := r.DetachOldRequests(c, now, 1); return e }, func(c context.Context) error { _, e := r.DeleteOldRequests(c, now, 1); return e }} {
			if err := call(t.Context()); err == nil {
				t.Fatal("write outside transaction")
			}
		}
		for _, clean := range []func(context.Context, time.Time, int) (int64, error){r.CleanupResults, r.DetachOldRequests, r.DeleteOldRequests} {
			for _, batch := range []int{0, 1001} {
				if _, err := clean(ctx, now, batch); err == nil {
					t.Fatal("invalid batch accepted")
				}
			}
			tx.exec = func(string, []any) (pgconn.CommandTag, error) { return pgconn.NewCommandTag("UPDATE 2"), nil }
			if n, err := clean(ctx, now, 1); err != nil || n < 2 {
				t.Fatalf("cleanup: %d %v", n, err)
			}
			tx.exec = func(string, []any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, errUnitStorage }
			if _, err := clean(ctx, now, 1); err == nil {
				t.Fatal("cleanup error hidden")
			}
		}
		tx.exec = func(string, []any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, errUnitStorage }
		if err := r.SavePool(ctx, 7, nil, ""); err == nil {
			t.Fatal("pool storage error hidden")
		}
	})
}

func TestUnitCallbackOutcomesKeepsAggregateRowsSeparateAndClosesCursor(t *testing.T) {
	db := &DB{}
	r := NewVerifications(db)
	tx := &unitTx{}
	withUnitTransaction(t, db, tx, func(ctx context.Context) {
		number := "+74951234567"
		rows := &unitRows{data: [][]any{{nil, "confirmed", int64(3)}, {&number, "confirmed", int64(2)}}}
		tx.query = func(q string, args []any) (pgx.Rows, error) {
			if len(args) != 2 || args[1] != maxCallbackOutcomeDetails {
				t.Fatalf("unbounded detail query: %v", args)
			}
			return rows, nil
		}
		details, total, err := r.PoolOutcomes(ctx, time.Now())
		if err != nil || len(details) != 1 || details[0].Number != number || details[0].Count != 2 || total["confirmed"] != 3 || !rows.closed {
			t.Fatalf("outcomes: %+v %+v %v closed=%v", details, total, err, rows.closed)
		}
		for _, bad := range []*unitRows{{data: [][]any{{"wrong arity"}}}, {err: errUnitStorage}} {
			tx.query = func(string, []any) (pgx.Rows, error) { return bad, nil }
			if _, _, err := r.PoolOutcomes(ctx, time.Now()); err == nil || !bad.closed {
				t.Fatal("cursor error hidden or not closed", err)
			}
		}
		tx.query = func(string, []any) (pgx.Rows, error) { return nil, errUnitStorage }
		if _, _, err := r.PoolOutcomes(ctx, time.Now()); err == nil {
			t.Fatal("query error hidden")
		}
	})
}

func TestUnitStartRepositoryPreservesOwnershipAndTranslatesBusyRaces(t *testing.T) {
	db := &DB{}
	r := NewVerificationStarts(db)
	tx := &unitTx{}
	withUnitTransaction(t, db, tx, func(ctx context.Context) {
		now := time.Now().UTC()
		h := token.Sum("request")
		record := verification.StartRecord{Key: "request-key", Hash: h, Request: verification.Request{ID: 7, Phone: "+79001234567", CreatedAt: now, ExpiresAt: now.Add(time.Minute)}}
		tx.row = func(q string, args []any) pgx.Row {
			if strings.Contains(q, "SELECT start_key") {
				return unitRow{values: []any{record.Key, h[:], int32(7), []byte("[]")}}
			}
			return unitRow{values: unitVerificationValues(now)}
		}
		if got, err := r.Find(ctx, record.Key); err != nil || got.Hash != h || got.Request.ID != 7 {
			t.Fatalf("attempt mapping: %+v %v", got, err)
		}
		for _, cause := range []error{pgx.ErrNoRows, errUnitStorage} {
			tx.row = func(string, []any) pgx.Row { return unitRow{err: cause} }
			if _, err := r.Find(ctx, record.Key); err == nil {
				t.Fatal("attempt read error hidden")
			}
		}
		tx.row = func(string, []any) pgx.Row {
			return unitRow{values: []any{record.Key, []byte("bad"), int32(7), []byte("[]")}}
		}
		if _, err := r.Find(ctx, record.Key); err == nil {
			t.Fatal("corrupt attempt accepted")
		}
		tx.row = func(q string, args []any) pgx.Row { return unitRow{values: []any{int32(8)}} }
		if got, busy, err := r.Claim(ctx, record); err != nil || got.Request.ID != 8 || !busy.IsZero() {
			t.Fatalf("claim: %+v %v %v", got, busy, err)
		}
		tx.row = func(q string, args []any) pgx.Row {
			if strings.Contains(q, "INSERT INTO") {
				return unitRow{err: pgx.ErrNoRows}
			}
			return unitRow{values: []any{int32(9), now.Add(time.Minute)}}
		}
		if got, busy, err := r.Claim(ctx, record); err != nil || got.Request.ID != 9 || got.Windows != nil || busy.IsZero() {
			t.Fatalf("busy claim: %+v %v %v", got, busy, err)
		}
		for _, cause := range []error{pgx.ErrNoRows, errUnitStorage} {
			tx.row = func(string, []any) pgx.Row { return unitRow{err: cause} }
			if _, _, err := r.Claim(ctx, record); err == nil {
				t.Fatal("race exhaustion or storage error hidden")
			}
		}
		for _, tag := range []string{"UPDATE 0", "UPDATE 1"} {
			tx.exec = func(string, []any) (pgconn.CommandTag, error) { return pgconn.NewCommandTag(tag), nil }
			for _, call := range []func(context.Context) error{func(c context.Context) error { return r.Complete(c, record, verification.Call{StartedAt: now}, h) }, func(c context.Context) error { return r.Reject(c, record, now) }, func(c context.Context) error { return r.Duplicate(c, record, now) }} {
				err := call(ctx)
				if (err == nil) != (tag == "UPDATE 1") {
					t.Fatalf("ownership result: %s %v", tag, err)
				}
				if err := call(t.Context()); err == nil {
					t.Fatal("write outside transaction")
				}
			}
		}
		if err := r.Forget(ctx, record.Key); err != nil {
			t.Fatal(err)
		}
		if err := r.Forget(t.Context(), record.Key); err == nil {
			t.Fatal("forget outside transaction")
		}
		if _, _, err := r.Claim(t.Context(), record); err == nil {
			t.Fatal("claim outside transaction")
		}
		tx.exec = func(string, []any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, errUnitStorage }
		for _, call := range []func() error{func() error { return r.Forget(ctx, record.Key) }, func() error { return r.Complete(ctx, record, verification.Call{}, h) }, func() error { return r.Reject(ctx, record, now) }, func() error { return r.Duplicate(ctx, record, now) }, func() error { _, _, e := r.Claim(ctx, record); return e }} {
			if err := call(); err == nil {
				t.Fatal("write failure hidden")
			}
		}
		bad := record
		bad.Windows = []ratelimit.Window{{Start: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)}}
		if _, _, err := r.Claim(ctx, bad); err == nil {
			t.Fatal("invalid stored window accepted")
		}
	})
}

func TestUnitZvonokGateUsesTransactionAndStopsOnStorageError(t *testing.T) {
	db := &DB{}
	tx := &unitTx{row: func(string, []any) pgx.Row { return unitRow{values: []any{0}} }}
	withUnitTransaction(t, db, tx, func(ctx context.Context) {
		g := NewZvonokGate(db, "test-key")
		if err := g.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		tx.row = func(string, []any) pgx.Row { return unitRow{err: errUnitStorage} }
		if err := g.Wait(ctx); err == nil {
			t.Fatal("lock error hidden")
		}
		tx.row = func(string, []any) pgx.Row { return unitRow{values: []any{0}} }
		tx.exec = func(string, []any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, errUnitStorage }
		if err := g.Wait(ctx); err == nil {
			t.Fatal("increment error hidden")
		}
		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()
		tx.row = func(string, []any) pgx.Row { return unitRow{values: []any{zvonokRequestsPerSecond}} }
		if err := g.Wait(cancelCtx); !errors.Is(err, context.Canceled) {
			t.Fatalf("full-window cancellation: %v", err)
		}
	})
}
