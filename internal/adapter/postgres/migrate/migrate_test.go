package migrate_test

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/user/internal/adapter/postgres/migrate"
	"github.com/fin-assist/user/internal/adapter/postgres/migrate/migratetest"
)

func sub(t *testing.T, dir string) fs.FS {
	t.Helper()
	f, err := fs.Sub(os.DirFS("testdata"), dir)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func cfg(dsn string) migrate.Config {
	c := migrate.DefaultConfig(dsn)
	c.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return c
}

func TestValidate(t *testing.T) {
	file := func(s string) *fstest.MapFile { return &fstest.MapFile{Data: []byte(s)} }
	cases := []struct {
		name  string
		files fstest.MapFS
		err   string
	}{
		{"pair", fstest.MapFS{"000001_a.up.sql": file("x"), "000001_a.down.sql": file("x"), "README.md": file("x")}, ""},
		{"no down", fstest.MapFS{"000001_a.up.sql": file("x")}, "has no down file"},
		{"no up", fstest.MapFS{"000001_a.down.sql": file("x")}, "has no up file"},
		{"bad name", fstest.MapFS{"1-a.up.sql": file("x")}, "does not match"},
		{"upper case", fstest.MapFS{"000001_Add.up.sql": file("x"), "000001_Add.down.sql": file("x")}, "does not match"},
		{"version reused", fstest.MapFS{
			"000001_a.up.sql": file("x"), "000001_a.down.sql": file("x"),
			"000001_b.up.sql": file("x"), "000001_b.down.sql": file("x"),
		}, "is used by"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := migrate.Validate(migrate.Source{Name: "s", Table: "t", FS: c.files})
			if c.err == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
				t.Fatalf("error %v, want %q", err, c.err)
			}
		})
	}
}

func TestSourcesMustNotShareTable(t *testing.T) {
	a := migrate.Source{Name: "a", Table: "schema_migrations", FS: fstest.MapFS{}}
	b := migrate.Source{Name: "b", Table: "schema_migrations", FS: fstest.MapFS{}}
	err := migrate.Up(context.Background(), cfg("postgres://unused"), []migrate.Source{a, b})
	if err == nil || !strings.Contains(err.Error(), "used by two sources") {
		t.Fatalf("error %v, want shared table rejected before connecting", err)
	}
}

func TestTestdataUpDownUp(t *testing.T) {
	for _, dir := range []string{"service", "library"} {
		t.Run(dir, func(t *testing.T) {
			migratetest.CheckUpDownUp(t, migrate.Source{Name: dir, Table: dir + "_schema_migrations", FS: sub(t, dir)})
		})
	}
}

func TestUpAppliesLibrariesThenServiceWithSeparateVersions(t *testing.T) {
	dsn := migratetest.DSN(t)
	srcs := []migrate.Source{
		{Name: "outbox", Table: "outbox_schema_migrations", FS: sub(t, "library")},
		{Name: "service", Table: "schema_migrations", FS: sub(t, "service")},
	}
	ctx := context.Background()
	if err := migrate.Up(ctx, cfg(dsn), srcs); err != nil {
		t.Fatal(err)
	}
	// A second run is a no-op.
	if err := migrate.Up(ctx, cfg(dsn), srcs); err != nil {
		t.Fatal(err)
	}
	st, err := migrate.Versions(ctx, cfg(dsn), srcs)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]uint{"outbox": 1, "service": 4}
	for _, s := range st {
		if s.Version != want[s.Source] || s.Dirty {
			t.Errorf("%s: version %d dirty %v, want %d clean", s.Source, s.Version, s.Dirty, want[s.Source])
		}
	}
	for _, table := range []string{"lib_outbox", "note", "outbox_schema_migrations", "schema_migrations"} {
		if !exists(t, dsn, table) {
			t.Errorf("table %s does not exist", table)
		}
	}
}

func TestEmptySourceIsSkipped(t *testing.T) {
	dsn := migratetest.DSN(t)
	srcs := []migrate.Source{{Name: "service", Table: "schema_migrations", FS: fstest.MapFS{"README.md": {Data: []byte("x")}}}}
	if err := migrate.Up(context.Background(), cfg(dsn), srcs); err != nil {
		t.Fatal(err)
	}
	st, err := migrate.Versions(context.Background(), cfg(dsn), srcs)
	if err != nil || len(st) != 1 || st[0].Version != 0 {
		t.Fatalf("versions %v, err %v; want version 0", st, err)
	}
}

// Two runs at once (two Jobs, or a Job and a deploy script): the second waits for the
// first even though lock_timeout is far shorter than the migration, and nothing is
// applied twice.
func TestConcurrentRunsWaitForEachOther(t *testing.T) {
	dsn := migratetest.DSN(t)
	slow := fstest.MapFS{
		"000001_slow.up.sql":   {Data: []byte("SELECT pg_sleep(2); CREATE TABLE slow (id int);")},
		"000001_slow.down.sql": {Data: []byte("DROP TABLE slow;")},
	}
	srcs := []migrate.Source{{Name: "service", Table: "schema_migrations", FS: slow}}
	c := cfg(dsn)
	c.LockTimeout = 100 * time.Millisecond

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = migrate.Up(context.Background(), c, srcs)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("run %d: %v", i, err)
		}
	}
}

func TestWaitTimeout(t *testing.T) {
	dsn := migratetest.DSN(t)
	// Hold the queue lock of the source from outside, as a hung run would.
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext('fin-assist/migrate/schema_migrations'))`); err != nil {
		t.Fatal(err)
	}
	c := cfg(dsn)
	c.WaitTimeout = 300 * time.Millisecond
	err = migrate.Up(ctx, c, []migrate.Source{{Name: "service", Table: "schema_migrations", FS: sub(t, "service")}})
	if err == nil || !strings.Contains(err.Error(), "still runs after") {
		t.Fatalf("error %v, want wait timeout", err)
	}
}

// A failed migration leaves the source dirty at its version; statements of the failed
// file before the error are rolled back (the file runs as one implicit transaction);
// after checking the schema by hand, force sets the version back.
func TestFailedMigrationIsDirtyUntilForced(t *testing.T) {
	dsn := migratetest.DSN(t)
	src := migrate.Source{Name: "service", Table: "schema_migrations", FS: sub(t, "broken")}
	ctx := context.Background()

	if err := migrate.Up(ctx, cfg(dsn), []migrate.Source{src}); err == nil {
		t.Fatal("want an error from the broken migration")
	}
	st, err := migrate.Versions(ctx, cfg(dsn), []migrate.Source{src})
	if err != nil {
		t.Fatal(err)
	}
	if st[0].Version != 2 || !st[0].Dirty {
		t.Fatalf("got %+v, want version 2 dirty", st[0])
	}
	if exists(t, dsn, "half_done") {
		t.Fatal("half_done exists: the failed file was not rolled back as a whole")
	}
	// Up refuses to continue on a dirty source.
	if err := migrate.Up(ctx, cfg(dsn), []migrate.Source{src}); err == nil || !strings.Contains(err.Error(), "Dirty") {
		t.Fatalf("error %v, want dirty", err)
	}
	if err := migrate.Force(ctx, cfg(dsn), src, 1); err != nil {
		t.Fatal(err)
	}
	st, _ = migrate.Versions(ctx, cfg(dsn), []migrate.Source{src})
	if st[0].Version != 1 || st[0].Dirty {
		t.Fatalf("after force: %+v, want version 1 clean", st[0])
	}
}

func TestDownRollsBackStepsAndRefusesIrreversible(t *testing.T) {
	dsn := migratetest.DSN(t)
	src := migrate.Source{Name: "service", Table: "schema_migrations", FS: sub(t, "service")}
	ctx := context.Background()
	if err := migrate.Up(ctx, cfg(dsn), []migrate.Source{src}); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Down(ctx, cfg(dsn), src, 0); err == nil {
		t.Fatal("zero steps must be rejected")
	}
	// The last step is irreversible: its down fails and leaves the source dirty.
	if err := migrate.Down(ctx, cfg(dsn), src, 1); err == nil || !strings.Contains(err.Error(), "irreversible") {
		t.Fatalf("error %v, want the irreversible step refused", err)
	}
	if err := migrate.Force(ctx, cfg(dsn), src, 4); err != nil {
		t.Fatal(err)
	}
	st, _ := migrate.Versions(ctx, cfg(dsn), []migrate.Source{src})
	if st[0].Version != 4 || st[0].Dirty {
		t.Fatalf("got %+v, want version 4 clean", st[0])
	}
}

func TestCancelledRunStops(t *testing.T) {
	dsn := migratetest.DSN(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := migrate.Up(ctx, cfg(dsn), []migrate.Source{{Name: "service", Table: "schema_migrations", FS: sub(t, "service")}})
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v, want context.Canceled", err)
	}
}

func TestFind(t *testing.T) {
	srcs := []migrate.Source{{Name: "outbox"}, {Name: "service"}}
	if s, err := migrate.Find(srcs, "service"); err != nil || s.Name != "service" {
		t.Fatalf("got %v %v", s, err)
	}
	if _, err := migrate.Find(srcs, "nope"); err == nil || !strings.Contains(err.Error(), "outbox service") {
		t.Fatalf("error %v, want known sources listed", err)
	}
}

func exists(t *testing.T, dsn, table string) bool {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var ok bool
	if err := conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}
