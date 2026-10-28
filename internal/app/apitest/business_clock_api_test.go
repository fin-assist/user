package apitest_test

import (
	"sync/atomic"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/user/internal/app"
	"github.com/fin-assist/user/internal/app/apitest"
)

func TestAPIBusinessClockControlsUnknownAndOriginalKeyWindow(t *testing.T) {
	base := time.Date(2032, time.February, 29, 23, 59, 0, 0, time.UTC)
	var instant atomic.Int64
	instant.Store(base.UnixNano())
	now := func() time.Time { return time.Unix(0, instant.Load()).UTC() }
	provider := &evidenceProvider{}
	provider.setMode("unknown")
	env := apitest.StartWithOptions(t, []app.Option{app.WithClock(now), app.WithPhoneBackend(provider, creationSelector{})})
	provider.env = env
	seedDocuments(t, env)
	client := userv1.NewAuthServiceClient(env.Conn)
	key, req := authKey(), consentStartRequest("+79001234567")
	_, err := client.StartPhoneVerification(authContext(t.Context(), key), req)
	if status.Code(err) != codes.Internal {
		t.Fatal("expected recoverable provider uncertainty", err)
	}
	assertRequestPermission(t, err, true)
	var accepted, deadline, keyEnd time.Time
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT e.consent_accepted_at,v.unknown_deadline,e.key_expires_at FROM phone_consent_receipt e JOIN phone_verification v ON v.id=e.attempt_id WHERE e.start_key=$1`, key).Scan(&accepted, &deadline, &keyEnd); err != nil || !accepted.Equal(base) || !deadline.Equal(base.Add(5*time.Minute)) || !keyEnd.Equal(base.Add(24*time.Hour)) {
		t.Fatal("assembly clock did not reach receipt/attempt", accepted, deadline, keyEnd, err)
	}
	instant.Store(deadline.UnixNano())
	_, err = client.StartPhoneVerification(authContext(t.Context(), key), req)
	if status.Code(err) != codes.Internal {
		t.Fatal("unknown cutoff was not terminal", err)
	}
	assertRequestPermission(t, err, false)
	if creates, recovers := provider.counts(); creates != 1 || recovers != 0 {
		t.Fatal("expired unknown was sent to provider", creates, recovers)
	}
	// The same UUID becomes a fresh intent only after its original 24-hour window.
	instant.Store(keyEnd.Add(time.Microsecond).UnixNano())
	_, err = client.StartPhoneVerification(authContext(t.Context(), key), req)
	if status.Code(err) != codes.Internal {
		t.Fatal("expected fresh uncertain attempt", err)
	}
	assertRequestPermission(t, err, true)
	if creates, recovers := provider.counts(); creates != 2 || recovers != 0 {
		t.Fatal("expired key used archived proof instead of a fresh intent", creates, recovers)
	}
	var fresh time.Time
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT consent_accepted_at FROM phone_consent_receipt WHERE start_key=$1 ORDER BY key_expires_at DESC LIMIT 1`, key).Scan(&fresh); err != nil || !fresh.Equal(now()) {
		t.Fatal("fresh key window did not use application clock", fresh, err)
	}
}

func TestAPIBusinessClockControlsDeletedEvidenceCalendarRetention(t *testing.T) {
	base := time.Date(2032, time.February, 29, 23, 59, 0, 0, time.UTC)
	var instant atomic.Int64
	instant.Store(base.UnixNano())
	now := func() time.Time { return time.Unix(0, instant.Load()).UTC() }
	env := apitest.StartWithOptions(t, []app.Option{app.WithClock(now)})
	seedProfiles(t, env)
	id := seedRegistration(t, env, "clock-proof", "+79001234567", base.Add(-time.Minute))
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE phone_consent_receipt SET linked_user_id=$2::uuid,retain_until=NULL WHERE attempt_id=$1`, id, profileUserA); err != nil {
		t.Fatal(err)
	}
	profiles := userv1.NewProfileServiceClient(env.Conn)
	ctx := apitest.User(t.Context(), profileUserA)
	if _, err := profiles.DeleteAccount(ctx, &userv1.DeleteAccountRequest{}); err != nil {
		t.Fatal(err)
	}
	want := base.AddDate(3, 0, 0)
	var retained time.Time
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT retain_until FROM phone_consent_receipt WHERE attempt_id=$1`, id).Scan(&retained); err != nil || !retained.Equal(want) {
		t.Fatal("deletion retention did not use injected calendar date", retained, want, err)
	}
	instant.Store(want.Add(-time.Microsecond).UnixNano())
	if _, err := profiles.DeleteAccount(ctx, &userv1.DeleteAccountRequest{}); err != nil {
		t.Fatal("repeat deletion", err)
	}
	if _, err := env.App.Verifications.Cleanup(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	var receipts int
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM phone_consent_receipt WHERE attempt_id=$1 AND retain_until=$2`, id, want).Scan(&receipts); err != nil || receipts != 1 {
		t.Fatal("receipt removed early or repeat extended retention", receipts, err)
	}
	instant.Store(want.UnixNano())
	if _, err := env.App.Verifications.Cleanup(t.Context(), 1000); err != nil {
		t.Fatal(err)
	}
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM phone_consent_receipt WHERE attempt_id=$1`, id).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatal("expired receipt survived application-clock cleanup", receipts, err)
	}
}
