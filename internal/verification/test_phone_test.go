package verification

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestUnitProductionProofGuardRejectsBeforeEffectsAndRedacts(t *testing.T) {
	f := fixture(t)
	var logs bytes.Buffer
	reg := prometheus.NewRegistry()
	guard, err := NewProductionProofGuard(slog.New(slog.NewJSONHandler(&logs, nil)), reg)
	if err != nil {
		t.Fatal(err)
	}
	f.s.SetProofGuard(guard)
	for _, provider := range []string{"fake", "zvonok", "test"} {
		changed, err := f.s.Confirm(t.Context(), Proof{Provider: provider, CallID: "private-call", Phone: "+70000001234"})
		if changed || !errors.Is(err, ErrProof) || f.tx.commits != 0 {
			t.Fatal("production confirmation reached effects", changed, err)
		}
	}
	metrics, err := reg.Gather()
	if err != nil || len(metrics) != 1 || metrics[0].Metric[0].GetCounter().GetValue() != 3 || len(metrics[0].Metric[0].Label) != 0 {
		t.Fatal(metrics, err)
	}
	if strings.Contains(logs.String(), "+70000001234") || strings.Contains(logs.String(), "private-call") ||
		!strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Fatal("unsafe or missing security event", logs.String())
	}
	// The guard does not change ordinary provider-checked confirmation.
	changed, err := f.s.Confirm(t.Context(), goodProof())
	if !changed || err != nil {
		t.Fatal(changed, err)
	}
	if _, err := NewProductionProofGuard(nil, reg); err == nil {
		t.Fatal("nil logger accepted")
	}
	if _, err := NewProductionProofGuard(slog.New(slog.DiscardHandler), nil); err == nil {
		t.Fatal("nil registry accepted")
	}
	if _, err := NewProductionProofGuard(slog.New(slog.DiscardHandler), reg); err == nil {
		t.Fatal("duplicate collector accepted")
	}
}
