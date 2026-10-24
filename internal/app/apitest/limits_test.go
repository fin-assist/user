package apitest_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/ops"
	"github.com/fin-assist/user/internal/ratelimit"
)

func TestIntegrationConfiguredLimitsAndCommittedMetricsAreExposed(t *testing.T) {
	env := apitest.Start(t, func(c *config.Config) {
		c.Limits[ratelimit.WatchIP] = ratelimit.Rule{Limit: 1, Window: 2 * time.Minute}
	})
	for i := 0; i < 2; i++ {
		var receipt ratelimit.Receipt
		err := env.App.DB.Run(t.Context(), func(ctx context.Context) error {
			d, r, err := env.App.Limits.Check(ctx, ratelimit.Watch, "", "192.0.2.45")
			receipt = r
			if err == nil && d.Allowed != (i == 0) {
				t.Fatalf("attempt %d: %+v", i, d)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		receipt.Committed()
	}
	w := httptest.NewRecorder()
	ops.Handler(env.Registry, &ops.Readiness{}).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("scrape: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`finassist_rate_limit_configured_limit{scope="watch_ip"} 1`,
		`finassist_rate_limit_window_seconds{scope="watch_ip"} 120`,
		`finassist_rate_limit_checks_total{result="allowed",scope="watch_ip"} 1`,
		`finassist_rate_limit_checks_total{result="denied",scope="watch_ip"} 1`,
		`finassist_rate_limit_consumed_total{scope="watch_ip"} 1`,
		`finassist_rate_limit_window_utilization_ratio_count{scope="watch_ip"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s", want)
		}
	}
	if strings.Contains(body, "192.0.2.45") {
		t.Fatal("subject leaked into metrics")
	}
	// The configured cleanup worker is present and returns on shutdown.
	found := false
	for _, job := range env.App.Jobs {
		if job.Name == "rate-limit-cleanup" {
			found = true
			_, err := env.App.DB.Primary().Exec(t.Context(), `INSERT INTO rate_limit_window VALUES
				('watch_ip:60','old','2000-01-01','2000-01-02',1),
				('watch_ip:60','active','2100-01-01','2100-01-02',1)`)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			stopped := make(chan error, 1)
			go func() { stopped <- job.Run(ctx) }()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				var old int
				if err := env.App.DB.Primary().QueryRow(ctx, `SELECT count(*) FROM rate_limit_window WHERE subject='old'`).Scan(&old); err != nil {
					t.Fatal(err)
				}
				if old == 0 {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("cleanup did not run")
				case <-tick.C:
				}
			}
			var active int
			if err := env.App.DB.Primary().QueryRow(ctx, `SELECT count(*) FROM rate_limit_window WHERE subject='active'`).Scan(&active); err != nil || active != 1 {
				t.Fatalf("active window: %d %v", active, err)
			}
			cancel()
			select {
			case err := <-stopped:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("cleanup did not stop")
			}
		}
	}
	if !found {
		t.Fatal("cleanup worker not wired")
	}
}
