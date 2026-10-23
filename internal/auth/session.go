// Package auth implements registration and refresh-session lifecycle. Its writes
// join the caller's transaction so an idempotency record commits with the effect.
package auth

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/fin-assist/contracts/gen/go/finerr"

	"github.com/fin-assist/user/internal/token"
)

// ErrMissing means the secret or its still-live owner no longer exists.
var ErrMissing = errors.New("auth: missing")

// Link is a primary snapshot of a refresh link and its session.
type Link struct {
	SessionID, Generation int32
	UserID                string
	VerificationID        *int32
	IssuedAt, ExpiresAt   time.Time
	RotatedAt, RevokedAt  *time.Time
}

// Registration is the verified new-user result that Register consumes once.
type Registration struct {
	ID         int32
	Phone      string
	VerifiedAt *time.Time
	IsNewUser  bool
	Closed     bool
}

// Repository provides only primary reads and transaction-bound mutations.
type Repository interface {
	FindLink(context.Context, token.Hash, time.Time) (Link, error)
	LockLink(context.Context, token.Hash, time.Time) (Link, error)
	Rotate(context.Context, token.Hash, token.Hash, Link, time.Time, time.Time) error
	Revoke(context.Context, Link, time.Time) error
	Logout(context.Context, token.Hash) error
	Registration(context.Context, token.Hash, bool) (Registration, error)
	CurrentPD(context.Context) (string, error)
	CreateUser(context.Context, string, string, time.Time) (string, bool, error)
	LockUser(context.Context, string) error
	GrantPD(context.Context, string, string, time.Time) error
	CreateSession(context.Context, string, int32, token.Hash, time.Time, time.Time) error
	CloseVerification(context.Context, int32, string, time.Time) error
}

// Transactions belongs to the service, including post-rollback reuse completion.
type Transactions interface {
	Run(context.Context, func(context.Context) error) error
}

// Guard serializes writes with deletion of the same user.
type Guard interface {
	Guard(context.Context, string) (bool, error)
}

// Signer issues the access JWT from the service's current signing key.
type Signer interface {
	Sign(context.Context, string) (string, error)
}

// Events publishes the initial timezone in the registration transaction.
type Events interface {
	TimezoneChanged(context.Context, string, string, int32) error
}

// Pair contains the secrets sent to the owner of a confirmed phone number.
type Pair struct{ Access, Refresh string }

// Rotation describes either a new pair or a reuse that must be durably revoked
// after the idempotency engine rolls its non-cacheable 401 transaction back.
type Rotation struct {
	Pair   Pair
	UserID string
	Reused bool
}

// RegisterInput contains the already-decoded registration request.
type RegisterInput struct {
	Token, ConsentVersion string
	Accepted              bool
	Timezone              *string
}

// Registered identifies the account and its newly-created session.
type Registered struct {
	Pair   Pair
	UserID string
}

// Service has no runtime goroutines; clocks and token generation are injected.
type Service struct {
	tx         Transactions
	repo       Repository
	guard      Guard
	signer     Signer
	events     Events
	now        func() time.Time
	newToken   func() (string, error)
	refreshTTL time.Duration
}

// New constructs the session lifecycle with a positive sliding refresh lifetime.
func New(tx Transactions, repo Repository, guard Guard, signer Signer, events Events, now func() time.Time, newToken func() (string, error), refreshTTL time.Duration) *Service {
	return &Service{tx: tx, repo: repo, guard: guard, signer: signer, events: events, now: now, newToken: newToken, refreshTTL: refreshTTL}
}

// Refresh locks the user guard before the link, then rotates exactly one live
// generation. Reused is returned only for a valid unexpired, previously-used link.
func (s *Service) Refresh(ctx context.Context, raw string) (out Rotation, err error) {
	err = s.tx.Run(ctx, func(ctx context.Context) error {
		now := s.now().UTC()
		hash := token.Sum(raw)
		l, err := s.lockLink(ctx, hash, now)
		if err != nil {
			return missingToken(err)
		}
		now = s.now().UTC()
		if !now.Before(l.ExpiresAt) {
			return finerr.InvalidToken()
		}
		if l.RevokedAt != nil {
			return finerr.TokenRevoked()
		}
		if l.RotatedAt != nil {
			out.Reused = true
			return finerr.TokenRevoked()
		}
		if l.Generation < 1 || l.Generation == math.MaxInt32 {
			return errors.New("auth: invalid refresh generation")
		}
		if now.Before(l.IssuedAt) {
			now = l.IssuedAt
		}
		pair, err := s.pair(ctx, l.UserID)
		if err != nil {
			return err
		}
		if err := s.repo.Rotate(ctx, hash, token.Sum(pair.Refresh), l, now, now.Add(s.refreshTTL)); err != nil {
			return missingToken(err)
		}
		if l.VerificationID != nil {
			if err := s.repo.CloseVerification(ctx, *l.VerificationID, l.UserID, now); err != nil {
				return missingToken(err)
			}
		}
		out.Pair, out.UserID = pair, l.UserID
		return nil
	})
	return out, err
}

// CompleteReuse rechecks the old secret in a new transaction and commits the
// session revocation before the handler returns TOKEN_REVOKED. It is repeatable
// and does not revoke an unrelated or deleted session.
func (s *Service) CompleteReuse(ctx context.Context, raw string) error {
	return s.tx.Run(ctx, func(ctx context.Context) error {
		now := s.now().UTC()
		l, err := s.lockLink(ctx, token.Sum(raw), now)
		if errors.Is(err, ErrMissing) {
			return nil
		}
		if err != nil {
			return err
		}
		if l.RevokedAt != nil || l.RotatedAt == nil {
			return nil
		}
		if now.Before(*l.RotatedAt) {
			now = *l.RotatedAt
		}
		return s.repo.Revoke(ctx, l, now)
	})
}

func (s *Service) lockLink(ctx context.Context, hash token.Hash, now time.Time) (Link, error) {
	l, err := s.repo.FindLink(ctx, hash, now)
	if err != nil {
		return Link{}, err
	}
	alive, err := s.guard.Guard(ctx, l.UserID)
	if err != nil {
		return Link{}, err
	}
	if !alive {
		return Link{}, finerr.TokenRevoked()
	}
	l, err = s.repo.LockLink(ctx, hash, now)
	return l, err
}

func missingToken(err error) error {
	if errors.Is(err, ErrMissing) {
		return finerr.InvalidToken()
	}
	return err
}

func (s *Service) pair(ctx context.Context, id string) (Pair, error) {
	refresh, err := s.newToken()
	if err != nil {
		return Pair{}, err
	}
	access, err := s.signer.Sign(ctx, id)
	if err != nil {
		return Pair{}, err
	}
	return Pair{Access: access, Refresh: refresh}, nil
}

// Logout deletes the entire chain for this secret. Unknown and repeated secrets
// are successful; access JWTs retain their original expiry.
func (s *Service) Logout(ctx context.Context, raw string) error {
	return s.tx.Run(ctx, func(ctx context.Context) error { return s.repo.Logout(ctx, token.Sum(raw)) })
}
