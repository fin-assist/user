// Package profile owns profile settings and assistant-consent decisions.
package profile

import (
	"context"
	"encoding/hex"
	"errors"
	"math"
	"strings"
	"time"
	_ "time/tzdata" // Keep IANA validation available in the distroless image.

	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

// Consent describes the active grant and the current document version.
type Consent struct {
	Granted        bool
	Version        *string
	CurrentVersion string
	GrantedAt      *time.Time
}

// Record holds a profile snapshot. Service derives effective consent from its versions.
type Record struct {
	ID, Phone, Theme, Timezone string
	SettingsVersion            int32
	CreatedAt                  time.Time
	Consent                    Consent
}

// Patch changes only present settings.
type Patch struct{ Theme, Timezone *string }

// ErrConsentConflict means another active grant prevented a history insertion.
var ErrConsentConflict = errors.New("profile: active consent conflict")

// Transactions keeps profile effects and events in one service-owned transaction.
type Transactions interface {
	Run(context.Context, func(context.Context) error) error
}

// Repository supplies snapshots and user-scoped writes in the service transaction.
type Repository interface {
	Read(context.Context, string) (Record, error)
	Current(context.Context, string) (Record, error)
	Lock(context.Context, string) error
	Update(context.Context, string, string, string, int32, time.Time) error
	Grant(context.Context, string, string, time.Time) error
	Revoke(context.Context, string, time.Time) error
}

// Guard serializes a user operation with account deletion on primary.
type Guard interface {
	Guard(context.Context, string) (bool, error)
}

// Events writes a durable timezone event in the profile transaction.
type Events interface {
	TimezoneChanged(context.Context, string, string, int32) error
}

// Service implements profile settings and assistant-consent usecases.
type Service struct {
	tx     Transactions
	repo   Repository
	guard  Guard
	events Events
	now    func() time.Time
}

// New assembles explicit dependencies; it starts no background work.
func New(tx Transactions, repo Repository, guard Guard, events Events, now func() time.Time) *Service {
	return &Service{tx: tx, repo: repo, guard: guard, events: events, now: now}
}

// Get reads using the request's replica/primary hint. Admission checks the tombstone.
func (s *Service) Get(ctx context.Context, id string) (Record, error) {
	p, err := s.repo.Read(ctx, id)
	return checkedRecord(p, err)
}

// Update assigns present settings; a real timezone change advances the version and
// publishes an event in the same transaction. An identical retry creates no event.
func (s *Service) Update(ctx context.Context, id string, patch Patch) (out Record, err error) {
	if patch.Theme != nil && !validTheme(*patch.Theme) {
		return Record{}, invalid("theme")
	}
	if patch.Timezone != nil && !validTimezone(*patch.Timezone) {
		return Record{}, invalid("timezone")
	}
	err = s.tx.Run(ctx, func(ctx context.Context) error {
		p, err := s.writable(ctx, id)
		if err != nil {
			return err
		}
		theme, zone := p.Theme, p.Timezone
		if patch.Theme != nil {
			theme = *patch.Theme
		}
		if patch.Timezone != nil {
			zone = *patch.Timezone
		}
		if theme == p.Theme && zone == p.Timezone {
			out = p
			return nil
		}
		version := p.SettingsVersion
		if zone != p.Timezone {
			if version == math.MaxInt32 {
				return errors.New("profile: settings version exhausted")
			}
			version++
		}
		if err := s.repo.Update(ctx, id, theme, zone, version, s.now().UTC()); err != nil {
			return err
		}
		if zone != p.Timezone {
			if err := s.events.TimezoneChanged(ctx, id, zone, version); err != nil {
				return err
			}
		}
		out, err = s.current(ctx, id)
		return err
	})
	return out, err
}

// Grant records the current assistant-document version. Repeated grants preserve
// the original timestamp; a newer document replaces the active grant with history.
func (s *Service) Grant(ctx context.Context, id, version string) (out Record, err error) {
	err = s.tx.Run(ctx, func(ctx context.Context) error {
		p, err := s.writable(ctx, id)
		if err != nil {
			return err
		}
		if version != p.Consent.CurrentVersion {
			return finerr.ConsentOutdated()
		}
		if p.Consent.Granted {
			out = p
			return nil
		}
		now := s.now().UTC()
		if p.Consent.GrantedAt != nil && now.Before(*p.Consent.GrantedAt) {
			now = *p.Consent.GrantedAt
		}
		if err := s.repo.Revoke(ctx, id, now); err != nil {
			return err
		}
		if err := s.repo.Grant(ctx, id, version, now); err != nil {
			return err
		}
		out, err = s.current(ctx, id)
		return err
	})
	return out, err
}

// Revoke removes the active assistant grant without deleting its history.
func (s *Service) Revoke(ctx context.Context, id string) error {
	return s.tx.Run(ctx, func(ctx context.Context) error {
		p, err := s.writable(ctx, id)
		if err != nil {
			return err
		}
		if p.Consent.Version == nil {
			return nil
		}
		now := s.now().UTC()
		if p.Consent.GrantedAt != nil && now.Before(*p.Consent.GrantedAt) {
			now = *p.Consent.GrantedAt
		}
		return s.repo.Revoke(ctx, id, now)
	})
}

// Status is the primary-only system check used by assistant. The guard serializes
// it with consent changes and deletion, even when a request supplied replica hints.
func (s *Service) Status(ctx context.Context, id string) (out Consent, err error) {
	if !validID(id) {
		return Consent{}, invalid("user_id")
	}
	err = s.tx.Run(ctx, func(ctx context.Context) error {
		alive, err := s.guard.Guard(ctx, id)
		if err != nil {
			return err
		}
		if !alive {
			return finerr.TokenRevoked()
		}
		p, err := s.current(ctx, id)
		if err != nil {
			return err
		}
		out = p.Consent
		return nil
	})
	return out, err
}

func (s *Service) writable(ctx context.Context, id string) (Record, error) {
	alive, err := s.guard.Guard(ctx, id)
	if err != nil {
		return Record{}, err
	}
	if !alive {
		return Record{}, finerr.TokenRevoked()
	}
	if err := s.repo.Lock(ctx, id); err != nil {
		return Record{}, err
	}
	return s.current(ctx, id)
}

func (s *Service) current(ctx context.Context, id string) (Record, error) {
	p, err := s.repo.Current(ctx, id)
	return checkedRecord(p, err)
}

func checkedRecord(p Record, err error) (Record, error) {
	if err != nil {
		return Record{}, err
	}
	if err := validRecord(p); err != nil {
		return Record{}, err
	}
	p.Consent.Granted = p.Consent.Version != nil && *p.Consent.Version == p.Consent.CurrentVersion
	return p, nil
}

func validRecord(p Record) error {
	if !validTheme(p.Theme) || p.SettingsVersion < 1 || p.Consent.CurrentVersion == "" {
		return errors.New("profile: invalid stored profile or missing current document")
	}
	return nil
}

func validTheme(s string) bool { return s == "system" || s == "light" || s == "dark" }
func validTimezone(s string) bool {
	if s == "" || s == "Local" || len(s) > 64 {
		return false
	}
	_, err := time.LoadLocation(s)
	return err == nil
}
func validID(s string) bool {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return false
	}
	_, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	return err == nil
}
func invalid(field string) error {
	return finerr.ValidationError(&errdetails.BadRequest_FieldViolation{Field: field, Reason: "INVALID_FORMAT", Description: "Invalid value"})
}
