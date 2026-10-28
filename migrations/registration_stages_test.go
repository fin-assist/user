package migrations_test

import (
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/fin-assist/go-migrate"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fin-assist/user/internal/testdb"
	"github.com/fin-assist/user/migrations"
)

func receiptSourceThrough(t *testing.T, version int) migrate.Source {
	t.Helper()
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	selected := fstest.MapFS{}
	for _, name := range files {
		prefix, _, ok := strings.Cut(name, "_")
		v, err := strconv.Atoi(prefix)
		if !ok || err != nil {
			t.Fatal("invalid embedded migration name", name)
		}
		if v > version {
			continue
		}
		data, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		selected[name] = &fstest.MapFile{Data: data}
	}
	src := source()
	src.FS = selected
	return src
}

func TestIntegrationReceiptMigrationStagesValidateFKAndAttachConcurrentIndex(t *testing.T) {
	dsn := testdb.DSN(t)
	if err := migrate.Up(t.Context(), config(dsn), []migrate.Source{receiptSourceThrough(t, 5)}); err != nil {
		t.Fatal(err)
	}
	db, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	var validated bool
	if err := db.QueryRow(t.Context(), `SELECT convalidated FROM pg_constraint WHERE conrelid='phone_verification'::regclass AND conname='phone_verification_consent_receipt_id_fkey'`).Scan(&validated); err != nil || validated {
		t.Fatal("receipt FK scanned the existing table during 000005", validated, err)
	}
	// NOT VALID skips only the old-row scan; new invalid references still fail.
	violates(t, db, "phone_verification_consent_receipt_id_fkey", `INSERT INTO phone_verification(phone,token_hash,provider,created_at,expires_at,consent_receipt_id) VALUES($1,$2,'test',$3,$4,123456)`, phoneA, hash("invalid-proof"), t0, t0.AddDate(0, 0, 1))
	if err := migrate.Up(t.Context(), config(dsn), []migrate.Source{receiptSourceThrough(t, 6)}); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(t.Context(), `SELECT convalidated FROM pg_constraint WHERE conrelid='phone_verification'::regclass AND conname='phone_verification_consent_receipt_id_fkey'`).Scan(&validated); err != nil || !validated {
		t.Fatal("separate 000006 did not validate the receipt FK", validated, err)
	}
	if err := migrate.Up(t.Context(), config(dsn), []migrate.Source{receiptSourceThrough(t, 7)}); err != nil {
		t.Fatal(err)
	}
	var ready bool
	var definition string
	if err := db.QueryRow(t.Context(), `SELECT indisunique AND indisvalid AND indisready FROM pg_index WHERE indexrelid='phone_verification_provider_campaign_call_uidx'::regclass`).Scan(&ready); err != nil || !ready {
		t.Fatal("000007 did not build a usable unique index", ready, err)
	}
	if err := db.QueryRow(t.Context(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='phone_verification'::regclass AND conname='phone_verification_provider_call_key'`).Scan(&definition); err != nil || definition != "UNIQUE (provider, provider_call_id)" {
		t.Fatal("old uniqueness disappeared before the atomic attach step", definition, err)
	}
	first := verification(t, db, phoneA, "first-campaign", t0)
	exec(t, db, `UPDATE phone_verification SET provider_call_id='shared-call',campaign='A' WHERE id=$1`, first)
	violates(t, db, "phone_verification_provider_call_key", `INSERT INTO phone_verification(phone,token_hash,provider,provider_call_id,campaign,created_at,expires_at) VALUES($1,$2,'zvonok','shared-call','B',$3,$4)`, phoneB, hash("before-attach"), t0, t0.AddDate(0, 0, 1))
	if err := migrate.Up(t.Context(), config(dsn), []migrate.Source{source()}); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(t.Context(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='phone_verification'::regclass AND conname='phone_verification_provider_call_key'`).Scan(&definition); err != nil || definition != "UNIQUE (provider, campaign, provider_call_id)" {
		t.Fatal("000008 changed the runtime constraint identity or columns", definition, err)
	}
	exec(t, db, `INSERT INTO phone_verification(phone,token_hash,provider,provider_call_id,campaign,created_at,expires_at) VALUES($1,$2,'zvonok','shared-call','B',$3,$4)`, phoneB, hash("after-attach"), t0, t0.AddDate(0, 0, 1))
	violates(t, db, "phone_verification_provider_call_key", `INSERT INTO phone_verification(phone,token_hash,provider,provider_call_id,campaign,created_at,expires_at) VALUES('+79007654322',$1,'zvonok','shared-call','A',$2,$3)`, hash("same-campaign"), t0, t0.AddDate(0, 0, 1))
	down, err := migrations.FS.ReadFile("000008_attach_provider_campaign_call.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(t.Context(), string(down)); err == nil {
		t.Fatal("downgrade accepted a cross-campaign collision under the old key")
	}
	if err := db.QueryRow(t.Context(), `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='phone_verification'::regclass AND conname='phone_verification_provider_call_key'`).Scan(&definition); err != nil || definition != "UNIQUE (provider, campaign, provider_call_id)" {
		t.Fatal("failed offline downgrade partially removed triple uniqueness", definition, err)
	}
	// Offline rollback requires no retained receipt and no collisions under the
	// old cross-campaign uniqueness. Its standalone index must survive down(8).
	exec(t, db, `DELETE FROM phone_verification`)
	if err := migrate.Down(t.Context(), config(dsn), source(), 1); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(t.Context(), `SELECT indisunique AND indisvalid AND indisready FROM pg_index WHERE indexrelid='phone_verification_provider_campaign_call_uidx'::regclass`).Scan(&ready); err != nil || !ready {
		t.Fatal("down(8) lost the 000007 index required by up(8)", ready, err)
	}
	if err := migrate.Up(t.Context(), config(dsn), []migrate.Source{source()}); err != nil {
		t.Fatal("down(8)->up(8) could not reattach the standalone index", err)
	}
}

func TestIntegrationReceiptDowngradeGuardsRetainedEvidence(t *testing.T) {
	for _, file := range []string{"000008_attach_provider_campaign_call.down.sql", "000005_registration_evidence.down.sql"} {
		t.Run(file, func(t *testing.T) {
			db, _ := schema(t)
			documents(t, db)
			exec(t, db, `INSERT INTO phone_consent_receipt(attempt_id,phone,client_ip,consent_version,consent_accepted_at,age_confirmed_at,attempt_end,retain_until,key_expires_at) VALUES(1,$1,'203.0.113.7','2026-09-28',$2,$2,$3,$4,$4)`, phoneA, t0, t0.AddDate(0, 0, 1), t0.AddDate(0, 0, 2))
			sql, err := migrations.FS.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(t.Context(), string(sql)); err == nil {
				t.Fatal("offline downgrade erased retained evidence")
			}
			var receipts int
			var definition string
			if err := db.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM phone_consent_receipt),pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid='phone_verification'::regclass AND conname='phone_verification_provider_call_key'`).Scan(&receipts, &definition); err != nil || receipts != 1 || definition != "UNIQUE (provider, campaign, provider_call_id)" {
				t.Fatal("guard failure partially changed schema or proof", receipts, definition, err)
			}
		})
	}
}
