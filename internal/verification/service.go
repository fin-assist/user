// Package verification confirms provider-checked phone calls and re-delivers their results.
package verification

import (
	"context"
	"crypto/subtle"
	"errors"
	"regexp"
	"time"

	"github.com/fin-assist/contracts/gen/go/finerr"

	"github.com/fin-assist/user/internal/auth"
	"github.com/fin-assist/user/internal/token"
)

var (
	ErrMissing = errors.New("verification: request missing")
	ErrProof   = errors.New("verification: provider proof does not match")
	errChanged = errors.New("verification: owner changed during read")
)

const ResultTTL = 15 * time.Minute

type State uint8

const (
	Pending State = iota + 1
	Verified
	Expired
)

// Outcome contains only the result allowed to leave through the verification secret.
type Outcome struct {
	State             State
	IsNewUser         bool
	UserID            string
	Pair              auth.Pair
	RegistrationToken string
}

// Request is primary state; nullable fields preserve the request lifecycle.
type Request struct {
	ID                      int32
	Phone, Provider, CallID string
	Hash                    token.Hash
	CreatedAt, ExpiresAt    time.Time
	VerifiedAt              *time.Time
	IsNewUser               bool
	UserID                  string
	RegistrationHash        *token.Hash
	Ciphertext              []byte
	KeyID                   string
	Closed                  bool
	Released                bool
}

// Proof must be obtained from the provider API by a trusted adapter, never from an
// unverified webhook or an end-user RPC. The service checks identity and local expiry.
type Proof struct{ Provider, CallID, Phone string }

// Repository reads primary and mutates only within the caller's transaction.
type Repository interface {
	ByToken(context.Context, token.Hash) (Request, error)
	ByCall(context.Context, string, string) (Request, error)
	Lock(context.Context, int32, bool) (Request, error)
	Verify(context.Context, Request, time.Time, Outcome, *token.Hash, []byte, string) error
	CleanupResults(context.Context, time.Time, int) (int64, error)
	DetachOldRequests(context.Context, time.Time, int) (int64, error)
	DeleteOldRequests(context.Context, time.Time, int) (int64, error)
}
type Transactions interface {
	Run(context.Context, func(context.Context) error) error
}
type Guard interface {
	Guard(context.Context, string) (bool, error)
}

// Accounts shares auth's session writer, so no second refresh-chain implementation exists.
type Accounts interface {
	FindUserByPhone(context.Context, string) (string, error)
	LockUser(context.Context, string) error
	CreateSession(context.Context, string, int32, token.Hash, time.Time, time.Time) error
}

// Codec binds authenticated ciphertext to both the request ID and its secret hash.
type Codec interface {
	Seal(Request, Outcome) ([]byte, string, error)
	Open(Request) (Outcome, error)
}

// Subscription coalesces notifications; a listener failure closes Done and must
// terminate the stream rather than silently pretending the request is pending.
type Subscription struct {
	Changed <-chan struct{}
	Done    <-chan struct{}
	Close   func()
}
type Notifications interface {
	Subscribe(int32) (Subscription, error)
}

type Service struct {
	tx                 Transactions
	repo               Repository
	accounts           Accounts
	guard              Guard
	signer             auth.Signer
	codec              Codec
	notifications      Notifications
	now                func() time.Time
	newToken           func() (string, error)
	refreshTTL, resync time.Duration
	checker            StatusChecker
	proofGuard         ProofGuard
}

// SetProofGuard installs environment policy during assembly, before serving.
func (s *Service) SetProofGuard(guard ProofGuard) { s.proofGuard = guard }

// SetStatusChecker installs the provider fallback during assembly, before serving.
func (s *Service) SetStatusChecker(checker StatusChecker) { s.checker = checker }

func New(tx Transactions, repo Repository, accounts Accounts, guard Guard, signer auth.Signer, codec Codec, notifications Notifications, now func() time.Time, newToken func() (string, error), refreshTTL, resync time.Duration) *Service {
	return &Service{tx: tx, repo: repo, accounts: accounts, guard: guard, signer: signer, codec: codec, notifications: notifications, now: now, newToken: newToken, refreshTTL: refreshTTL, resync: resync}
}

var providerName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// Confirm atomically stores one immutable result, a session for an existing user,
// and NOTIFY. Repeated/late proof issues no new secrets. No provider RPC runs here.
func (s *Service) Confirm(ctx context.Context, p Proof) (changed bool, err error) {
	return s.confirm(ctx, p, nil)
}

// ConfirmCreated joins Start's completion transaction and shares Confirm's
// entire result/notification path. The trusted caller must resolve owner and
// hold Guard -> user BEFORE completing/locking the request. Empty owner is the
// pre-lock snapshot; a concurrent registration is handled by ordinary Register.
// Re-discovering an account here could take Guard while holding the request.
func (s *Service) ConfirmCreated(ctx context.Context, p Proof, owner string) (bool, error) {
	return s.confirm(ctx, p, &owner)
}

func (s *Service) confirm(ctx context.Context, p Proof, owner *string) (changed bool, err error) {
	if s.proofGuard != nil {
		if err := s.proofGuard.Check(ctx, p); err != nil {
			return false, err
		}
	}
	if !providerName.MatchString(p.Provider) || len(p.CallID) < 1 || len(p.CallID) > 128 || !validPhone(p.Phone) {
		return false, ErrProof
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err = s.tx.Run(ctx, func(ctx context.Context) error {
		before, err := s.repo.ByCall(ctx, p.Provider, p.CallID)
		if err != nil {
			return err
		}
		if before.Phone != p.Phone {
			return ErrProof
		}
		// Learn the user before taking a request lock: all existing-user writers use
		// Guard -> app_user -> verification, including registration and deletion.
		var id string
		if owner != nil {
			id = *owner
		} else {
			id, err = s.accounts.FindUserByPhone(ctx, p.Phone)
			if err != nil {
				return err
			}
			if id != "" {
				alive, err := s.guard.Guard(ctx, id)
				if err != nil {
					return err
				}
				if !alive {
					return nil
				}
				if err = s.accounts.LockUser(ctx, id); err != nil {
					return err
				}
			}
		}
		r, err := s.repo.Lock(ctx, before.ID, true)
		if errors.Is(err, ErrMissing) {
			return nil
		}
		if err != nil {
			return err
		}
		if r.Phone != p.Phone || r.Provider != p.Provider || r.CallID != p.CallID {
			return ErrProof
		}
		now := s.now().UTC()
		if r.VerifiedAt != nil || r.Closed || r.Released || !now.Before(r.ExpiresAt) {
			return nil
		}
		if now.Before(r.CreatedAt) {
			return errors.New("verification: request created in the future")
		}
		out := Outcome{State: Verified, IsNewUser: id == "", UserID: id}
		var registrationHash *token.Hash
		if out.IsNewUser {
			out.RegistrationToken, err = s.newToken()
			if err != nil {
				return err
			}
			h := token.Sum(out.RegistrationToken)
			registrationHash = &h
		} else {
			out.Pair, err = auth.IssuePair(ctx, id, s.signer, s.newToken)
			if err != nil {
				return err
			}
			if err = s.accounts.CreateSession(ctx, id, r.ID, token.Sum(out.Pair.Refresh), now, now.Add(s.refreshTTL)); err != nil {
				return err
			}
		}
		ciphertext, key, err := s.codec.Seal(r, out)
		if err != nil {
			return err
		}
		if err = s.repo.Verify(ctx, r, now, out, registrationHash, ciphertext, key); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// Snapshot authenticates the secret and serializes delivery with consumption and
// deletion. Reading pending at/after expiry before opening a stream is INVALID_TOKEN.
func (s *Service) Snapshot(ctx context.Context, raw string) (Request, Outcome, error) {
	return s.snapshot(ctx, token.Sum(raw), false)
}
func (s *Service) snapshot(ctx context.Context, hash token.Hash, allowExpired bool) (r Request, out Outcome, err error) {
	for range 3 {
		err = s.tx.Run(ctx, func(ctx context.Context) error {
			before, err := s.repo.ByToken(ctx, hash)
			if err != nil {
				return invalidMissing(err)
			}
			if before.UserID != "" {
				alive, err := s.guard.Guard(ctx, before.UserID)
				if err != nil {
					return err
				}
				if !alive {
					return finerr.InvalidToken()
				}
			}
			r, err = s.repo.Lock(ctx, before.ID, false)
			if err != nil {
				return invalidMissing(err)
			}
			if subtle.ConstantTimeCompare(r.Hash[:], hash[:]) != 1 || r.Closed {
				return finerr.InvalidToken()
			}
			if r.UserID != before.UserID {
				return errChanged
			}
			now := s.now().UTC()
			if r.VerifiedAt == nil {
				if r.Released || !now.Before(r.ExpiresAt) {
					if !allowExpired {
						return finerr.InvalidToken()
					}
					out = Outcome{State: Expired}
				} else {
					out = Outcome{State: Pending}
				}
				return nil
			}
			if !now.Before(r.VerifiedAt.Add(ResultTTL)) || len(r.Ciphertext) == 0 || r.KeyID == "" {
				return finerr.InvalidToken()
			}
			out, err = s.codec.Open(r)
			if err != nil {
				return err
			}
			if err = validateResult(r, out); err != nil {
				return err
			}
			return nil
		})
		if !errors.Is(err, errChanged) {
			return r, out, err
		}
	}
	return Request{}, Outcome{}, errors.New("verification: unstable request owner")
}
func validateResult(r Request, o Outcome) error {
	if o.State != Verified || o.IsNewUser != r.IsNewUser || o.UserID != r.UserID {
		return errors.New("verification: inconsistent encrypted result")
	}
	if o.IsNewUser {
		if r.UserID != "" || r.RegistrationHash == nil || o.RegistrationToken == "" || o.Pair.Access != "" || o.Pair.Refresh != "" {
			return errors.New("verification: inconsistent registration result")
		}
		h := token.Sum(o.RegistrationToken)
		if subtle.ConstantTimeCompare(h[:], r.RegistrationHash[:]) != 1 {
			return errors.New("verification: registration result mismatch")
		}
	} else if r.UserID == "" || o.Pair.Access == "" || o.Pair.Refresh == "" || o.RegistrationToken != "" || r.RegistrationHash != nil {
		return errors.New("verification: inconsistent session result")
	}
	return nil
}
func invalidMissing(err error) error {
	if errors.Is(err, ErrMissing) {
		return finerr.InvalidToken()
	}
	return err
}
func validPhone(s string) bool {
	if len(s) != 12 || s[:2] != "+7" {
		return false
	}
	for _, c := range s[2:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Watch subscribes before its second snapshot, closing the read/LISTEN race. Only
// pending streams receive the expires_at+10s ceiling; verified results can replay
// throughout their independent 15-minute window, even after request expiry.
func (s *Service) Watch(ctx context.Context, raw string, send func(Outcome) error) error {
	r, out, err := s.Snapshot(ctx, raw)
	if err != nil {
		return err
	}
	if out.State != Pending {
		return send(out)
	}
	sub, err := s.notifications.Subscribe(r.ID)
	if err != nil {
		return err
	}
	defer sub.Close()
	ctx, cancel := context.WithTimeout(ctx, r.ExpiresAt.Add(10*time.Second).Sub(s.now()))
	defer cancel()
	hash := token.Sum(raw)
	r, out, err = s.snapshot(ctx, hash, true)
	if err != nil {
		return err
	}
	if err = send(out); err != nil {
		return err
	}
	if out.State != Pending {
		return nil
	}
	remaining := r.ExpiresAt.Sub(s.now())
	if remaining < 0 {
		remaining = 0
	}
	expiry := time.NewTimer(remaining)
	defer expiry.Stop()
	poll := time.NewTicker(s.resync)
	defer poll.Stop()
	for {
		final := false
		checkProvider := false
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-sub.Done:
			return errors.New("verification: notification listener unavailable")
		case <-sub.Changed:
		case <-expiry.C:
			final = true
			checkProvider = true
		case <-poll.C:
			checkProvider = true
		}
		// Deliver already committed proof without another provider round trip,
		// including when the resync timer wins the race with its NOTIFY.
		r, out, err = s.snapshot(ctx, hash, true)
		if err != nil {
			return err
		}
		if out.State == Verified {
			return send(out)
		}
		if out.State == Expired {
			final, checkProvider = true, true
		}
		if s.checker != nil && checkProvider {
			// Provider errors must not turn a healthy pending stream into a failed
			// login. NOTIFY and the local expiry still determine client delivery.
			_ = s.checker.Check(ctx, r, final)
			_, out, err = s.snapshot(ctx, hash, true)
			if err != nil {
				return err
			}
		}
		if out.State != Pending {
			return send(out)
		}
	}
}

// Cleanup requires a context without an outer transaction; each stage commits separately.
func (s *Service) Cleanup(ctx context.Context, batch int) (n int64, err error) {
	if batch < 1 || batch > 1000 {
		return 0, errors.New("verification: invalid cleanup batch")
	}
	// Never retain request-row locks while waiting for session rows, or vice versa:
	// Refresh closes a request after locking its session; deletion has cascades.
	for _, stage := range []func(context.Context, time.Time, int) (int64, error){s.repo.CleanupResults, s.repo.DetachOldRequests, s.repo.DeleteOldRequests} {
		var count int64
		if err = s.tx.Run(ctx, func(ctx context.Context) error { var e error; count, e = stage(ctx, s.now().UTC(), batch); return e }); err != nil {
			return n, err
		}
		n += count
	}
	return n, nil
}
