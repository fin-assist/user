package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/adapter/postgres/migrate"
	"github.com/fin-assist/user/internal/adapter/postgres/migrate/migratetest"
	"github.com/fin-assist/user/internal/ratelimit"
	"github.com/fin-assist/user/migrations"
)

func limitsDB(t *testing.T) *DB {
	t.Helper()
	dsn := migratetest.DSN(t)
	if err := migrate.Up(t.Context(), migrate.DefaultConfig(dsn), []migrate.Source{{Name: "service", Table: "schema_migrations", FS: migrations.FS}}); err != nil {
		t.Fatal(err)
	}
	db, err := Open(t.Context(), Options{PrimaryDSN: dsn, ReplicaDSN: migratetest.DSN(t), MaxConns: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	return db
}

func takeLimit(ctx context.Context, db *DB, l *ratelimit.Limiter, op ratelimit.Operation, phone, ip string) (ratelimit.Decision, error) {
	var d ratelimit.Decision
	var receipt ratelimit.Receipt
	err := db.Run(ctx, func(ctx context.Context) error {
		var err error
		d, receipt, err = l.Check(ctx, op, phone, ip)
		return err
	})
	if err == nil {
		receipt.Committed()
	}
	return d, err
}

func TestIntegrationLimitsMultipleWindowsAndReset(t *testing.T) {
	db := limitsDB(t)
	now := time.Date(2026, 12, 31, 23, 59, 30, 0, time.UTC)
	l, err := ratelimit.New(ratelimit.Defaults(), NewLimits(db), func() time.Time { return now }, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if d, err := takeLimit(t.Context(), db, l, ratelimit.Start, "+79001234567", "192.0.2.1"); err != nil || !d.Allowed {
			t.Fatalf("attempt %d: %+v %v", i, d, err)
		}
	}
	d, err := takeLimit(t.Context(), db, l, ratelimit.Start, "+79001234567", "192.0.2.1")
	if err != nil || d.Allowed || !d.RetryAt.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("denial: %+v %v", d, err)
	}
	rows, err := db.Primary().Query(t.Context(), `SELECT hits FROM rate_limit_window`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 3 {
			t.Fatalf("denial consumed another window: %d", n)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	// Equality with the boundary opens the new minute, hour, day and year.
	now = now.Add(30 * time.Second)
	if d, err := takeLimit(t.Context(), db, l, ratelimit.Start, "+79001234567", "192.0.2.1"); err != nil || !d.Allowed {
		t.Fatalf("reset: %+v %v", d, err)
	}
	// With two exhausted windows, wait for the later one, not whichever row is first.
	_, err = db.Primary().Exec(t.Context(), `UPDATE rate_limit_window SET hits=100 WHERE window_start=$1`, now)
	if err != nil {
		t.Fatal(err)
	}
	d, err = takeLimit(t.Context(), db, l, ratelimit.Start, "+79001234567", "192.0.2.1")
	if err != nil || d.Allowed || !d.RetryAt.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("longest retry: %+v %v", d, err)
	}
	// A different phone still shares the exhausted IP window.
	if d, err := takeLimit(t.Context(), db, l, ratelimit.Start, "+79001234568", "192.0.2.1"); err != nil || d.Allowed {
		t.Fatalf("IP bypass: %+v %v", d, err)
	}
}

func TestIntegrationLimitsConcurrentAttemptsCanonicalIPAndScopes(t *testing.T) {
	db := limitsDB(t)
	reg := prometheus.NewRegistry()
	rules := ratelimit.Defaults()
	rules[ratelimit.WatchIP] = ratelimit.Rule{Limit: 7, Window: time.Minute}
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	l, err := ratelimit.New(rules, NewLimits(db), func() time.Time { return now }, reg)
	if err != nil {
		t.Fatal(err)
	}
	var allowed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Go(func() {
			ip := "192.0.2.1"
			if i%2 == 0 {
				ip = "::ffff:192.0.2.1"
			}
			d, err := takeLimit(t.Context(), db, l, ratelimit.Watch, "", ip)
			if err != nil {
				t.Error(err)
			} else if d.Allowed {
				allowed.Add(1)
			}
		})
	}
	wg.Wait()
	if allowed.Load() != 7 {
		t.Fatalf("allowed: %d", allowed.Load())
	}
	var count, hits int
	if err := db.Primary().QueryRow(t.Context(), `SELECT count(*),sum(hits) FROM rate_limit_window`).Scan(&count, &hits); err != nil || count != 1 || hits != 7 {
		t.Fatalf("counters %d %d %v", count, hits, err)
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var consumed float64
	for _, f := range families {
		if f.GetName() == "finassist_rate_limit_consumed_total" {
			for _, m := range f.Metric {
				if m.Label[0].GetValue() == string(ratelimit.WatchIP) {
					consumed = m.Counter.GetValue()
				}
			}
		}
	}
	if consumed != 7 {
		t.Fatalf("committed consumption: %v", consumed)
	}
	for _, tt := range []struct {
		op ratelimit.Operation
		ip string
	}{{ratelimit.Watch, "192.0.2.2"}, {ratelimit.Register, "192.0.2.1"}, {ratelimit.Refresh, "192.0.2.1"}} {
		if d, err := takeLimit(t.Context(), db, l, tt.op, "", tt.ip); err != nil || !d.Allowed {
			t.Fatalf("independent scope/subject: %+v %v", d, err)
		}
	}
}

func TestIntegrationLimitsConcurrentStartOverlappingWindows(t *testing.T) {
	for _, tt := range []struct {
		name        string
		phone, ip   func(int) string
		wantAllowed int
	}{
		{"same_phone_different_ips", func(int) string { return "+79001234567" }, func(i int) string { return fmt.Sprintf("192.0.2.%d", i+1) }, 3},
		{"different_phones_same_ip", func(i int) string { return fmt.Sprintf("+7900%07d", i) }, func(int) string { return "192.0.2.1" }, 5},
		{"crossing_phones_and_ips", func(i int) string { return fmt.Sprintf("+7900%07d", i%4) }, func(i int) string { return fmt.Sprintf("192.0.2.%d", (i/4)%4+1) }, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db := limitsDB(t)
			rules := ratelimit.Defaults()
			rules[ratelimit.VerificationIPShort] = ratelimit.Rule{Limit: 5, Window: time.Minute}
			rules[ratelimit.VerificationIPLong] = ratelimit.Rule{Limit: 8, Window: time.Hour}
			now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
			l, err := ratelimit.New(rules, NewLimits(db), func() time.Time { return now }, prometheus.NewRegistry())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var wg sync.WaitGroup
			start := make(chan struct{})
			var mu sync.Mutex
			phones, ips := map[string]int{}, map[string]int{}
			allowed := 0
			for i := 0; i < 40; i++ {
				wg.Go(func() {
					<-start
					phone, ip := tt.phone(i), tt.ip(i)
					d, err := takeLimit(ctx, db, l, ratelimit.Start, phone, ip)
					if err != nil {
						t.Errorf("concurrent Start: %v", err)
						return
					}
					if d.Allowed {
						mu.Lock()
						allowed++
						phones[phone]++
						ips[ip]++
						mu.Unlock()
					}
				})
			}
			close(start)
			wg.Wait()
			if allowed == 0 || tt.wantAllowed > 0 && allowed != tt.wantAllowed {
				t.Fatalf("allowed: %d, expected %d", allowed, tt.wantAllowed)
			}
			rows, err := db.Primary().Query(ctx, `SELECT scope,subject,hits FROM rate_limit_window`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			totals := map[string]int{}
			for rows.Next() {
				var scope, subject string
				var hits int
				if err := rows.Scan(&scope, &subject, &hits); err != nil {
					t.Fatal(err)
				}
				var expected, limit int
				switch scope {
				case "verification_phone_short:3600":
					expected, limit = phones[subject], 3
				case "verification_phone_long:86400":
					expected, limit = phones[subject], 10
				case "verification_ip_short:60":
					expected, limit = ips[subject], 5
				case "verification_ip_long:3600":
					expected, limit = ips[subject], 8
				default:
					t.Fatalf("unexpected scope %s", scope)
				}
				if hits != expected || hits > limit {
					t.Fatalf("partial/extra consumption: %s hits=%d accepted=%d limit=%d", scope, hits, expected, limit)
				}
				totals[scope] += hits
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if len(totals) != 4 {
				t.Fatalf("missing windows: %v", totals)
			}
			for scope, total := range totals {
				if total != allowed {
					t.Fatalf("%s total=%d allowed=%d", scope, total, allowed)
				}
			}
		})
	}
}

func TestIntegrationLimitsRollbackDurationChangeAndFailures(t *testing.T) {
	db := limitsDB(t)
	repo := NewLimits(db)
	rules := ratelimit.Defaults()
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	l, err := ratelimit.New(rules, repo, func() time.Time { return now }, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	rollback := errors.New("effect failed")
	err = db.Run(t.Context(), func(ctx context.Context) error {
		if _, _, err := l.Check(ctx, ratelimit.Watch, "", "192.0.2.1"); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	var n int
	if err := db.Primary().QueryRow(t.Context(), `SELECT count(*) FROM rate_limit_window`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rollback: %d %v", n, err)
	}
	if _, _, err := l.Check(t.Context(), ratelimit.Watch, "", "192.0.2.1"); err == nil {
		t.Fatal("write outside transaction")
	}
	if err := repo.Increment(t.Context(), nil); err == nil {
		t.Fatal("increment outside transaction")
	}
	windows := []ratelimit.Window{{Scope: ratelimit.WatchIP, Subject: "192.0.2.90", Start: now, End: now.Add(time.Minute), Limit: 60}}
	if err := db.Run(t.Context(), func(ctx context.Context) error { return repo.Increment(ctx, windows) }); err == nil {
		t.Fatal("missing locked window accepted")
	}
	if err := db.Run(t.Context(), func(ctx context.Context) error {
		if _, err := repo.Lock(ctx, windows); err != nil {
			return err
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		return repo.Increment(cancelled, windows)
	}); err == nil {
		t.Fatal("cancelled increment succeeded")
	}
	if d, err := takeLimit(t.Context(), db, l, ratelimit.Watch, "", "192.0.2.1"); err != nil || !d.Allowed {
		t.Fatal(err)
	}
	rules[ratelimit.WatchIP] = ratelimit.Rule{Limit: 2, Window: 2 * time.Minute}
	changed, err := ratelimit.New(rules, repo, func() time.Time { return now }, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if d, err := takeLimit(t.Context(), db, changed, ratelimit.Watch, "", "192.0.2.1"); err != nil || !d.Allowed {
			t.Fatalf("duration reused old row: %+v %v", d, err)
		}
	}
	if d, err := takeLimit(t.Context(), db, changed, ratelimit.Watch, "", "192.0.2.1"); err != nil || d.Allowed || !d.RetryAt.Equal(now.Add(2*time.Minute)) {
		t.Fatalf("new window end: %+v %v", d, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := takeLimit(ctx, db, l, ratelimit.Watch, "", "192.0.2.1"); err == nil {
		t.Fatal("cancelled operation succeeded")
	}
	if err := db.Run(t.Context(), func(ctx context.Context) error { _, err := repo.Lock(ctx, nil); return err }); err == nil {
		t.Fatal("empty windows accepted")
	}
	if _, err := db.Primary().Exec(t.Context(), `UPDATE rate_limit_window SET hits=-1`); err != nil {
		t.Fatal(err)
	}
	if _, err := takeLimit(t.Context(), db, l, ratelimit.Watch, "", "192.0.2.1"); err == nil {
		t.Fatal("negative stored count accepted")
	}
	if _, err := db.Primary().Exec(t.Context(), `DROP TABLE rate_limit_window`); err != nil {
		t.Fatal(err)
	}
	if _, err := takeLimit(t.Context(), db, l, ratelimit.Watch, "", "192.0.2.1"); err == nil {
		t.Fatal("storage failure hidden")
	}
}

func TestIntegrationLimitsCleanupRetainsLiveWindowsAndSkipsLocked(t *testing.T) {
	db := limitsDB(t)
	repo := NewLimits(db)
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 4; i++ {
		if _, err := db.Primary().Exec(t.Context(), `INSERT INTO rate_limit_window VALUES ('watch_ip:60',$1,$2,$3,1)`, string(rune('a'+i)), now.Add(-2*time.Hour), now.Add(time.Duration(i-2)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	// An independently held row is skipped rather than blocking every cleanup worker.
	tx, err := db.Primary().Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(t.Context(), `SELECT 1 FROM rate_limit_window WHERE subject='a' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	if n, err := repo.Cleanup(t.Context(), now, 1); err != nil || n != 1 {
		t.Fatalf("first batch: %d %v", n, err)
	}
	if n, err := repo.Cleanup(t.Context(), now, 10); err != nil || n != 1 {
		t.Fatalf("second batch: %d %v", n, err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n, err := repo.Cleanup(t.Context(), now, 10); err != nil || n != 1 {
		t.Fatalf("unlocked: %d %v", n, err)
	}
	for _, batch := range []int{0, 10001} {
		if _, err := repo.Cleanup(t.Context(), now, batch); err == nil {
			t.Fatal("unbounded batch")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := repo.Cleanup(ctx, now, 1); err == nil {
		t.Fatal("cancelled cleanup")
	}
	var n int
	if err := db.Primary().QueryRow(t.Context(), `SELECT count(*) FROM rate_limit_window`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("active row lost: %d %v", n, err)
	}
}
