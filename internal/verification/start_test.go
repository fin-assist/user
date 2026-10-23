package verification

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	idempotency "github.com/fin-assist/go-idempotency"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/ratelimit"
	"github.com/fin-assist/user/internal/token"
)

type startEngineFake struct {
	replay                       *idempotency.Result
	reserveErr, finishErr        error
	current                      bool
	reserves, finishes, releases int
}

func (e *startEngineFake) WithCompletion(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}
func (e *startEngineFake) Reserve(context.Context, idempotency.Request) (idempotency.Reservation, idempotency.Result, error) {
	e.reserves++
	if e.replay != nil {
		r := *e.replay
		r.Replayed = true
		return idempotency.Reservation{}, r, e.reserveErr
	}
	return idempotency.Reservation{}, idempotency.Result{}, e.reserveErr
}
func (e *startEngineFake) DoReserved(ctx context.Context, _ idempotency.Reservation, fn func(context.Context) (idempotency.Response, error)) (idempotency.Result, error) {
	e.finishes++
	if e.finishErr != nil {
		return idempotency.Result{}, e.finishErr
	}
	out, err := fn(ctx)
	if err != nil {
		if !out.DuplicateRetryAt.IsZero() {
			return idempotency.Result{Response: out, Err: err}, nil
		}
		return idempotency.Result{}, err
	}
	r := idempotency.Result{Response: out}
	e.replay = &r
	return r, nil
}
func (e *startEngineFake) Release(context.Context, idempotency.Reservation) error {
	e.releases++
	return nil
}
func (e *startEngineFake) ReleaseIfCurrent(context.Context, idempotency.Reservation) (bool, error) {
	return e.current, e.finishErr
}

type startRepoFake struct {
	record                                           *StartRecord
	busy                                             time.Time
	failure                                          string
	claimed, completed, rejected, duplicated, forgot int
}

func (r *startRepoFake) Find(context.Context, string) (StartRecord, error) {
	if r.failure == "find" {
		return StartRecord{}, errDependency
	}
	if r.record == nil {
		return StartRecord{}, ErrMissing
	}
	return *r.record, nil
}
func (r *startRepoFake) Forget(context.Context, string) error {
	r.forgot++
	if r.failure == "forget" {
		return errDependency
	}
	r.record = nil
	return nil
}
func (r *startRepoFake) Claim(_ context.Context, v StartRecord) (StartRecord, time.Time, error) {
	r.claimed++
	if r.failure == "claim" {
		return v, time.Time{}, errDependency
	}
	v.Request.ID = 1
	r.record = &v
	return v, r.busy, nil
}
func (r *startRepoFake) Complete(_ context.Context, v StartRecord, c Call, h token.Hash) error {
	r.completed++
	if r.failure == "complete" {
		return errDependency
	}
	r.record = &v
	r.record.Request.Hash = h
	r.record.Request.CallID = c.ID
	r.record.Windows = nil
	return nil
}
func (r *startRepoFake) Reject(context.Context, StartRecord, time.Time) error {
	r.rejected++
	if r.failure == "reject" {
		return errDependency
	}
	r.record = nil
	return nil
}
func (r *startRepoFake) Duplicate(context.Context, StartRecord, time.Time) error {
	r.duplicated++
	if r.failure == "duplicate" {
		return errDependency
	}
	r.record.Windows = nil
	return nil
}

type startCounters struct {
	hits    []int
	err     error
	refunds int
}

func (c *startCounters) Lock(_ context.Context, w []ratelimit.Window) ([]int, error) {
	if c.hits == nil {
		c.hits = make([]int, len(w))
	}
	return slices.Clone(c.hits), c.err
}
func (c *startCounters) Increment(context.Context, []ratelimit.Window) error {
	for i := range c.hits {
		c.hits[i]++
	}
	return c.err
}
func (c *startCounters) Refund(context.Context, []ratelimit.Window) error { c.refunds++; return c.err }

type startProviderFake struct {
	tx                    *testTX
	name                  string
	call                  Call
	createErr, recoverErr error
	creates, recovers     int
	create                func(context.Context, Attempt) (Call, error)
}

func (p *startProviderFake) Name() string { return p.name }
func (p *startProviderFake) Create(ctx context.Context, a Attempt) (Call, error) {
	if p.tx.active {
		panic("provider called inside transaction")
	}
	p.creates++
	if p.create != nil {
		return p.create(ctx, a)
	}
	return p.call, p.createErr
}
func (p *startProviderFake) Recover(context.Context, Attempt) (Call, error) {
	if p.tx.active {
		panic("recovery called inside transaction")
	}
	p.recovers++
	return p.call, p.recoverErr
}

type startSelectorFake struct {
	tx    *testTX
	out   string
	err   error
	calls int
}

func (s *startSelectorFake) Select(_ context.Context, _ int32, n []string) (string, error) {
	if !s.tx.active {
		panic("selector outside transaction")
	}
	s.calls++
	if s.err != nil {
		return "", s.err
	}
	if s.out != "" {
		return s.out, nil
	}
	return n[0], nil
}

type startFixtureData struct {
	s        *Starter
	tx       *testTX
	repo     *startRepoFake
	engine   *startEngineFake
	counters *startCounters
	provider *startProviderFake
	selector *startSelectorFake
	accounts *testAccounts
	guard    *testGuard
	now      time.Time
	key      idempotency.Request
}

func startFixture(t *testing.T) *startFixtureData {
	t.Helper()
	f := &startFixtureData{tx: &testTX{}, repo: &startRepoFake{}, engine: &startEngineFake{current: true}, counters: &startCounters{}, now: time.Date(2026, 10, 23, 20, 0, 0, 0, time.UTC)}
	f.provider = &startProviderFake{tx: f.tx, name: "test", call: Call{ID: "call-1", Phone: "+79001234567", StartedAt: f.now, Numbers: []string{"+74951234567", "+74957654321"}}}
	f.selector = &startSelectorFake{tx: f.tx}
	f.accounts = &testAccounts{tx: f.tx}
	f.guard = &testGuard{tx: f.tx, alive: true}
	limits, err := ratelimit.New(ratelimit.Defaults(), f.counters, func() time.Time { return f.now }, prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	f.s, err = NewStarter(f.tx, f.repo, f.engine, limits, f.accounts, f.guard, f.provider, f.selector, func() time.Time { return f.now }, func() (string, error) { return "opaque-secret", nil }, slog.New(slog.DiscardHandler), prometheus.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	f.key = idempotency.Request{Endpoint: "/finassist.user.v1.AuthService/StartPhoneVerification", Key: "019c7714-3b77-74d1-9866-e1f484aae2ab", Hash: token.Sum("body")}
	return f
}
func (f *startFixtureData) run(ctx context.Context) (Started, error) {
	return f.s.Start(ctx, f.key, "+79001234567", "192.0.2.1")
}
func (f *startFixtureData) unfinished() {
	f.repo.record = &StartRecord{Key: f.key.Key, Hash: f.key.Hash, Request: Request{ID: 1, Phone: f.provider.call.Phone, Provider: "test", CreatedAt: f.now, ExpiresAt: f.now.Add(RequestTTL)}, Windows: make([]ratelimit.Window, 4)}
}

func TestStartCommitReplayAndRecoveredOwnAttempt(t *testing.T) {
	f := startFixture(t)
	out, err := f.run(t.Context())
	if err != nil || out.Token == "" || out.CallbackNumber != f.provider.call.Numbers[0] || !out.ExpiresAt.Equal(f.now.Add(RequestTTL)) {
		t.Fatal(out, err)
	}
	again, err := f.run(t.Context())
	if err != nil || again != out || f.provider.creates != 1 || f.selector.calls != 1 || f.counters.hits[0] != 1 {
		t.Fatal(again, err)
	}
	f = startFixture(t)
	f.unfinished()
	out, err = f.run(t.Context())
	if err != nil || out.Token == "" || f.provider.creates != 0 || f.provider.recovers != 1 || f.counters.hits != nil {
		t.Fatal(out, err)
	}
	f = startFixture(t)
	f.provider.createErr = ErrProviderUnavailable
	out, err = f.run(t.Context())
	if err != nil || out.Token == "" || f.provider.recovers != 1 {
		t.Fatal(out, err)
	}
}
func TestStartRejectDuplicateAndBusy(t *testing.T) {
	f := startFixture(t)
	f.provider.createErr = ErrProviderRejected
	if _, err := f.run(t.Context()); !errors.Is(err, ErrProviderUnavailable) || f.repo.rejected != 1 || f.counters.refunds != 1 {
		t.Fatal(err)
	}
	f = startFixture(t)
	f.provider.createErr = ErrProviderDuplicate
	if _, err := f.run(t.Context()); err == nil || f.repo.duplicated != 1 || f.counters.refunds != 0 {
		t.Fatal(err)
	}
	f = startFixture(t)
	f.repo.busy = f.now.Add(5*time.Minute + time.Second)
	if _, err := f.run(t.Context()); err == nil || f.provider.creates != 0 || f.engine.finishes != 1 || f.counters.hits[0] != 1 {
		t.Fatal(err)
	}
}
func TestStartErrorBranches(t *testing.T) {
	for name, mutate := range map[string]func(*startFixtureData){
		"reserve":     func(f *startFixtureData) { f.engine.reserveErr = errDependency },
		"transaction": func(f *startFixtureData) { f.tx.err = errDependency },
		"find":        func(f *startFixtureData) { f.repo.failure = "find" },
		"claim":       func(f *startFixtureData) { f.repo.failure = "claim" },
		"counter":     func(f *startFixtureData) { f.counters.err = errDependency },
		"full limit":  func(f *startFixtureData) { f.counters.hits = []int{999, 999, 999, 999} },
		"unavailable": func(f *startFixtureData) {
			f.provider.createErr = ErrProviderUnavailable
			f.provider.recoverErr = ErrProviderUnavailable
		},
		"invalid call":      func(f *startFixtureData) { f.provider.call.Phone = "foreign" },
		"selector":          func(f *startFixtureData) { f.selector.err = errDependency },
		"foreign selection": func(f *startFixtureData) { f.selector.out = "+18005550123" },
		"complete":          func(f *startFixtureData) { f.repo.failure = "complete" },
		"random":            func(f *startFixtureData) { f.s.newToken = func() (string, error) { return "", errDependency } },
		"final random": func(f *startFixtureData) {
			calls := 0
			f.s.newToken = func() (string, error) {
				calls++
				if calls == 2 {
					return "", errDependency
				}
				return "random", nil
			}
		},
		"owner lookup":   func(f *startFixtureData) { f.accounts.failure = "find-user" },
		"owner lock":     func(f *startFixtureData) { f.accounts.id = "owner"; f.accounts.failure = "lock-user" },
		"tombstone":      func(f *startFixtureData) { f.accounts.id = "owner"; f.guard.alive = false },
		"guard storage":  func(f *startFixtureData) { f.accounts.id = "owner"; f.guard.err = errDependency },
		"lost rejection": func(f *startFixtureData) { f.provider.createErr = ErrProviderRejected; f.engine.current = false },
		"reject storage": func(f *startFixtureData) { f.provider.createErr = ErrProviderRejected; f.repo.failure = "reject" },
		"refund storage": func(f *startFixtureData) {
			f.provider.create = func(context.Context, Attempt) (Call, error) {
				f.counters.err = errDependency
				return Call{}, ErrProviderRejected
			}
		},
		"duplicate storage": func(f *startFixtureData) { f.provider.createErr = ErrProviderDuplicate; f.repo.failure = "duplicate" },
		"finish engine":     func(f *startFixtureData) { f.engine.finishErr = errDependency },
		"disabled":          func(f *startFixtureData) { f.s.provider = nil; f.s.selector = nil },
		"recovery conflict": func(f *startFixtureData) { f.unfinished(); f.repo.record.Hash = token.Sum("other body") },
		"recovery provider": func(f *startFixtureData) { f.unfinished(); f.repo.record.Request.Provider = "old" },
		"forget":            func(f *startFixtureData) { f.unfinished(); f.repo.record.Windows = nil; f.repo.failure = "forget" },
	} {
		t.Run(name, func(t *testing.T) {
			f := startFixture(t)
			mutate(f)
			if out, err := f.run(t.Context()); err == nil || out.Token != "" {
				t.Fatal(out, err)
			}
		})
	}
}
func TestStartCancelledAndExpiredKeyLink(t *testing.T) {
	f := startFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.run(ctx); !errors.Is(err, context.Canceled) || f.engine.reserves != 0 {
		t.Fatal(err)
	}
	f = startFixture(t)
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	f.provider.create = func(context.Context, Attempt) (Call, error) { cancel(); return f.provider.call, nil }
	if out, err := f.run(ctx); err != nil || out.Token == "" || f.repo.completed != 1 {
		t.Fatal(out, err)
	}
	f = startFixture(t)
	f.unfinished()
	f.repo.record.Request.ExpiresAt = f.now
	if _, err := f.run(t.Context()); err != nil || f.repo.forgot != 1 || f.provider.creates != 1 {
		t.Fatal(err)
	}
}
func TestStartReplayBindingAndConstructor(t *testing.T) {
	for name, mutate := range map[string]func(*startFixtureData){
		"missing link":   func(f *startFixtureData) { f.repo.record = nil },
		"wrong hash":     func(f *startFixtureData) { f.repo.record.Hash = token.Sum("other") },
		"wrong token":    func(f *startFixtureData) { f.repo.record.Request.Hash = token.Sum("other") },
		"bad body":       func(f *startFixtureData) { f.engine.replay.Response.Body = []byte("invalid") },
		"owner mismatch": func(f *startFixtureData) { f.engine.replay.Response.OwnerID = "old-owner" },
	} {
		t.Run(name, func(t *testing.T) {
			f := startFixture(t)
			if _, err := f.run(t.Context()); err != nil {
				t.Fatal(err)
			}
			mutate(f)
			if out, err := f.run(t.Context()); err == nil || out.Token != "" || f.provider.creates != 1 {
				t.Fatal(out, err)
			}
		})
	}
	f := startFixture(t)
	if _, err := NewStarter(nil, f.repo, f.engine, f.s.limits, f.accounts, f.guard, f.provider, f.selector, time.Now, token.New, f.s.log, prometheus.NewRegistry()); err == nil {
		t.Fatal("nil dependency")
	}
	if _, err := NewStarter(f.tx, f.repo, f.engine, f.s.limits, f.accounts, f.guard, nil, f.selector, time.Now, token.New, f.s.log, prometheus.NewRegistry()); err == nil {
		t.Fatal("half backend")
	}
	f.provider.name = "INVALID"
	if _, err := NewStarter(f.tx, f.repo, f.engine, f.s.limits, f.accounts, f.guard, f.provider, f.selector, time.Now, token.New, f.s.log, prometheus.NewRegistry()); err == nil {
		t.Fatal("bad provider")
	}
	b, _ := json.Marshal(Started{})
	if _, err := decodeStarted(idempotency.Result{Response: idempotency.Response{Body: b}}); err == nil {
		t.Fatal("empty cached result")
	}
	for _, phone := range []string{"", "+7123", "+7900123456x", "+18005550123"} {
		if validStartPhone(phone) {
			t.Fatal(phone)
		}
	}
	if !validStartPhone("+79001234567") {
		t.Fatal("valid phone")
	}
	if got := ceilMinute(f.now.Add(time.Nanosecond)); !got.Equal(f.now.Add(time.Minute)) {
		t.Fatal(got)
	}
}
