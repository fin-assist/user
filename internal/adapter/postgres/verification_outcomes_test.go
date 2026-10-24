package postgres

import (
	"bytes"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/verification"
)

func TestIntegrationCallbackOutcomeDetailsHaveIndependentLimit(t *testing.T) {
	const detailLimit = 1024 // The documented maximum number of detail groups per report.
	for _, tt := range []struct {
		name             string
		groups, outcomes int
	}{
		{"empty", 0, 1},
		{"below-limit", detailLimit - 1, 1},
		{"at-limit", detailLimit, 1},
		{"one-outcome-overflow", detailLimit + 2, 1},
		{"two-outcomes-overflow", detailLimit + 2, 2},
		{"three-outcomes-overflow", detailLimit + 2, 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			db, _, repo, now := phoneState(t)
			_, err := db.Primary().Exec(t.Context(), `INSERT INTO phone_verification
				(phone,token_hash,provider,provider_call_id,created_at,expires_at,verified_at,callback_number)
				SELECT '+7900'||lpad(g::text,7,'0'),decode(lpad(g::text,64,'0'),'hex'),
					'test','limit:'||g,$1::timestamptz,
					CASE WHEN (g-1)%$3=2 THEN $1::timestamptz-interval '1 hour'
						ELSE $1::timestamptz+interval '1 hour' END,
					CASE WHEN (g-1)%$3=1 THEN $1::timestamptz ELSE NULL END,
					'+7495'||lpad(g::text,7,'0')
				FROM generate_series(1,$2::int) AS g`, now, tt.groups, tt.outcomes)
			if err != nil {
				t.Fatal(err)
			}
			rows, totals, err := repo.PoolOutcomes(t.Context(), now.Add(-24*time.Hour))
			if err != nil || len(rows) != min(tt.groups, detailLimit) {
				t.Fatalf("details=%d, want=%d, error=%v", len(rows), min(tt.groups, detailLimit), err)
			}
			states := []string{"pending", "confirmed", "expired"}
			wantTotals := map[string]int64{}
			for i := range tt.groups {
				wantTotals[states[i%tt.outcomes]]++
			}
			for _, state := range states {
				if totals[state] != wantTotals[state] {
					t.Fatalf("%s total=%d, want=%d", state, totals[state], wantTotals[state])
				}
			}
			for i, row := range rows {
				if row.Number != fmt.Sprintf("+7495%07d", i+1) || row.State != states[i%tt.outcomes] || row.Count != 1 {
					t.Fatalf("detail[%d]=%+v", i, row)
				}
			}
			var logs bytes.Buffer
			reg := prometheus.NewRegistry()
			numbers, err := verification.NewNumbers(repo, slog.New(slog.NewTextHandler(&logs, nil)), reg)
			if err != nil {
				t.Fatal(err)
			}
			if err = numbers.Report(t.Context()); err != nil {
				t.Fatal(err)
			}
			if count := bytes.Count(logs.Bytes(), []byte("phone callback outcome (rolling 24h)")); count != len(rows) {
				t.Fatalf("detail log entries=%d, want=%d", count, len(rows))
			}
			families, err := reg.Gather()
			if err != nil {
				t.Fatal(err)
			}
			metricTotals := map[string]float64{}
			metricOther := map[string]float64{}
			var overflow float64
			for _, family := range families {
				for _, metric := range family.Metric {
					switch family.GetName() {
					case "finassist_phone_callback_outcomes_24h":
						var number, state string
						for _, label := range metric.Label {
							switch label.GetName() {
							case "callback_number":
								number = label.GetValue()
							case "outcome":
								state = label.GetValue()
							}
						}
						metricTotals[state] += metric.GetGauge().GetValue()
						if number == "other" {
							metricOther[state] += metric.GetGauge().GetValue()
						}
					case "finassist_phone_callback_overflow_requests_24h":
						overflow = metric.GetGauge().GetValue()
					}
				}
			}
			wantOther := map[string]int64{}
			for i := 256; i < tt.groups; i++ {
				wantOther[states[i%tt.outcomes]]++
			}
			for _, state := range states {
				if metricTotals[state] != float64(wantTotals[state]) || metricOther[state] != float64(wantOther[state]) {
					t.Fatalf("%s metrics total=%v other=%v, want total=%d other=%d", state, metricTotals[state], metricOther[state], wantTotals[state], wantOther[state])
				}
			}
			if overflow != float64(max(0, tt.groups-256)) {
				t.Fatalf("overflow=%v, requests=%d", overflow, tt.groups)
			}
		})
	}
}
