package postgres

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func seedStoredEvidence(t *testing.T, pool *pgxpool.Pool, id int32) {
	t.Helper()
	_, err := pool.Exec(t.Context(), `INSERT INTO consent_document(type,version,title,url,is_current) VALUES('personal_data','v1','Test','https://example.test/pd',false) ON CONFLICT(type,version) DO NOTHING`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(t.Context(), `INSERT INTO phone_consent_receipt(attempt_id,phone,client_ip,consent_version,consent_accepted_at,age_confirmed_at,attempt_end,retain_until,key_expires_at) SELECT id,phone,'203.0.113.7','v1',created_at,created_at,expires_at,expires_at+interval '24 hours',created_at+interval '24 hours' FROM phone_verification WHERE id=$1`, id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(t.Context(), `UPDATE phone_verification SET consent_receipt_id=id,unknown_deadline=expires_at,first_dispatch_at=created_at,dispatch_not_after=expires_at WHERE id=$1`, id)
	if err != nil {
		t.Fatal(err)
	}
}
