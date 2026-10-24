package verification

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// TestPhonePrefix identifies service-local synthetic phones +7000000xxxx.
// It is not an officially allocated test range and must never reach a real provider.
const TestPhonePrefix = "+7000000"

// IsTestPhone recognizes exactly the synthetic namespace, including four digits.
func IsTestPhone(phone string) bool {
	return validPhone(phone) && strings.HasPrefix(phone, TestPhonePrefix)
}

// ProofGuard checks local security policy before any confirmation effects.
type ProofGuard interface {
	Check(context.Context, Proof) error
}

// ProductionProofGuard rejects synthetic phones independently of the provider.
// Its counter and ERROR event describe attempts, including rolled-back requests.
type ProductionProofGuard struct {
	log      *slog.Logger
	rejected prometheus.Counter
}

// NewProductionProofGuard must be installed for every prod confirmation path.
func NewProductionProofGuard(log *slog.Logger, reg prometheus.Registerer) (*ProductionProofGuard, error) {
	if log == nil || reg == nil {
		return nil, errors.New("verification: production guard dependencies required")
	}
	counter := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "finassist_phone_test_confirmation_rejected_total",
		Help: "Synthetic-phone confirmation attempts rejected in prod; no private labels.",
	})
	if err := reg.Register(counter); err != nil {
		return nil, err
	}
	return &ProductionProofGuard{log: log, rejected: counter}, nil
}

// Check never logs the caller phone, provider call ID or credentials.
func (g *ProductionProofGuard) Check(ctx context.Context, p Proof) error {
	if !IsTestPhone(p.Phone) {
		return nil
	}
	g.rejected.Inc()
	g.log.ErrorContext(ctx, "synthetic phone confirmation rejected in prod")
	return ErrProof
}
