package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	usersettingsv1 "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	idempotency "github.com/fin-assist/go-idempotency"
	outboxlib "github.com/fin-assist/go-outbox"
	tombstone "github.com/fin-assist/go-tombstone"
	"google.golang.org/protobuf/proto"

	events "github.com/fin-assist/user/internal/adapter/outbox"
	"github.com/fin-assist/user/internal/adapter/postgres/migrate"
	"github.com/fin-assist/user/internal/adapter/postgres/migrate/migratetest"
	"github.com/fin-assist/user/internal/profile"
	"github.com/fin-assist/user/internal/reqctx"
	"github.com/fin-assist/user/migrations"
)

const profileID = "019c7714-3b77-74d1-9866-e1f484aae2ab"
const otherProfileID = "019c7714-3b77-74d1-9866-e1f484aae2ac"

func profileDB(t *testing.T) (*DB, *profile.Service, *Profiles) {
	t.Helper()
	primary, replica := migratetest.DSN(t), migratetest.DSN(t)
	sources := []migrate.Source{{Name: idempotency.MigrationsName, Table: idempotency.MigrationsTable, FS: idempotency.Migrations}, {Name: outboxlib.MigrationsName, Table: outboxlib.MigrationsTable, FS: outboxlib.Migrations}, {Name: tombstone.MigrationsName, Table: tombstone.MigrationsTable, FS: tombstone.Migrations}, {Name: "service", Table: "schema_migrations", FS: migrations.FS}}
	for _, dsn := range []string{primary, replica} {
		if err := migrate.Up(t.Context(), migrate.DefaultConfig(dsn), sources); err != nil {
			t.Fatal(err)
		}
	}
	db, err := Open(t.Context(), Options{PrimaryDSN: primary, ReplicaDSN: replica, MaxConns: 6})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	_, err = db.Primary().Exec(t.Context(), `INSERT INTO consent_document(type,version,title,url,is_current) VALUES('assistant','v1','Old test','https://example.test/v1',false),('assistant','v2','Current test','https://example.test/v2',true);INSERT INTO app_user(id,phone) VALUES('019c7714-3b77-74d1-9866-e1f484aae2ab','+79001234567'),('019c7714-3b77-74d1-9866-e1f484aae2ac','+79001234568')`)
	if err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC) }
	guard, err := tombstone.New(db, db.Primary(), clock)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := outboxlib.NewPublisher(db, clock)
	if err != nil {
		t.Fatal(err)
	}
	repo := NewProfiles(db)
	return db, profile.New(db, repo, guard, events.NewEvents(publisher), clock), repo
}

func profileReason(err error) string {
	rest, ok := finerr.ToREST(err)
	if !ok {
		return ""
	}
	return rest.Code
}

func TestIntegrationProfilesSettingsOutboxAndPrimaryReads(t *testing.T) {
	db, s, repo := profileDB(t)
	if _, err := repo.Read(t.Context(), profileID); profileReason(err) != "NOT_FOUND" {
		t.Fatalf("replica was bypassed: %v", err)
	}
	p, err := s.Get(reqctx.WithReadPrimary(t.Context()), profileID)
	if err != nil || p.Phone != "+79001234567" || p.Consent.Granted || p.Consent.Version != nil || p.Consent.CurrentVersion != "v2" {
		t.Fatalf("%+v %v", p, err)
	}
	dark, zone := "dark", "Asia/Tokyo"
	updated, err := s.Update(t.Context(), profileID, profile.Patch{Theme: &dark, Timezone: &zone})
	if err != nil || updated.SettingsVersion != 2 || updated.Theme != dark || updated.Timezone != zone {
		t.Fatalf("%+v %v", updated, err)
	}
	var b []byte
	var key, topic string
	if err := db.Primary().QueryRow(t.Context(), `SELECT event_key,topic,payload FROM outbox_events`).Scan(&key, &topic, &b); err != nil {
		t.Fatal(err)
	}
	var message usersettingsv1.SetUserTimezoneRequest
	if err := proto.Unmarshal(b, &message); err != nil {
		t.Fatal(err)
	}
	if key != profileID || topic != events.TimezoneTopic || message.GetUserId() != profileID || message.GetTimezone() != zone || message.GetSettingsVersion() != 2 {
		t.Fatalf("event: %s %s %v", key, topic, &message)
	}
	if _, err := s.Update(t.Context(), profileID, profile.Patch{Timezone: &zone}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.Primary().QueryRow(t.Context(), `SELECT count(*) FROM outbox_events`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("retry created event: %d %v", count, err)
	}
	other, err := repo.Current(t.Context(), otherProfileID)
	if err != nil || other.Theme != "system" || other.SettingsVersion != 1 {
		t.Fatalf("foreign profile changed: %+v %v", other, err)
	}
	if err := db.Run(t.Context(), func(ctx context.Context) error {
		return repo.Update(ctx, "019c7714-3b77-74d1-9866-e1f484aae2ad", "light", "UTC", 1, time.Now())
	}); profileReason(err) != "NOT_FOUND" {
		t.Fatal(err)
	}
}

func TestIntegrationProfilesConsentHistoryAndVersionSwitch(t *testing.T) {
	db, s, repo := profileDB(t)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			if _, err := s.Grant(t.Context(), profileID, "v2"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	var total int
	if err := db.Primary().QueryRow(t.Context(), `SELECT count(*) FROM user_consent WHERE user_id=$1`, profileID).Scan(&total); err != nil || total != 1 {
		t.Fatalf("duplicate grants: %d %v", total, err)
	}
	if err := s.Revoke(t.Context(), profileID); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(t.Context(), profileID); err != nil {
		t.Fatal(err)
	}
	if c, err := s.Status(t.Context(), profileID); err != nil || c.Granted || c.Version != nil {
		t.Fatalf("primary revoke: %+v %v", c, err)
	}
	if _, err := s.Grant(t.Context(), profileID, "v2"); err != nil {
		t.Fatal(err)
	}
	_, err := db.Primary().Exec(t.Context(), `UPDATE consent_document SET is_current=false WHERE type='assistant';INSERT INTO consent_document(type,version,title,url,is_current) VALUES('assistant','v3','New test','https://example.test/v3',true)`)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Status(t.Context(), profileID)
	if err != nil || c.Granted || c.Version == nil || *c.Version != "v2" || c.CurrentVersion != "v3" {
		t.Fatalf("outdated consent effective: %+v %v", c, err)
	}
	if _, err := s.Grant(t.Context(), profileID, "v2"); profileReason(err) != "CONSENT_OUTDATED" {
		t.Fatal(err)
	}
	if _, err := s.Grant(t.Context(), profileID, "v3"); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := db.Primary().QueryRow(t.Context(), `SELECT count(*),count(*) FILTER(WHERE revoked_at IS NULL) FROM user_consent WHERE user_id=$1`, profileID).Scan(&total, &active); err != nil || total != 3 || active != 1 {
		t.Fatalf("history %d active %d err %v", total, active, err)
	}
	var invalid int
	if err := db.Primary().QueryRow(t.Context(), `SELECT count(*) FROM user_consent WHERE revoked_at<granted_at`).Scan(&invalid); err != nil || invalid != 0 {
		t.Fatal("invalid revocation time")
	}
	other, err := repo.Current(t.Context(), otherProfileID)
	if err != nil || other.Consent.Granted {
		t.Fatal("foreign grant")
	}
	for _, tt := range []struct{ id, version, want string }{{otherProfileID, "missing", "CONSENT_OUTDATED"}, {"019c7714-3b77-74d1-9866-e1f484aae2ad", "v3", "NOT_FOUND"}} {
		err := db.Run(t.Context(), func(ctx context.Context) error { return repo.Grant(ctx, tt.id, tt.version, time.Now()) })
		if profileReason(err) != tt.want {
			t.Fatalf("foreign key mapping: %v", err)
		}
	}
	if err := db.Run(t.Context(), func(ctx context.Context) error { return repo.Grant(ctx, profileID, "v3", time.Now()) }); !errors.Is(err, profile.ErrConsentConflict) {
		t.Fatalf("unique mapping: %v", err)
	}
}

type failedSettings struct{}

func (failedSettings) TimezoneChanged(context.Context, string, string, int32) error {
	return errors.New("publisher failed")
}

func TestIntegrationProfilesRollbackConcurrencyAndDeletion(t *testing.T) {
	db, s, repo := profileDB(t)
	guard, err := tombstone.New(db, db.Primary(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	bad := profile.New(db, repo, guard, failedSettings{}, time.Now)
	zone := "UTC"
	if _, err := bad.Update(t.Context(), profileID, profile.Patch{Timezone: &zone}); err == nil {
		t.Fatal("publisher failure hidden")
	}
	p, err := repo.Current(t.Context(), profileID)
	if err != nil || p.SettingsVersion != 1 || p.Timezone != "Europe/Moscow" {
		t.Fatalf("partial commit: %+v %v", p, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Go(func() {
			zone := []string{"UTC", "Asia/Tokyo", "Europe/Moscow", "America/New_York"}[i%4]
			if _, err := s.Update(t.Context(), profileID, profile.Patch{Timezone: &zone}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	p, err = repo.Current(t.Context(), profileID)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Primary().Query(t.Context(), `SELECT payload FROM outbox_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	version := int32(1)
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		var m usersettingsv1.SetUserTimezoneRequest
		if err := proto.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		version++
		if m.GetSettingsVersion() != version {
			t.Fatalf("non-monotonic event: %v expected %d", &m, version)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if version != p.SettingsVersion {
		t.Fatalf("missing event: %d profile %d", version, p.SettingsVersion)
	}
	barrier := make(chan struct{})
	for range 12 {
		wg.Go(func() {
			<-barrier
			_, err := s.Update(t.Context(), profileID, profile.Patch{})
			if err != nil && profileReason(err) != "TOKEN_REVOKED" {
				t.Errorf("write/delete race: %v", err)
			}
		})
	}
	wg.Go(func() {
		<-barrier
		if err := db.Run(t.Context(), func(ctx context.Context) error {
			return guard.Purge(ctx, profileID, func(ctx context.Context, id string) error {
				_, err := db.Writer(ctx).Exec(ctx, `DELETE FROM app_user WHERE id=$1`, id)
				return err
			})
		}); err != nil {
			t.Error(err)
		}
	})
	close(barrier)
	wg.Wait()
	if _, err := s.Grant(t.Context(), profileID, "v2"); profileReason(err) != "TOKEN_REVOKED" {
		t.Fatal(err)
	}
	if _, err := s.Status(t.Context(), profileID); profileReason(err) != "TOKEN_REVOKED" {
		t.Fatal(err)
	}
	var count int
	if err := db.Primary().QueryRow(t.Context(), `SELECT count(*) FROM app_user WHERE id=$1`, profileID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("resurrection: %d %v", count, err)
	}
}

func TestIntegrationProfilesTransactionAndStorageErrors(t *testing.T) {
	db, s, repo := profileDB(t)
	for name, fn := range map[string]func() error{"lock": func() error { return repo.Lock(t.Context(), profileID) }, "update": func() error { return repo.Update(t.Context(), profileID, "dark", "UTC", 1, time.Now()) }, "grant": func() error { return repo.Grant(t.Context(), profileID, "v2", time.Now()) }, "revoke": func() error { return repo.Revoke(t.Context(), profileID, time.Now()) }} {
		if err := fn(); err == nil {
			t.Fatalf("%s outside transaction", name)
		}
	}
	missing := "019c7714-3b77-74d1-9866-e1f484aae2ad"
	if _, err := s.Update(t.Context(), missing, profile.Patch{}); profileReason(err) != "NOT_FOUND" {
		t.Fatal(err)
	}
	if err := s.Revoke(t.Context(), missing); profileReason(err) != "NOT_FOUND" {
		t.Fatal(err)
	}
	if _, err := s.Status(t.Context(), missing); profileReason(err) != "NOT_FOUND" {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Update(ctx, profileID, profile.Patch{}); err == nil {
		t.Fatal("cancelled write")
	}
	for _, table := range []string{"user_consent", "app_user"} {
		if _, err := db.Primary().Exec(t.Context(), fmt.Sprintf("DROP TABLE %s CASCADE", table)); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.Current(t.Context(), profileID); err == nil {
			t.Fatal("storage failure hidden")
		}
		calls := []func(context.Context) error{
			func(ctx context.Context) error { return repo.Grant(ctx, profileID, "v2", time.Now()) },
			func(ctx context.Context) error { return repo.Revoke(ctx, profileID, time.Now()) },
		}
		if table == "app_user" {
			calls = []func(context.Context) error{
				func(ctx context.Context) error { return repo.Lock(ctx, profileID) },
				func(ctx context.Context) error { return repo.Update(ctx, profileID, "dark", "UTC", 2, time.Now()) },
			}
		}
		for _, call := range calls {
			if err := db.Run(t.Context(), call); err == nil || profileReason(err) != "" {
				t.Fatalf("SQL error hidden/misclassified: %v", err)
			}
		}
	}
}
