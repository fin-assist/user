package apitest_test

import (
	"testing"
	"time"

	"github.com/fin-assist/user/internal/app"
	"github.com/fin-assist/user/internal/app/apitest"
)

func TestIntegrationBusinessClockControlsAbandonedEvidenceCleanup(t *testing.T) {
	now := time.Date(2032, time.February, 29, 23, 59, 0, 0, time.UTC)
	env := apitest.StartWithOptions(t, []app.Option{app.WithClock(func() time.Time { return now })})
	seedDocuments(t, env)
	id := seedRegistration(t, env, "expired-clock-proof", "+79001234567", now.Add(-26*time.Hour))
	var before int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM phone_consent_receipt WHERE attempt_id=$1`, id).Scan(&before); err != nil || before != 1 {
		t.Fatal("missing cleanup fixture", before, err)
	}
	// Execute the assembled phone job's work without a public RPC or wall-clock wait.
	if _, err := env.App.Verifications.Cleanup(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	var receipts, requests int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT (SELECT count(*) FROM phone_consent_receipt WHERE attempt_id=$1),(SELECT count(*) FROM phone_verification WHERE id=$1)`, id).Scan(&receipts, &requests); err != nil || receipts != 0 || requests != 0 {
		t.Fatal("cleanup used wall time instead of application time", receipts, requests, err)
	}
}
