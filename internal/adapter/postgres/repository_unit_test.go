package postgres

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/fin-assist/user/internal/auth"
	"github.com/fin-assist/user/internal/deletion"
	"github.com/fin-assist/user/internal/profile"
	"github.com/fin-assist/user/internal/ratelimit"
	"github.com/fin-assist/user/internal/token"
)

// These fakes verify Go adapter contracts; the integration suite still verifies SQL on PostgreSQL.
type unitRow struct {
	values []any
	err    error
}

func (r unitRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != len(r.values) {
		return errors.New("unexpected scan arity")
	}
	for i, d := range dest {
		target := reflect.ValueOf(d).Elem()
		if r.values[i] == nil {
			target.SetZero()
			continue
		}
		v := reflect.ValueOf(r.values[i])
		if !v.Type().AssignableTo(target.Type()) {
			return errors.New("unexpected scan type")
		}
		target.Set(v)
	}
	return nil
}

type unitRows struct {
	pgx.Rows
	data   [][]any
	pos    int
	err    error
	closed bool
}

func (r *unitRows) Next() bool {
	if r.pos >= len(r.data) {
		return false
	}
	r.pos++
	return true
}
func (r *unitRows) Scan(dest ...any) error { return (unitRow{values: r.data[r.pos-1]}).Scan(dest...) }
func (r *unitRows) Err() error             { return r.err }
func (r *unitRows) Close()                 { r.closed = true }

type unitTx struct {
	pgx.Tx
	row   func(string, []any) pgx.Row
	exec  func(string, []any) (pgconn.CommandTag, error)
	query func(string, []any) (pgx.Rows, error)
}

func (f *unitTx) QueryRow(_ context.Context, q string, args ...any) pgx.Row {
	if f.row == nil {
		return unitRow{err: errors.New("storage unavailable")}
	}
	return f.row(q, args)
}
func (f *unitTx) Exec(_ context.Context, q string, args ...any) (pgconn.CommandTag, error) {
	if f.exec == nil {
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}
	return f.exec(q, args)
}
func (f *unitTx) Query(_ context.Context, q string, args ...any) (pgx.Rows, error) {
	if f.query == nil {
		return &unitRows{}, nil
	}
	return f.query(q, args)
}
func unitContext(tx *unitTx) context.Context {
	return context.WithValue(context.Background(), txKey{}, pgx.Tx(tx))
}

var errUnitStorage = errors.New("storage unavailable")

func TestUnitAuthenticationRepositoryMapsFailuresAndLocksIdentity(t *testing.T) {
	db := &DB{}
	r := NewAuth(db)
	now := time.Now().UTC()
	verificationID := int32(7)
	tx := &unitTx{}
	ctx := unitContext(tx)
	tx.row = func(q string, args []any) pgx.Row {
		if strings.Contains(q, "SELECT r.session_id") {
			return unitRow{values: []any{int32(3), int32(1), "00000000-0000-0000-0000-000000000001", &verificationID, now, now.Add(time.Hour), nil, nil}}
		}
		return unitRow{values: []any{int32(3)}}
	}
	for _, lock := range []bool{false, true} {
		var link auth.Link
		var err error
		if lock {
			link, err = r.LockLink(ctx, token.Sum("refresh"), now)
		} else {
			link, err = r.FindLink(ctx, token.Sum("refresh"), now)
		}
		if err != nil || link.UserID != "00000000-0000-0000-0000-000000000001" || link.SessionID != 3 {
			t.Fatalf("link: %+v %v", link, err)
		}
	}
	for _, cause := range []error{pgx.ErrNoRows, errUnitStorage, &pgconn.PgError{ConstraintName: "auth_session_user_fkey"}} {
		tx.row = func(string, []any) pgx.Row { return unitRow{err: cause} }
		if _, err := r.FindLink(ctx, token.Sum("refresh"), now); err == nil {
			t.Fatal("read error hidden")
		}
		if _, err := r.LockLink(ctx, token.Sum("refresh"), now); err == nil {
			t.Fatal("lock error hidden")
		}
	}
	tx.row = func(q string, args []any) pgx.Row {
		if strings.Contains(q, "SELECT EXISTS") {
			return unitRow{values: []any{true}}
		}
		if strings.Contains(q, "SELECT closed_at") {
			return unitRow{values: []any{true}}
		}
		return unitRow{values: []any{"00000000-0000-0000-0000-000000000001"}}
	}
	if err := r.LockUser(ctx, "00000000-0000-0000-0000-000000000001"); err != nil {
		t.Fatal(err)
	}
	if err := r.GrantPD(ctx, "00000000-0000-0000-0000-000000000001", "v1", now); err != nil {
		t.Fatal(err)
	}
	if err := r.CloseVerification(ctx, 7, "00000000-0000-0000-0000-000000000001", now); err != nil {
		t.Fatal(err)
	}
	for _, tag := range []string{"UPDATE 0", "UPDATE 1"} {
		tx.exec = func(q string, args []any) (pgconn.CommandTag, error) {
			if len(args) == 0 {
				t.Fatal("missing bound arguments")
			}
			return pgconn.NewCommandTag(tag), nil
		}
		err := r.Rotate(ctx, token.Sum("old"), token.Sum("new"), auth.Link{SessionID: 3, Generation: 1}, now, now.Add(time.Hour))
		if (err == nil) != (tag == "UPDATE 1") {
			t.Fatalf("rotation tag %s: %v", tag, err)
		}
		if err := r.CloseVerification(ctx, 7, "00000000-0000-0000-0000-000000000001", now); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Revoke(ctx, auth.Link{SessionID: 3, UserID: "00000000-0000-0000-0000-000000000001"}, now); err != nil {
		t.Fatal(err)
	}
	if err := r.Logout(ctx, token.Sum("old")); err != nil {
		t.Fatal(err)
	}
	tx.exec = func(string, []any) (pgconn.CommandTag, error) { return pgconn.NewCommandTag("DELETE 2"), nil }
	if n, err := r.CleanupSessions(ctx, now, 10); err != nil || n != 4 {
		t.Fatalf("cleanup: %d %v", n, err)
	}
	tx.exec = func(string, []any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, errUnitStorage }
	for _, call := range []func() error{func() error { return r.Revoke(ctx, auth.Link{}, now) }, func() error { return r.Logout(ctx, token.Hash{}) }, func() error { return r.Rotate(ctx, token.Hash{}, token.Hash{}, auth.Link{}, now, now) }, func() error { return r.GrantPD(ctx, "00000000-0000-0000-0000-000000000001", "v1", now) }, func() error { return r.CloseVerification(ctx, 7, "00000000-0000-0000-0000-000000000001", now) }, func() error { _, e := r.CleanupSessions(ctx, now, 10); return e }} {
		if err := call(); err == nil {
			t.Fatal("write failure hidden")
		}
	}
	for _, call := range []func() error{func() error { _, e := r.LockLink(t.Context(), token.Hash{}, now); return e }, func() error { return r.Revoke(t.Context(), auth.Link{}, now) }, func() error { return r.Logout(t.Context(), token.Hash{}) }, func() error { return r.Rotate(t.Context(), token.Hash{}, token.Hash{}, auth.Link{}, now, now) }, func() error { return r.LockUser(t.Context(), "u") }, func() error { return r.GrantPD(t.Context(), "u", "v1", now) }, func() error { return r.CloseVerification(t.Context(), 7, "u", now) }, func() error { _, e := r.CleanupSessions(t.Context(), now, 10); return e }, func() error { return r.CreateSession(t.Context(), "u", 7, token.Hash{}, now, now) }} {
		if err := call(); err == nil {
			t.Fatal("write accepted outside transaction")
		}
	}
	for _, constraint := range []string{"user_consent_document_fkey", "auth_session_user_fkey", "auth_session_phone_verification_fkey", "phone_verification_user_fkey", "user_consent_user_fkey", "unknown"} {
		if authError(&pgconn.PgError{ConstraintName: constraint}) == nil {
			t.Fatal("constraint error hidden")
		}
	}
}

func TestUnitRegistrationRepositoryConsumesOnlyBoundOwnerAndTranslatesConstraints(t *testing.T) {
	r := NewAuth(&DB{})
	tx := &unitTx{}
	ctx := unitContext(tx)
	now := time.Now().UTC()
	tx.row = func(q string, args []any) pgx.Row {
		switch {
		case strings.Contains(q, "registration_token_hash"):
			return unitRow{values: []any{int32(7), "+79001234567", &now, true, false}}
		case strings.Contains(q, "consent_document"):
			return unitRow{values: []any{"v1"}}
		case strings.Contains(q, "auth_session"):
			return unitRow{values: []any{int32(8)}}
		default:
			return unitRow{values: []any{"00000000-0000-0000-0000-000000000001"}}
		}
	}
	for _, lock := range []bool{false, true} {
		v, err := r.Registration(ctx, token.Sum("registration"), lock)
		if err != nil || v.ID != 7 || v.Phone != "+79001234567" {
			t.Fatalf("registration: %+v %v", v, err)
		}
	}
	if _, err := r.Registration(t.Context(), token.Hash{}, true); err == nil {
		t.Fatal("registration lock without transaction")
	}
	if v, err := r.CurrentPD(ctx); err != nil || v != "v1" {
		t.Fatalf("document: %s %v", v, err)
	}
	if id, created, err := r.CreateUser(ctx, "+79001234567", "Europe/Moscow", now); err != nil || id != "00000000-0000-0000-0000-000000000001" || !created {
		t.Fatalf("user: %s %v %v", id, created, err)
	}
	if err := r.CreateSession(ctx, "00000000-0000-0000-0000-000000000001", 7, token.Hash{}, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, cause := range []error{pgx.ErrNoRows, errUnitStorage} {
		tx.row = func(string, []any) pgx.Row { return unitRow{err: cause} }
		if _, err := r.Registration(ctx, token.Hash{}, false); err == nil {
			t.Fatal("registration error hidden")
		}
		v, err := r.CurrentPD(ctx)
		if errors.Is(cause, pgx.ErrNoRows) {
			if err != nil || v != "" {
				t.Fatal(v, err)
			}
		} else if err == nil {
			t.Fatal("document error hidden")
		}
		if _, _, err := r.CreateUser(ctx, "phone", "UTC", now); err == nil {
			t.Fatal("create/read error hidden")
		}
		if err := r.CreateSession(ctx, "00000000-0000-0000-0000-000000000001", 7, token.Hash{}, now, now); err == nil {
			t.Fatal("session error hidden")
		}
		id, err := r.FindUserByPhone(ctx, "phone")
		if errors.Is(cause, pgx.ErrNoRows) {
			if err != nil || id != "" {
				t.Fatal(id, err)
			}
		} else if err == nil {
			t.Fatal("lookup error hidden")
		}
	}
	calls := 0
	tx.row = func(q string, args []any) pgx.Row {
		calls++
		if calls == 1 {
			return unitRow{err: pgx.ErrNoRows}
		}
		if len(args) != 1 || args[0] != "phone" {
			t.Fatalf("unscoped phone lookup: %v", args)
		}
		return unitRow{values: []any{"00000000-0000-0000-0000-000000000002"}}
	}
	if id, created, err := r.CreateUser(ctx, "phone", "UTC", now); err != nil || id != "00000000-0000-0000-0000-000000000002" || created {
		t.Fatalf("existing user: %s %v %v", id, created, err)
	}
	if _, _, err := r.CreateUser(t.Context(), "phone", "UTC", now); err == nil {
		t.Fatal("create outside transaction")
	}
}

func TestUnitProfileAndConsentRepositoriesPreserveNullableFieldsAndRefuseMissingRows(t *testing.T) {
	db := &DB{}
	r := NewProfiles(db)
	documents := NewConsentDocuments(db)
	tx := &unitTx{}
	ctx := unitContext(tx)
	now := time.Now().UTC()
	version := "v2"
	tx.row = func(q string, args []any) pgx.Row {
		if strings.Contains(q, "SELECT u.id") {
			if len(args) != 1 || args[0] != "00000000-0000-0000-0000-000000000001" {
				t.Fatalf("unscoped profile read: %v", args)
			}
			return unitRow{values: []any{"00000000-0000-0000-0000-000000000001", "+79001234567", "dark", "UTC", int32(2), now, &version, &now, "v2"}}
		}
		if strings.Contains(q, "SELECT version,title,url") {
			return unitRow{values: []any{"v2", "Consent", "https://example.test/doc"}}
		}
		return unitRow{values: []any{"00000000-0000-0000-0000-000000000001"}}
	}
	for _, read := range []func(context.Context, string) (profile.Record, error){r.Read, r.Current} {
		got, err := read(ctx, "00000000-0000-0000-0000-000000000001")
		if err != nil || got.ID != "00000000-0000-0000-0000-000000000001" || got.Consent.Version == nil || *got.Consent.Version != "v2" {
			t.Fatalf("profile mapping: %+v %v", got, err)
		}
	}
	if err := r.Lock(ctx, "00000000-0000-0000-0000-000000000001"); err != nil {
		t.Fatal(err)
	}
	if doc, err := documents.Current(ctx, "assistant"); err != nil || doc.Type != "assistant" || doc.Version != "v2" || doc.URL != "https://example.test/doc" {
		t.Fatalf("consent mapping: %+v %v", doc, err)
	}
	for _, call := range []func(context.Context) error{func(c context.Context) error { return r.Lock(c, "00000000-0000-0000-0000-000000000001") }, func(c context.Context) error {
		return r.Update(c, "00000000-0000-0000-0000-000000000001", "dark", "UTC", 2, now)
	}, func(c context.Context) error { return r.Grant(c, "00000000-0000-0000-0000-000000000001", "v2", now) }, func(c context.Context) error { return r.Revoke(c, "00000000-0000-0000-0000-000000000001", now) }} {
		if err := call(ctx); err != nil {
			t.Fatal(err)
		}
		if err := call(t.Context()); err == nil {
			t.Fatal("write outside transaction")
		}
	}
	tx.exec = func(string, []any) (pgconn.CommandTag, error) { return pgconn.NewCommandTag("UPDATE 0"), nil }
	if err := r.Update(ctx, "00000000-0000-0000-0000-000000000001", "dark", "UTC", 2, now); !errors.Is(err, finerr.NotFound()) {
		if finerrCode(err) != "NOT_FOUND" {
			t.Fatalf("missing profile: %v", err)
		}
	}
	for _, constraint := range []string{"user_consent_user_fkey", "user_consent_document_fkey", "user_consent_active_key", "unknown"} {
		tx.exec = func(string, []any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, &pgconn.PgError{ConstraintName: constraint}
		}
		if err := r.Grant(ctx, "00000000-0000-0000-0000-000000000001", "v2", now); err == nil {
			t.Fatal("constraint hidden")
		}
	}
	for _, cause := range []error{pgx.ErrNoRows, errUnitStorage} {
		tx.row = func(string, []any) pgx.Row { return unitRow{err: cause} }
		if _, err := documents.Current(ctx, "assistant"); err == nil {
			t.Fatal("document error hidden")
		}
		if _, err := r.Current(ctx, "00000000-0000-0000-0000-000000000001"); err == nil {
			t.Fatal("read error hidden")
		}
		if err := r.Lock(ctx, "00000000-0000-0000-0000-000000000001"); err == nil {
			t.Fatal("lock error hidden")
		}
	}
	tx.exec = func(string, []any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, errUnitStorage }
	if err := r.Update(ctx, "00000000-0000-0000-0000-000000000001", "dark", "UTC", 2, now); err == nil {
		t.Fatal("update error hidden")
	}
	if err := r.Revoke(ctx, "00000000-0000-0000-0000-000000000001", now); err == nil {
		t.Fatal("revoke error hidden")
	}
}

func finerrCode(err error) string {
	r, ok := finerr.ToREST(err)
	if !ok {
		return ""
	}
	return r.Code
}

func TestUnitLimitAndDeletionRepositoriesRequireTransactionsAndPropagateStorageErrors(t *testing.T) {
	db := &DB{}
	limits := NewLimits(db)
	accounts := NewAccounts(db)
	tx := &unitTx{}
	ctx := unitContext(tx)
	now := time.Now().UTC()
	windows := []ratelimit.Window{{Scope: ratelimit.WatchIP, Subject: "192.0.2.1", Start: now, End: now.Add(time.Minute), Limit: 3}}
	tx.row = func(q string, args []any) pgx.Row {
		if strings.Contains(q, "SELECT phone") {
			return unitRow{values: []any{"+79001234567"}}
		}
		return unitRow{values: []any{2}}
	}
	if counts, err := limits.Lock(ctx, windows); err != nil || !reflect.DeepEqual(counts, []int{2}) {
		t.Fatalf("counts: %v %v", counts, err)
	}
	if err := limits.Increment(ctx, windows); err != nil {
		t.Fatal(err)
	}
	if err := limits.Refund(ctx, windows); err != nil {
		t.Fatal(err)
	}
	if _, err := limits.Lock(ctx, nil); err == nil {
		t.Fatal("empty windows accepted")
	}
	for _, batch := range []int{0, 10001} {
		if _, err := limits.Cleanup(ctx, now, batch); err == nil {
			t.Fatal("invalid batch accepted")
		}
	}
	if _, err := limits.Cleanup(ctx, now, 1); err != nil {
		t.Fatal(err)
	}
	if err := accounts.Remove(ctx, "00000000-0000-0000-0000-000000000001"); err != nil {
		t.Fatal(err)
	}
	tx.exec = func(string, []any) (pgconn.CommandTag, error) { return pgconn.NewCommandTag("UPDATE 0"), nil }
	if err := limits.Increment(ctx, windows); err == nil {
		t.Fatal("missing locked window hidden")
	}
	for _, call := range []func(context.Context) error{func(c context.Context) error { _, e := limits.Lock(c, windows); return e }, func(c context.Context) error { return limits.Increment(c, windows) }, func(c context.Context) error { return limits.Refund(c, windows) }, func(c context.Context) error { return accounts.Remove(c, "00000000-0000-0000-0000-000000000001") }} {
		if err := call(t.Context()); err == nil {
			t.Fatal("write outside transaction")
		}
	}
	tx.row = func(string, []any) pgx.Row { return unitRow{err: errUnitStorage} }
	if _, err := limits.Lock(ctx, windows); err == nil {
		t.Fatal("window read error hidden")
	}
	tx.exec = func(string, []any) (pgconn.CommandTag, error) { return pgconn.CommandTag{}, errUnitStorage }
	for _, call := range []func() error{func() error { return limits.Increment(ctx, windows) }, func() error { return limits.Refund(ctx, windows) }, func() error { _, e := limits.Cleanup(ctx, now, 1); return e }, func() error { return accounts.Remove(ctx, "00000000-0000-0000-0000-000000000001") }} {
		if err := call(); err == nil {
			t.Fatal("storage error hidden")
		}
	}
	tx.query = func(string, []any) (pgx.Rows, error) { return nil, &pgconn.PgError{Code: "55P03"} }
	if err := accounts.Remove(ctx, "00000000-0000-0000-0000-000000000001"); !errors.Is(err, deletion.ErrReplayBusy) {
		t.Fatalf("busy replay: %v", err)
	}
}

func TestUnitDeletionRepositoryStopsAtEachStorageFailure(t *testing.T) {
	r := NewAccounts(&DB{})
	// Failure positions correspond to the lock cursor, account read, parent locks,
	// linked-key locks and each individual deletion. No later query may hide an error.
	for _, failure := range []struct {
		kind string
		at   int
	}{{"query", 1}, {"query", 2}, {"query", 3}, {"rows", 1}, {"rows", 2}, {"rows", 3}, {"row", 1}, {"exec", 1}, {"exec", 2}, {"exec", 3}, {"exec", 4}, {"exec", 5}} {
		t.Run(failure.kind+string(rune('0'+failure.at)), func(t *testing.T) {
			count := map[string]int{}
			tx := &unitTx{}
			tx.query = func(string, []any) (pgx.Rows, error) {
				count["query"]++
				if failure.kind == "query" && count["query"] == failure.at {
					return nil, errUnitStorage
				}
				rows := &unitRows{data: [][]any{{1}}}
				if failure.kind == "rows" && count["query"] == failure.at {
					rows.err = errUnitStorage
				}
				return rows, nil
			}
			tx.row = func(string, []any) pgx.Row {
				if failure.kind == "row" {
					return unitRow{err: errUnitStorage}
				}
				return unitRow{values: []any{"+79001234567"}}
			}
			tx.exec = func(string, []any) (pgconn.CommandTag, error) {
				count["exec"]++
				if failure.kind == "exec" && count["exec"] == failure.at {
					return pgconn.CommandTag{}, errUnitStorage
				}
				return pgconn.NewCommandTag("DELETE 1"), nil
			}
			if err := r.Remove(unitContext(tx), "00000000-0000-0000-0000-000000000001"); err == nil {
				t.Fatal("deletion continued after storage failure", failure)
			}
		})
	}
}
