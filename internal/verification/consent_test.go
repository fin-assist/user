package verification

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/user/internal/domain"
)

func TestUnitPreTransferConsentValidationAndCapturedTimeout(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Consent
		want string
	}{
		{"missing", Consent{}, "VALIDATION_ERROR"},
		{"no-age", Consent{Accepted: true, Version: "v1"}, "VALIDATION_ERROR"},
		{"no-version", Consent{Accepted: true, AgeConfirmed: true}, "VALIDATION_ERROR"},
		{"long-unicode", Consent{Accepted: true, AgeConfirmed: true, Version: strings.Repeat("я", 65)}, "VALIDATION_ERROR"},
		{"false-pd", Consent{AgeConfirmed: true, Version: "v1"}, "CONSENT_REQUIRED"},
		{"outdated", Consent{Accepted: true, AgeConfirmed: true, Version: "old"}, "CONSENT_OUTDATED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := startFixture(t)
			_, e := f.s.StartWithConsent(t.Context(), f.key, f.provider.call.Phone, "203.0.113.7", tc.c)
			if reason(e) != tc.want || f.provider.creates != 0 || f.repo.claimed != 0 {
				t.Fatal(reason(e), f.provider.creates, f.repo.claimed)
			}
		})
	}
	f := startFixture(t)
	for _, d := range []time.Duration{0, -time.Second, time.Second - time.Nanosecond, RequestTTL + time.Nanosecond} {
		if f.s.SetUnknownTimeout(d) == nil {
			t.Fatal("invalid timeout accepted", d)
		}
	}
	if e := f.s.SetUnknownTimeout(time.Second); e != nil {
		t.Fatal(e)
	}
	f.provider.createErr = ErrProviderUnavailable
	if _, e := f.run(t.Context()); e == nil {
		t.Fatal("unknown accepted")
	}
	first := *f.repo.record
	if !first.Request.UnknownDeadline.Equal(f.now.Add(time.Second)) {
		t.Fatal("unknown deadline not captured")
	}
	if e := f.s.SetUnknownTimeout(RequestTTL); e != nil {
		t.Fatal(e)
	}
	f.provider.recoverErr = ErrProviderUnavailable
	if _, e := f.run(t.Context()); e == nil {
		t.Fatal("unknown recovery accepted")
	}
	if !f.repo.record.Request.UnknownDeadline.Equal(first.Request.UnknownDeadline) || !f.repo.record.Request.FirstDispatchAt.Equal(first.Request.FirstDispatchAt) || f.provider.creates != 1 || f.provider.recovers != 1 {
		t.Fatal("recovery changed effect identity")
	}
	f.now = first.Request.UnknownDeadline
	_, e := f.run(t.Context())
	var advice *domain.RequestRetryError
	if !errors.As(e, &advice) || advice.RequestRetryPermission() || f.provider.creates != 1 || f.provider.recovers != 1 {
		t.Fatal("terminal retry allowed", e)
	}
}

func TestUnitTerminalProviderAndBusyOutcomes(t *testing.T) {
	f := startFixture(t)
	f.provider.createErr = ErrProviderTerminal
	if _, e := f.run(t.Context()); !errors.Is(e, ErrProviderUnavailable) {
		t.Fatal(e)
	}
	f = startFixture(t)
	f.provider.createErr = &ProviderDuplicateError{BusyUntil: f.now.Add(10 * time.Second)}
	_, e := f.run(t.Context())
	var advice *domain.RequestRetryError
	if !errors.As(e, &advice) || advice.RequestRetryPermission() || reason(e) != "RATE_LIMITED" {
		t.Fatal(e)
	}
	f = startFixture(t)
	f.provider.createErr = &ProviderDuplicateError{BusyUntil: f.now}
	if _, e = f.run(t.Context()); !errors.Is(e, ErrProviderUnavailable) || f.repo.duplicated != 0 {
		t.Fatal("past duplicate created fence", e)
	}
	f = startFixture(t)
	if _, e = f.s.Start(context.Background(), f.key, f.provider.call.Phone, "203.0.113.7"); reason(e) != "VALIDATION_ERROR" {
		t.Fatal("legacy entry bypass", e)
	}
}

func TestUnitLateProviderDuplicateCannotRefenceReleasedAttempt(t *testing.T) {
	f := startFixture(t)
	f.provider.create = func(context.Context, Attempt) (Call, error) {
		f.now = f.now.Add(RequestTTL)
		return Call{}, &ProviderDuplicateError{BusyUntil: f.now.Add(RequestTTL)}
	}
	if _, e := f.run(t.Context()); !errors.Is(e, ErrProviderUnavailable) || f.repo.duplicated != 0 || f.repo.terminated != 1 {
		t.Fatal("late duplicate was not terminal", e, f.repo.duplicated, f.repo.terminated)
	}
}

func TestUnitFinalAdmissionFailureCannotPublishProvisionalStart(t *testing.T) {
	for _, kind := range []string{"missing", "terminal", "public-expired"} {
		t.Run(kind, func(t *testing.T) {
			f := startFixture(t)
			switch kind {
			case "missing":
				f.repo.finalizeErr = ErrMissing
			case "terminal":
				f.repo.finalizeErr = ErrProviderTerminal
			case "public-expired":
				f.repo.finalizeErr = &ProviderDuplicateError{BusyUntil: f.now.Add(time.Minute)}
			}
			out, err := f.run(t.Context())
			if err == nil || out != (Started{}) || f.provider.creates != 1 || f.provider.recovers != 0 {
				t.Fatal("provisional result escaped final barrier", out, err)
			}
			var advice *domain.RequestRetryError
			if !errors.As(err, &advice) || advice.RequestRetryPermission() {
				t.Fatal("failed final admission allowed original-key retry", err)
			}
			if kind == "public-expired" {
				if reason(err) != "RATE_LIMITED" || f.repo.duplicated != 1 {
					t.Fatal("active busy bound was lost", err)
				}
			} else if !errors.Is(err, ErrProviderUnavailable) {
				t.Fatal(err)
			}
		})
	}
}
