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
	"unicode/utf8"

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
	Key                      string
	Hash                     [32]byte
	Request                  Request
	Windows                  []ratelimit.Window
	Consent                  Consent
	ClientIP                 string
	AcceptedAt, KeyExpiresAt time.Time
}

// Consent is the separate pre-transfer declaration, pinned to its original receipt.
type Consent struct {
	Accepted     bool
	Version      string
	AgeConfirmed bool
}

// Starts mutates workflow state only in the caller's primary transaction.
type Starts interface {
	Find(context.Context, string) (StartRecord, error)
	Busy(context.Context, string, time.Time) (int32, time.Time, error)
	CurrentPD(context.Context) (string, error)
	Dispatch(context.Context, StartRecord, time.Time) (StartRecord, bool, error)
	Terminate(context.Context, StartRecord, time.Time) error
	FinalizeKey(context.Context, StartRecord) error
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
	tx             Transactions
	repo           Starts
	engine         StartEngine
	limits         StartLimits
	accounts       Accounts
	guard          Guard
	provider       Provider
	selector       NumberSelector
	now            func() time.Time
	newToken       func() (string, error)
	log            *slog.Logger
	duplicates     prometheus.Counter
	immediate      CreationConfirmation
	unknownTimeout time.Duration
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
	return &Starter{tx: tx, repo: repo, engine: engine, limits: limits, accounts: accounts, guard: guard, provider: provider, selector: selector, now: now, newToken: newToken, log: log, duplicates: c, unknownTimeout: RequestTTL}, nil
}

// Start accepts a library-extracted key, whole-input hash and a trusted client IP.
// The durable record authorizes recovery only for its original key and body.
func (s *Starter) Start(ctx context.Context, key idempotency.Request, phone, ip string) (Started, error) {
	return s.StartWithConsent(ctx, key, phone, ip, Consent{})
}

// SetUnknownTimeout configures new attempts only; persisted deadlines are immutable.
func (s *Starter) SetUnknownTimeout(d time.Duration) error {
	if d < time.Second || d > RequestTTL {
		return errors.New("verification: invalid unknown timeout")
	}
	s.unknownTimeout = d
	return nil
}

// StartWithConsent preserves key precedence and commits evidence before dispatch.
func (s *Starter) StartWithConsent(ctx context.Context, key idempotency.Request, phone, ip string, consent Consent) (Started, error) {
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
		record, err = s.repo.Find(tx, key.Key)
		if err == nil {
			if record.Hash != key.Hash {
				return domain.WithRequestRetry(finerr.IdempotencyConflict(), false)
			}
			if record.Request.StartTerminal || record.Request.Closed || record.Request.Released || !record.Request.ExpiresAt.After(s.now()) {
				if err := s.repo.Terminate(tx, record, s.now().UTC()); err != nil {
					return err
				}
				result.Err = terminalStartError()
				return s.engine.Release(tx, reservation)
			}
			if record.Request.Provider != s.providerName() || record.Request.CallID != "" {
				return terminalStartError()
			}
			return s.engine.BindResource(tx, reservation, startResource(record.Request.ID))
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
		// Preserve Guard -> user -> request ordering even when Busy releases
		// an expired holder; otherwise concurrent account deletion can invert it.
		if _, err = s.owner(tx, phone); err != nil {
			return err
		}
		holder, busy, err := s.repo.Busy(tx, phone, s.now().UTC())
		if err != nil {
			return err
		}
		if !busy.IsZero() {
			result, err = s.engine.DoReserved(tx, reservation, func(context.Context) (idempotency.Response, error) {
				retry := ceilMinute(busy)
				return idempotency.Response{DuplicateRetryAt: retry, Resource: startResource(holder)}, finerr.RateLimited(retry)
			})
			return err
		}
		if gate := validateConsent(phone, consent); gate != nil {
			result, err = s.engine.DoReserved(tx, reservation, func(context.Context) (idempotency.Response, error) { return idempotency.Response{}, gate })
			return err
		}
		version, err := s.repo.CurrentPD(tx)
		if err != nil {
			return err
		}
		if version == "" {
			return ErrProviderUnavailable
		}
		if version != consent.Version {
			result, err = s.engine.DoReserved(tx, reservation, func(context.Context) (idempotency.Response, error) {
				return idempotency.Response{}, domain.WithRequestRetry(finerr.ConsentOutdated(), false)
			})
			return err
		}
		if s.provider == nil {
			return ErrProviderUnavailable
		}
		raw, err := s.newToken()
		if err != nil {
			return err
		}
		now := s.now().UTC()
		record = StartRecord{Key: key.Key, Hash: key.Hash, Consent: consent, ClientIP: ip, AcceptedAt: now, KeyExpiresAt: now.Add(24 * time.Hour), Windows: receipt.ReservedWindows(), Request: Request{Phone: phone, Provider: s.provider.Name(), Campaign: s.providerCampaign(), Evidence: true, Hash: token.Sum(raw), CreatedAt: now, ExpiresAt: now.Add(s.unknownTimeout), UnknownDeadline: now.Add(s.unknownTimeout)}}
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
	var firstDispatch bool
	err = s.tx.Run(completion, func(tx context.Context) error {
		var e error
		record, firstDispatch, e = s.repo.Dispatch(tx, record, s.now().UTC())
		return e
	})
	if err != nil {
		if errors.Is(err, ErrMissing) && !s.now().Before(record.Request.UnknownDeadline) {
			return Started{}, s.terminate(completion, record)
		}
		if errors.Is(err, ErrMissing) {
			return Started{}, terminalStartError()
		}
		// Storage/COMMIT uncertainty does not prove a terminal workflow state.
		// The same key can discover whether the marker committed before recovery.
		return Started{}, err
	}
	a := Attempt{ID: record.Request.ID, Phone: record.Request.Phone, Provider: record.Request.Provider, CallID: record.Request.CallID, Campaign: record.Request.Campaign, CreatedAt: record.AcceptedAt, ExpiresAt: record.Request.UnknownDeadline, FirstDispatchAt: record.Request.FirstDispatchAt, DispatchNotAfter: record.Request.DispatchNotAfter}
	// Reserve the last three seconds of the original completion budget for the
	// transaction that binds the call, selects a number and publishes the result.
	// Preflight and marker waits consume the network budget, never this reserve.
	network, cancelNetwork := context.WithDeadline(completion, deadline.Add(-3*time.Second))
	defer cancelNetwork()
	network, cancel := context.WithTimeout(network, a.DispatchNotAfter.Sub(s.now()))
	defer cancel()
	var call Call
	if firstDispatch {
		if !s.now().Before(a.DispatchNotAfter) {
			return Started{}, s.terminate(completion, record)
		}
		call, err = s.provider.Create(network, a)
	} else {
		call, err = s.provider.Recover(network, a)
	}
	if err != nil {
		if !s.now().Before(record.Request.UnknownDeadline) {
			return Started{}, s.terminate(completion, record)
		}
		if firstDispatch && errors.Is(err, ErrProviderRejected) {
			return Started{}, s.reject(completion, reservation, record)
		}
		if errors.Is(err, ErrProviderDuplicate) {
			return Started{}, s.duplicate(completion, reservation, record, requestedAt, err)
		}
		if errors.Is(err, ErrProviderTerminal) {
			return Started{}, s.terminate(completion, record)
		}
		if !s.now().Before(record.Request.UnknownDeadline) {
			return Started{}, s.terminate(completion, record)
		}
		return Started{}, domain.WithRequestRetry(errors.Join(ErrProviderUnavailable, finerr.InternalError()), true)
	}
	if err = validateCall(a, call, s.now()); err != nil {
		if !s.now().Before(record.Request.UnknownDeadline) {
			return Started{}, s.terminate(completion, record)
		}
		return Started{}, domain.WithRequestRetry(errors.Join(ErrProviderUnavailable, finerr.InternalError()), true)
	}
	var completionFailure error
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
				completionFailure = err
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
			body, err := json.Marshal(Started{raw, selected, call.PublicExpiry()})
			return idempotency.Response{Body: body, OwnerID: owner}, err
		})
		if err == nil {
			err = s.repo.FinalizeKey(tx, record)
		}
		return err
	})
	if err != nil {
		if completionFailure != nil {
			err = completionFailure
		}
		if errors.Is(err, ErrProviderDuplicate) {
			return Started{}, s.duplicate(completion, reservation, record, requestedAt, err)
		}
		if errors.Is(err, ErrMissing) {
			if !s.now().Before(record.Request.UnknownDeadline) {
				return Started{}, s.terminate(completion, record)
			}
			return Started{}, terminalStartError()
		}
		if errors.Is(err, ErrProviderTerminal) {
			return Started{}, s.terminate(completion, record)
		}
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
		if errors.Is(err, ErrMissing) {
			if !s.now().Before(record.Request.UnknownDeadline) {
				return s.terminate(ctx, record)
			}
			return terminalStartError()
		}
		return err
	}
	refund()
	return terminalStartError()
}

func (s *Starter) duplicate(ctx context.Context, reservation idempotency.Reservation, record StartRecord, requestedAt time.Time, cause error) error {
	if !s.now().Before(record.Request.UnknownDeadline) {
		return s.terminate(ctx, record)
	}
	var effectFailure error
	var result idempotency.Result
	busy := requestedAt.Add(RequestTTL)
	var duplicate *ProviderDuplicateError
	if errors.As(cause, &duplicate) {
		busy = duplicate.BusyUntil
	}
	if !busy.After(s.now()) {
		return s.terminate(ctx, record)
	}
	retry := ceilMinute(busy)
	record.Request.KnownBusyUntil = busy
	err := s.tx.Run(ctx, func(tx context.Context) error {
		var err error
		result, err = s.engine.DoReserved(tx, reservation, func(effect context.Context) (idempotency.Response, error) {
			if err := s.repo.Duplicate(effect, record, s.now().UTC()); err != nil {
				effectFailure = err
				return idempotency.Response{}, err
			}
			return idempotency.Response{DuplicateRetryAt: retry}, domain.WithRequestRetry(finerr.RateLimited(retry), false)
		})
		return err
	})
	if err != nil {
		if errors.Is(effectFailure, ErrMissing) {
			if !s.now().Before(record.Request.UnknownDeadline) {
				return s.terminate(ctx, record)
			}
			return terminalStartError()
		}
		if errors.Is(effectFailure, ErrProviderTerminal) {
			return s.terminate(ctx, record)
		}
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

func terminalStartError() error {
	return domain.WithRequestRetry(errors.Join(ErrProviderUnavailable, finerr.InternalError()), false)
}
func (s *Starter) terminate(ctx context.Context, r StartRecord) error {
	if err := s.tx.Run(ctx, func(tx context.Context) error { return s.repo.Terminate(tx, r, s.now().UTC()) }); err != nil {
		return err
	}
	return terminalStartError()
}
func (s *Starter) providerName() string {
	if s.provider == nil {
		return ""
	}
	return s.provider.Name()
}
func (s *Starter) providerCampaign() string {
	if p, ok := s.provider.(interface{ Campaign() string }); ok {
		return p.Campaign()
	}
	return ""
}
func validateConsent(phone string, c Consent) error {
	var fields []*errdetails.BadRequest_FieldViolation
	if !validStartPhone(phone) {
		fields = append(fields, &errdetails.BadRequest_FieldViolation{Field: "phone", Reason: "INVALID_FORMAT"})
	}
	if !c.AgeConfirmed {
		fields = append(fields, &errdetails.BadRequest_FieldViolation{Field: "age_confirmed", Reason: "REQUIRED"})
	}
	n := utf8.RuneCountInString(c.Version)
	if n < 1 || n > 64 || !utf8.ValidString(c.Version) {
		fields = append(fields, &errdetails.BadRequest_FieldViolation{Field: "pd_consent_version", Reason: "INVALID_FORMAT"})
	}
	if len(fields) > 0 {
		return domain.WithRequestRetry(finerr.ValidationError(fields...), false)
	}
	if !c.Accepted {
		return domain.WithRequestRetry(finerr.ConsentRequired(), false)
	}
	return nil
}
