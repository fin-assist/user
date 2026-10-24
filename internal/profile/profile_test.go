package profile

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/fin-assist/contracts/gen/go/finerr"
)

const userID = "019c7714-3b77-74d1-9866-e1f484aae2ab"

var errDependency = errors.New("dependency failed")

type fixture struct {
	p                  Record
	now                time.Time
	fail               string
	alive              bool
	calls              []string
	readCount          int
	eventVersion       int32
	eventID, eventZone string
	revokedAt          time.Time
}

func newFixture() *fixture {
	return &fixture{p: Record{ID: userID, Phone: "+79001234567", Theme: "system", Timezone: "Europe/Moscow", SettingsVersion: 1, Consent: Consent{CurrentVersion: "v2"}}, now: time.Date(2026, 10, 23, 11, 0, 0, 0, time.UTC), alive: true}
}
func (f *fixture) called(name string) error {
	f.calls = append(f.calls, name)
	if f.fail == name {
		return errDependency
	}
	return nil
}
func (f *fixture) Run(ctx context.Context, fn func(context.Context) error) error {
	if err := f.called("begin"); err != nil {
		return err
	}
	if err := fn(ctx); err != nil {
		return err
	}
	return f.called("commit")
}
func (f *fixture) Guard(context.Context, string) (bool, error)  { return f.alive, f.called("guard") }
func (f *fixture) Read(context.Context, string) (Record, error) { return f.p, f.called("read") }
func (f *fixture) Current(context.Context, string) (Record, error) {
	f.readCount++
	return f.p, f.called("current" + string(rune('0'+f.readCount)))
}
func (f *fixture) Lock(context.Context, string) error { return f.called("lock") }
func (f *fixture) Update(_ context.Context, id, theme, zone string, version int32, _ time.Time) error {
	if id != userID {
		return errDependency
	}
	if err := f.called("update"); err != nil {
		return err
	}
	f.p.Theme, f.p.Timezone, f.p.SettingsVersion = theme, zone, version
	return nil
}
func (f *fixture) Grant(_ context.Context, id, version string, now time.Time) error {
	if id != userID {
		return errDependency
	}
	if err := f.called("grant"); err != nil {
		return err
	}
	f.p.Consent = Consent{Granted: true, Version: &version, CurrentVersion: f.p.Consent.CurrentVersion, GrantedAt: &now}
	return nil
}
func (f *fixture) Revoke(_ context.Context, id string, now time.Time) error {
	if id != userID {
		return errDependency
	}
	f.revokedAt = now
	if err := f.called("revoke"); err != nil {
		return err
	}
	f.p.Consent = Consent{CurrentVersion: f.p.Consent.CurrentVersion}
	return nil
}
func (f *fixture) TimezoneChanged(_ context.Context, id, zone string, version int32) error {
	f.eventID, f.eventZone, f.eventVersion = id, zone, version
	return f.called("event")
}
func (f *fixture) service() *Service { return New(f, f, f, f, func() time.Time { return f.now }) }
func ptr[T any](v T) *T              { return &v }
func reason(err error) string {
	rest, ok := finerr.ToREST(err)
	if !ok {
		return ""
	}
	return rest.Code
}

func TestProfileUpdateAndRetry(t *testing.T) {
	for _, tt := range []struct {
		name        string
		patch       Patch
		theme, zone string
		version     int32
		event       bool
	}{
		{"empty", Patch{}, "system", "Europe/Moscow", 1, false},
		{"same", Patch{Theme: ptr("system"), Timezone: ptr("Europe/Moscow")}, "system", "Europe/Moscow", 1, false},
		{"theme", Patch{Theme: ptr("dark")}, "dark", "Europe/Moscow", 1, false},
		{"zone", Patch{Timezone: ptr("Asia/Tokyo")}, "system", "Asia/Tokyo", 2, true},
		{"both", Patch{Theme: ptr("light"), Timezone: ptr("UTC")}, "light", "UTC", 2, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture()
			p, err := f.service().Update(t.Context(), userID, tt.patch)
			if err != nil || p.Theme != tt.theme || p.Timezone != tt.zone || p.SettingsVersion != tt.version {
				t.Fatalf("%+v %v", p, err)
			}
			if (f.eventVersion != 0) != tt.event {
				t.Fatal(f.calls)
			}
			if tt.event && (f.eventID != userID || f.eventZone != tt.zone || f.eventVersion != tt.version) {
				t.Fatal("incomplete event")
			}
			f.eventVersion = 0
			f.readCount = 0
			if _, err := f.service().Update(t.Context(), userID, tt.patch); err != nil || f.eventVersion != 0 {
				t.Fatalf("retry: %v", err)
			}
		})
	}
}

func TestProfileValidationAndVersionBoundary(t *testing.T) {
	for _, patch := range []Patch{{Theme: ptr("")}, {Theme: ptr("unknown")}, {Timezone: ptr("")}, {Timezone: ptr("Local")}, {Timezone: ptr("Etc/Unknown")}, {Timezone: ptr(strings.Repeat("A", 65))}, {Timezone: ptr("../UTC")}, {Timezone: ptr("/etc/passwd")}} {
		f := newFixture()
		if _, err := f.service().Update(t.Context(), userID, patch); reason(err) != "VALIDATION_ERROR" || len(f.calls) != 0 {
			t.Fatalf("%+v %v", patch, err)
		}
	}
	f := newFixture()
	f.p.SettingsVersion = math.MaxInt32
	if _, err := f.service().Update(t.Context(), userID, Patch{Timezone: ptr("UTC")}); err == nil {
		t.Fatal("version overflow")
	}
	f.readCount = 0
	if p, err := f.service().Update(t.Context(), userID, Patch{Theme: ptr("dark")}); err != nil || p.SettingsVersion != math.MaxInt32 {
		t.Fatalf("theme at boundary: %+v %v", p, err)
	}
	for _, mutate := range []func(*fixture){func(f *fixture) { f.p.Theme = "bad" }, func(f *fixture) { f.p.SettingsVersion = 0 }, func(f *fixture) { f.p.Consent.CurrentVersion = "" }} {
		f := newFixture()
		mutate(f)
		if _, err := f.service().Get(t.Context(), userID); err == nil {
			t.Fatal("invalid stored profile exposed")
		}
	}
}

func TestConsentLifecycleAndPrimaryStatus(t *testing.T) {
	f := newFixture()
	s := f.service()
	p, err := s.Grant(t.Context(), userID, "v2")
	if err != nil || !p.Consent.Granted {
		t.Fatalf("grant: %+v %v", p, err)
	}
	grantedAt := *p.Consent.GrantedAt
	f.now = f.now.Add(time.Hour)
	f.readCount = 0
	p, err = s.Grant(t.Context(), userID, "v2")
	if err != nil || !p.Consent.GrantedAt.Equal(grantedAt) {
		t.Fatalf("repeated grant: %v", err)
	}
	f.readCount = 0
	if c, err := s.Status(t.Context(), userID); err != nil || !c.Granted {
		t.Fatalf("status: %+v %v", c, err)
	}
	for _, call := range f.calls {
		if call == "read" {
			t.Fatal("system status used replica")
		}
	}
	f.readCount = 0
	if err := s.Revoke(t.Context(), userID); err != nil {
		t.Fatal(err)
	}
	f.readCount = 0
	if err := s.Revoke(t.Context(), userID); err != nil {
		t.Fatal(err)
	}
	f.readCount = 0
	if c, err := s.Status(t.Context(), userID); err != nil || c.Granted || c.Version != nil {
		t.Fatalf("revoked status: %+v %v", c, err)
	}
	f.readCount = 0
	if _, err := s.Grant(t.Context(), userID, "old"); reason(err) != "CONSENT_OUTDATED" {
		t.Fatal(err)
	}
	f.readCount = 0
	if p, err := s.Grant(t.Context(), userID, "v2"); err != nil || !p.Consent.Granted {
		t.Fatalf("grant after revoke: %+v %v", p, err)
	}
}

func TestConsentOldVersionAndBackwardsClock(t *testing.T) {
	f := newFixture()
	old := "v1"
	future := f.now.Add(time.Hour)
	f.p.Consent.Version = &old
	f.p.Consent.GrantedAt = &future
	if _, err := f.service().Grant(t.Context(), userID, "v2"); err != nil || !f.revokedAt.Equal(future) || !f.p.Consent.GrantedAt.Equal(future) {
		t.Fatalf("replace old: %v %v", f.revokedAt, err)
	}
	f = newFixture()
	f.p.Consent.Version = &old
	f.p.Consent.GrantedAt = &future
	if err := f.service().Revoke(t.Context(), userID); err != nil || !f.revokedAt.Equal(future) {
		t.Fatalf("backwards clock: %v", err)
	}
}

func TestProfileDependencyErrorsAndDeletedUser(t *testing.T) {
	for _, point := range []string{"begin", "guard", "lock", "current1", "update", "event", "current2", "commit"} {
		f := newFixture()
		f.fail = point
		if _, err := f.service().Update(t.Context(), userID, Patch{Timezone: ptr("UTC")}); !errors.Is(err, errDependency) {
			t.Fatalf("%s: %v", point, err)
		}
	}
	for _, point := range []string{"begin", "guard", "lock", "current1", "revoke", "grant", "current2", "commit"} {
		f := newFixture()
		f.fail = point
		if _, err := f.service().Grant(t.Context(), userID, "v2"); !errors.Is(err, errDependency) {
			t.Fatalf("grant %s: %v", point, err)
		}
	}
	for _, point := range []string{"guard", "lock", "current1", "revoke"} {
		f := newFixture()
		f.p.Consent.Version = ptr("v2")
		f.fail = point
		if err := f.service().Revoke(t.Context(), userID); !errors.Is(err, errDependency) {
			t.Fatalf("revoke %s: %v", point, err)
		}
	}
	for _, point := range []string{"begin", "guard", "current1", "commit"} {
		f := newFixture()
		f.fail = point
		if _, err := f.service().Status(t.Context(), userID); !errors.Is(err, errDependency) {
			t.Fatalf("status %s: %v", point, err)
		}
	}
	f := newFixture()
	f.fail = "read"
	if _, err := f.service().Get(t.Context(), userID); !errors.Is(err, errDependency) {
		t.Fatal(err)
	}
	f = newFixture()
	f.alive = false
	if _, err := f.service().Update(t.Context(), userID, Patch{}); reason(err) != "TOKEN_REVOKED" {
		t.Fatal(err)
	}
	if _, err := f.service().Status(t.Context(), userID); reason(err) != "TOKEN_REVOKED" {
		t.Fatal(err)
	}
	f = newFixture()
	f.p.Consent.CurrentVersion = ""
	if _, err := f.service().Status(t.Context(), userID); err == nil {
		t.Fatal("status without document")
	}
	for _, id := range []string{"", "not-a-uuid", strings.Repeat("f", 36), "019c7714-3b77-74d1-9866-e1f484aae2az"} {
		f := newFixture()
		if _, err := f.service().Status(t.Context(), id); reason(err) != "VALIDATION_ERROR" || len(f.calls) != 0 {
			t.Fatalf("id %q: %v", id, err)
		}
	}
}

func (f *fixture) RunGrant(ctx context.Context, fn func(context.Context) error) error {
	return f.Run(ctx, fn)
}
