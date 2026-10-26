package verification

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/fin-assist/contracts/gen/go/finerr"
	idempotency "github.com/fin-assist/go-idempotency"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/genproto/googleapis/rpc/errdetails"

	"github.com/fin-assist/user/internal/domain"
	"github.com/fin-assist/user/internal/ratelimit"
	"github.com/fin-assist/user/internal/token"
)

// Started is the encrypted replay body. Remaining time is computed by the transport.
type Started struct {
	Token, CallbackNumber string
	ExpiresAt             time.Time
}

// StartRecord is the request's own durable creation attempt. Busy keys are linked
// only through idempotency Resource metadata and never overwrite this identity.
// Windows contains four refundable slots while creation is unfinished.
type StartRecord struct {
	Key     string
	Hash    [32]byte
	Request Request
	Windows []ratelimit.Window
}

// Starts mutates workflow state only in the caller's primary transaction.
type Starts interface {
	Find(context.Context, string) (StartRecord, error)
	Forget(context.Context, string) error
	Claim(context.Context, StartRecord) (StartRecord, time.Time, error)
	Complete(context.Context, StartRecord, Call, token.Hash) error
	Reject(context.Context, StartRecord, time.Time) error
	Duplicate(context.Context, StartRecord, time.Time) error
}

// StartEngine is the shared library's external-effect protocol.
type StartEngine interface {
	BindResource(context.Context, idempotency.Reservation, idempotency.Resource) error
	Reserve(context.Context, idempotency.Request) (idempotency.Reservation, idempotency.Result, error)
	DoReserved(context.Context, idempotency.Reservation, func(context.Context) (idempotency.Response, error)) (idempotency.Result, error)
	Release(context.Context, idempotency.Reservation) error
	ReleaseIfCurrent(context.Context, idempotency.Reservation) (bool, error)
	WithCompletion(context.Context) (context.Context, context.CancelFunc)
}

// StartLimits reserves all four windows atomically and compensates original slots.
type StartLimits interface {
	Check(context.Context, ratelimit.Operation, string, string) (ratelimit.Decision, ratelimit.Receipt, error)
	Refund(context.Context, []ratelimit.Window) (func(), error)
}

// Starter coordinates creation without holding a database transaction over a call.
type Starter struct {
	tx         Transactions
	repo       Starts
	engine     StartEngine
	limits     StartLimits
	accounts   Accounts
	guard      Guard
	provider   Provider
	selector   NumberSelector
	now        func() time.Time
	newToken   func() (string, error)
	log        *slog.Logger
	duplicates prometheus.Counter
	immediate  CreationConfirmation
}

// CreationConfirmation reuses the account identity already resolved and locked
// by Start before it locks the request. Empty owner means no account existed then.
type CreationConfirmation interface {
	ConfirmCreated(context.Context, Proof, string) (bool, error)
}

// SetImmediateConfirmation enables an automatically verifying backend during
// trusted assembly, before serving. Confirm must join the creation transaction
// and perform only local effects; it must never contact an external provider.
func (s *Starter) SetImmediateConfirmation(confirm CreationConfirmation) { s.immediate = confirm }

// NewStarter accepts nil provider AND selector for the disabled runtime mode.
func NewStarter(tx Transactions, repo Starts, engine StartEngine, limits StartLimits, accounts Accounts, guard Guard, provider Provider, selector NumberSelector, now func() time.Time, newToken func() (string, error), log *slog.Logger, reg prometheus.Registerer) (*Starter, error) {
	if tx == nil || repo == nil || engine == nil || limits == nil || accounts == nil || guard == nil || now == nil || newToken == nil || log == nil || reg == nil || (provider == nil) != (selector == nil) {
		return nil, errors.New("verification: invalid start dependencies")
	}
	if provider != nil && !providerName.MatchString(provider.Name()) {
		return nil, errors.New("verification: invalid provider name")
	}
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "finassist_phone_verification_provider_duplicate_total", Help: "Provider duplicates while locally free; no private labels."})
	if err := reg.Register(c); err != nil {
		return nil, err
	}
	return &Starter{tx: tx, repo: repo, engine: engine, limits: limits, accounts: accounts, guard: guard, provider: provider, selector: selector, now: now, newToken: newToken, log: log, duplicates: c}, nil
}

// Start accepts a library-extracted key, whole-input hash and a trusted client IP.
// The durable record authorizes recovery only for its original key and body.
func (s *Starter) Start(ctx context.Context, key idempotency.Request, phone, ip string) (Started, error) {
	completion, done := s.engine.WithCompletion(ctx)
	defer done()
	completion, stop := context.WithTimeout(completion, 10*time.Second)
	defer stop()
	preflight, stopPreflight := context.WithCancel(completion)
	defer stopPreflight()
	watch := context.AfterFunc(ctx, stopPreflight)
	defer watch()
	if err := ctx.Err(); err != nil {
		return Started{}, err
	}
	requestedAt := s.now().UTC()
	var reservation idempotency.Reservation
	var result idempotency.Result
	var record StartRecord
	var receipt ratelimit.Receipt
	var fresh bool
	var forget bool
	err := s.tx.Run(preflight, func(tx context.Context) error {
		var err error
		reservation, result, err = s.engine.Reserve(tx, key)
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = preflight.Err(); err != nil {
			return err
		}
		// Once the key is acquired, local effects and their commit are bounded but
		// detached, as with the library's ordinary effect protocol.
		watch()
		if result.Replayed {
			if result.Err == nil {
				return s.checkReplay(tx, key, result)
			}
			return nil
		}
		if !validStartPhone(phone) {
			result, err = s.engine.DoReserved(tx, reservation, func(context.Context) (idempotency.Response, error) {
				return idempotency.Response{}, domain.WithRequestRetry(finerr.ValidationError(&errdetails.BadRequest_FieldViolation{Field: "phone", Reason: "INVALID_FORMAT"}), false)
			})
			return err
		}
		if s.provider == nil {
			return ErrProviderUnavailable
		}
		record, err = s.repo.Find(tx, key.Key)
		if err == nil {
			if len(record.Windows) == 4 && record.Request.ExpiresAt.After(s.now()) && !record.Request.Released {
				if record.Hash != key.Hash {
					return domain.WithRequestRetry(finerr.IdempotencyConflict(), false)
				}
				if record.Request.Provider != s.provider.Name() || record.Request.CallID != "" {
					return ErrProviderUnavailable
				}
				return s.engine.BindResource(tx, reservation, startResource(record.Request.ID))
			}
			// Clear only an expired/finished attempt identity; its request and
			// independently cached library resource links remain intact.
			forget = true
		} else if !errors.Is(err, ErrMissing) {
			return err
		}
		var decision ratelimit.Decision
		decision, receipt, err = s.limits.Check(tx, ratelimit.Start, phone, ip)
		if err != nil {
			return err
		}
		if !decision.Allowed {
			result.Err = domain.WithRequestRetry(finerr.RateLimited(decision.RetryAt), true)
			return s.engine.Release(tx, reservation)
		}
		if _, err = s.owner(tx, phone); err != nil {
			return err
		}
		if forget {
			if err = s.repo.Forget(tx, key.Key); err != nil {
				return err
			}
		}
		raw, err := s.newToken()
		if err != nil {
			return err
		}
		now := s.now().UTC()
		record = StartRecord{Key: key.Key, Hash: key.Hash, Windows: receipt.ReservedWindows(), Request: Request{Phone: phone, Provider: s.provider.Name(), Hash: token.Sum(raw), CreatedAt: now, ExpiresAt: now.Add(RequestTTL)}}
		var busy time.Time
		record, busy, err = s.repo.Claim(tx, record)
		if err != nil {
			return err
		}
		if !busy.IsZero() {
			retry := ceilMinute(busy)
			result, err = s.engine.DoReserved(tx, reservation, func(context.Context) (idempotency.Response, error) {
				return idempotency.Response{DuplicateRetryAt: retry, Resource: startResource(record.Request.ID)}, finerr.RateLimited(retry)
			})
			return err
		}
		fresh = true
		return s.engine.BindResource(tx, reservation, startResource(record.Request.ID))
	})
	if errors.Is(err, idempotency.ErrNotStored) {
		return Started{}, result.Err
	}
	if err != nil {
		return Started{}, err
	}
	receipt.Committed()
	if result.Replayed || result.Err != nil {
		return decodeStarted(result)
	}
	// The external effect has started after the committed reservation. Its final
	// transaction must survive caller cancellation, while retaining the original
	// ten-second completion bound and the library's completion context marker.
	finish, stopFinish := s.engine.WithCompletion(context.WithoutCancel(ctx))
	defer stopFinish()
	deadline, _ := completion.Deadline()
	finish, stopDeadline := context.WithDeadline(finish, deadline)
	defer stopDeadline()
	completion = finish
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_ = s.tx.Run(cleanup, func(tx context.Context) error { return s.engine.Release(tx, reservation) })
	}()
	a := Attempt{ID: record.Request.ID, Phone: record.Request.Phone, Provider: record.Request.Provider, CallID: record.Request.CallID, CreatedAt: record.Request.CreatedAt, ExpiresAt: record.Request.ExpiresAt}
	network, cancel := context.WithTimeout(completion, 7*time.Second)
	defer cancel()
	first, cancelFirst := context.WithTimeout(network, 3*time.Second)
	var call Call
	if fresh {
		call, err = s.provider.Create(first, a)
	} else {
		call, err = s.provider.Recover(first, a)
	}
	cancelFirst()
	if err != nil && !errors.Is(err, ErrProviderRejected) && !errors.Is(err, ErrProviderDuplicate) && network.Err() == nil {
		call, err = s.provider.Recover(network, a)
	}
	if err != nil {
		if errors.Is(err, ErrProviderRejected) {
			return Started{}, s.reject(completion, reservation, record)
		}
		if errors.Is(err, ErrProviderDuplicate) {
			return Started{}, s.duplicate(completion, reservation, record, requestedAt)
		}
		return Started{}, ErrProviderUnavailable
	}
	if err = validateCall(a, call, s.now()); err != nil {
		return Started{}, err
	}
	err = s.tx.Run(completion, func(tx context.Context) error {
		var err error
		result, err = s.engine.DoReserved(tx, reservation, func(effect context.Context) (idempotency.Response, error) {
			owner, err := s.owner(effect, phone)
			if err != nil {
				return idempotency.Response{}, err
			}
			raw, err := s.newToken()
			if err != nil {
				return idempotency.Response{}, err
			}
			if err = s.repo.Complete(effect, record, call, token.Sum(raw)); err != nil {
				return idempotency.Response{}, err
			}
			selected, err := s.selector.Select(effect, record.Request.ID, slices.Clone(call.Numbers))
			if err != nil {
				return idempotency.Response{}, err
			}
			if !slices.Contains(call.Numbers, selected) || !callbackNumber(selected) {
				return idempotency.Response{}, ErrProviderUnavailable
			}
			if s.immediate != nil {
				changed, err := s.immediate.ConfirmCreated(effect, Proof{Provider: s.provider.Name(), CallID: call.ID, Phone: phone}, owner)
				if err != nil {
					return idempotency.Response{}, err
				}
				if !changed {
					return idempotency.Response{}, ErrProviderUnavailable
				}
			}
			body, err := json.Marshal(Started{raw, selected, call.StartedAt.Add(RequestTTL)})
			return idempotency.Response{Body: body, OwnerID: owner}, err
		})
		return err
	})
	if err != nil {
		return Started{}, err
	}
	return decodeStarted(result)
}

func (s *Starter) owner(ctx context.Context, phone string) (string, error) {
	id, err := s.accounts.FindUserByPhone(ctx, phone)
	if err != nil || id == "" {
		return id, err
	}
	alive, err := s.guard.Guard(ctx, id)
	if err != nil {
		return "", err
	}
	if !alive {
		return "", ErrProviderUnavailable
	}
	if err = s.accounts.LockUser(ctx, id); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Starter) checkReplay(ctx context.Context, key idempotency.Request, result idempotency.Result) error {
	out, err := decodeStarted(result)
	if err != nil {
		return err
	}
	record, err := s.repo.Find(ctx, key.Key)
	if err != nil {
		return ErrProviderUnavailable
	}
	h := token.Sum(out.Token)
	if record.Hash != key.Hash || result.Response.Resource != startResource(record.Request.ID) || subtle.ConstantTimeCompare(h[:], record.Request.Hash[:]) != 1 {
		return ErrProviderUnavailable
	}
	owner, err := s.owner(ctx, record.Request.Phone)
	if err != nil {
		return err
	}
	if result.Response.OwnerID != "" && owner != result.Response.OwnerID {
		return ErrProviderUnavailable
	}
	return nil
}

func startResource(id int32) idempotency.Resource {
	return idempotency.Resource{Type: "phone_verification", ID: strconv.FormatInt(int64(id), 10)}
}

func (s *Starter) reject(ctx context.Context, reservation idempotency.Reservation, record StartRecord) error {
	var refund func()
	err := s.tx.Run(ctx, func(tx context.Context) error {
		released, err := s.engine.ReleaseIfCurrent(tx, reservation)
		if err != nil {
			return err
		}
		if !released {
			return finerr.RequestInProgress(time.Second)
		}
		refund, err = s.limits.Refund(tx, record.Windows)
		if err != nil {
			return err
		}
		return s.repo.Reject(tx, record, s.now().UTC())
	})
	if err != nil {
		return err
	}
	refund()
	return ErrProviderUnavailable
}

func (s *Starter) duplicate(ctx context.Context, reservation idempotency.Reservation, record StartRecord, requestedAt time.Time) error {
	var result idempotency.Result
	retry := ceilMinute(requestedAt.Add(RequestTTL))
	err := s.tx.Run(ctx, func(tx context.Context) error {
		var err error
		result, err = s.engine.DoReserved(tx, reservation, func(effect context.Context) (idempotency.Response, error) {
			if err := s.repo.Duplicate(effect, record, s.now().UTC()); err != nil {
				return idempotency.Response{}, err
			}
			return idempotency.Response{DuplicateRetryAt: retry}, finerr.RateLimited(retry)
		})
		return err
	})
	if err != nil {
		return err
	}
	s.duplicates.Inc()
	s.log.WarnContext(ctx, "phone verification provider duplicate")
	return result.Err
}

func decodeStarted(r idempotency.Result) (Started, error) {
	if r.Err != nil {
		return Started{}, r.Err
	}
	var out Started
	if err := json.Unmarshal(r.Response.Body, &out); err != nil || out.Token == "" || !callbackNumber(out.CallbackNumber) || out.ExpiresAt.IsZero() {
		return Started{}, ErrProviderUnavailable
	}
	return out, nil
}
func ceilMinute(t time.Time) time.Time {
	v := t.UTC().Truncate(time.Minute)
	if v.Before(t) {
		v = v.Add(time.Minute)
	}
	return v
}
func validStartPhone(s string) bool {
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
