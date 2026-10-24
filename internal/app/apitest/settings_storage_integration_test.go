package apitest_test

import (
	"testing"
	"time"

	outboxlib "github.com/fin-assist/go-outbox"

	"github.com/fin-assist/user/internal/adapter/outbox"
	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/profile"
)

func TestIntegrationSettingsRollBackWhenStorageOrOutboxIsUnavailable(t *testing.T) {
	for _, tc := range []struct{ name, breakSQL, restoreSQL string }{
		{"write-column", `ALTER TABLE app_user RENAME COLUMN updated_at TO unavailable`, `ALTER TABLE app_user RENAME COLUMN unavailable TO updated_at`},
		{"outbox", `ALTER TABLE outbox_events RENAME TO unavailable_events`, `ALTER TABLE unavailable_events RENAME TO outbox_events`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := apitest.Start(t)
			seedProfiles(t, env)
			pub, err := outboxlib.NewPublisher(env.App.DB, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			svc := profile.New(env.App.DB, postgres.NewProfiles(env.App.DB), env.App.Tombstones, outbox.NewEvents(pub), time.Now)
			if _, err := env.App.DB.Primary().Exec(t.Context(), tc.breakSQL); err != nil {
				t.Fatal(err)
			}
			zone := "Asia/Tokyo"
			if _, err := svc.Update(t.Context(), profileUserA, profile.Patch{Timezone: &zone}); err == nil {
				t.Fatal("partial settings committed")
			}
			if _, err := env.App.DB.Primary().Exec(t.Context(), tc.restoreSQL); err != nil {
				t.Fatal(err)
			}
			got, err := svc.Get(t.Context(), profileUserA)
			if err != nil || got.Timezone != "Europe/Moscow" || got.SettingsVersion != 1 {
				t.Fatal(got, err)
			}
			var n int
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM outbox_events`).Scan(&n); err != nil || n != 0 {
				t.Fatal(n, err)
			}
			if _, err := svc.Update(t.Context(), profileUserA, profile.Patch{Timezone: &zone}); err != nil {
				t.Fatal("retry after restore", err)
			}
		})
	}
	env := apitest.Start(t)
	seedProfiles(t, env)
	pub, err := outboxlib.NewPublisher(env.App.DB, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	svc := profile.New(env.App.DB, postgres.NewProfiles(env.App.DB), env.App.Tombstones, outbox.NewEvents(pub), time.Now)
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE app_user SET settings_version=2147483647 WHERE id=$1`, profileUserA); err != nil {
		t.Fatal(err)
	}
	zone := "Asia/Tokyo"
	if _, err := svc.Update(t.Context(), profileUserA, profile.Patch{Timezone: &zone}); err == nil {
		t.Fatal("version overflow accepted")
	}
	if _, err := svc.Grant(t.Context(), profileUserA, "old"); apitest.Reason(err) != "CONSENT_OUTDATED" {
		t.Fatal(err)
	}
	if _, err := svc.Grant(t.Context(), profileUserA, "v2"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Grant(t.Context(), profileUserA, "v2"); err != nil {
		t.Fatal("idempotent consent", err)
	}
	// Persisted grant time may be ahead after a clock correction. Revocation must
	// remain monotonic rather than violate the actual history interval.
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE user_consent SET granted_at=now()+interval '1 hour' WHERE user_id=$1`, profileUserA); err != nil {
		t.Fatal(err)
	}
	if err := svc.Revoke(t.Context(), profileUserA); err != nil {
		t.Fatal(err)
	}
	var monotonic bool
	if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT revoked_at>=granted_at FROM user_consent WHERE user_id=$1`, profileUserA).Scan(&monotonic); err != nil || !monotonic {
		t.Fatal(monotonic, err)
	}
	if err := svc.Revoke(t.Context(), profileUserA); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"bad", "019c7714-3b77-74d1-9866-e1f484aae2af"} {
		if _, err := svc.Status(t.Context(), id); err == nil {
			t.Fatal("unknown user has consent")
		}
	}
	if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE app_user SET theme='corrupt' WHERE id=$1`, profileUserA); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(t.Context(), profileUserA); err == nil {
		t.Fatal("invalid persisted profile served")
	}
}
