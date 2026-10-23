package postgres

import (
	"context"
	"testing"

	"github.com/fin-assist/user/internal/adapter/grpc/server"
	"github.com/fin-assist/user/internal/adapter/postgres/migrate"
	"github.com/fin-assist/user/internal/adapter/postgres/migrate/migratetest"
	"github.com/fin-assist/user/migrations"
)

func TestConsentDocumentsPrimaryAndVersionSwitch(t *testing.T) {
	dsn := migratetest.DSN(t)
	if err := migrate.Up(t.Context(), migrate.DefaultConfig(dsn), []migrate.Source{{Name: "service", Table: "schema_migrations", FS: migrations.FS}}); err != nil {
		t.Fatal(err)
	}
	db, err := Open(t.Context(), Options{PrimaryDSN: dsn, ReplicaDSN: migratetest.DSN(t), MaxConns: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := NewConsentDocuments(db)
	if _, err := repo.Current(t.Context(), "personal_data"); server.Reason(err) != "NOT_FOUND" {
		t.Fatal(err)
	}
	_, err = db.Primary().Exec(t.Context(), `INSERT INTO consent_document(type,version,title,url,is_current) VALUES ('personal_data','v1','Old','https://example.test/v1',true),('personal_data','v2','New','https://example.test/v2',false)`)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := repo.Current(t.Context(), "personal_data"); err != nil || d.Version != "v1" {
		t.Fatalf("primary: %+v %v", d, err)
	}
	err = db.Run(t.Context(), func(ctx context.Context) error {
		if _, err := db.Writer(ctx).Exec(ctx, `UPDATE consent_document SET is_current=false WHERE type='personal_data'; UPDATE consent_document SET is_current=true WHERE type='personal_data' AND version='v2'`); err != nil {
			return err
		}
		d, err := repo.Current(ctx, "personal_data")
		if err == nil && (d.Version != "v2" || d.Title != "New" || d.URL != "https://example.test/v2" || d.Type != "personal_data") {
			t.Fatalf("inside transaction: %+v", d)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if d, err := repo.Current(t.Context(), "personal_data"); err != nil || d.Version != "v2" {
		t.Fatalf("new current: %+v %v", d, err)
	}
	if _, err := repo.Current(t.Context(), "personal_data' OR true --"); server.Reason(err) != "NOT_FOUND" {
		t.Fatalf("unparameterized type: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := repo.Current(ctx, "personal_data"); err == nil {
		t.Fatal("cancelled query succeeded")
	}
	if _, err := db.Primary().Exec(t.Context(), `DROP TABLE consent_document CASCADE`); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Current(t.Context(), "personal_data"); err == nil || server.Reason(err) != "" {
		t.Fatalf("database error hidden as domain error: %v", err)
	}
}
