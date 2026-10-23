package verification

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fin-assist/contracts/gen/go/finerr"

	"github.com/fin-assist/user/internal/auth"
	"github.com/fin-assist/user/internal/token"
)

var errDependency = errors.New("dependency failed")

type testTX struct {
	err                error
	active             bool
	commits, rollbacks int
}

func (x *testTX) Run(ctx context.Context, fn func(context.Context) error) error {
	if x.err != nil {
		return x.err
	}
	x.active = true
	defer func() { x.active = false }()
	err := fn(ctx)
	if err != nil {
		x.rollbacks++
	} else {
		x.commits++
	}
	return err
}

type testRepo struct {
	mu           sync.Mutex
	r            Request
	tx           *testTX
	failure      string
	locks        int
	changeOwner  bool
	byTokenOwner string
	stages       []string
	stageFailure string
}

func (r *testRepo) read() (Request, error) {
	if r.failure == "missing" {
		return Request{}, ErrMissing
	}
	return r.r, nil
}
func (r *testRepo) ByToken(context.Context, token.Hash) (Request, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure == "by-token" {
		return Request{}, errDependency
	}
	out, err := r.read()
	if r.changeOwner && r.locks == 0 {
		out.UserID = r.byTokenOwner
	}
	return out, err
}
func (r *testRepo) ByCall(context.Context, string, string) (Request, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure == "by-call" {
		return Request{}, errDependency
	}
	return r.read()
}
func (r *testRepo) Lock(_ context.Context, _ int32, _ bool) (Request, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.locks++
	if !r.tx.active {
		panic("lock outside transaction")
	}
	if r.failure == "lock" {
		return Request{}, errDependency
	}
	if r.failure == "lock-missing" {
		return Request{}, ErrMissing
	}
	return r.read()
}
func (r *testRepo) Verify(_ context.Context, _ Request, now time.Time, o Outcome, h *token.Hash, b []byte, k string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.tx.active {
		panic("verify outside transaction")
	}
	if r.failure == "verify" {
		return errDependency
	}
	r.r.VerifiedAt = &now
	r.r.IsNewUser = o.IsNewUser
	r.r.UserID = o.UserID
	r.r.RegistrationHash = h
	r.r.Ciphertext = b
	r.r.KeyID = k
	return nil
}
func (r *testRepo) stage(name string) (int64, error) {
	if !r.tx.active {
		panic("cleanup outside transaction")
	}
	r.stages = append(r.stages, name)
	if r.stageFailure == name {
		return 0, errDependency
	}
	return 1, nil
}
func (r *testRepo) CleanupResults(context.Context, time.Time, int) (int64, error) {
	return r.stage("results")
}
func (r *testRepo) DetachOldRequests(context.Context, time.Time, int) (int64, error) {
	return r.stage("detach")
}
func (r *testRepo) DeleteOldRequests(context.Context, time.Time, int) (int64, error) {
	return r.stage("delete")
}

type testAccounts struct {
	id      string
	failure string
	created int
	hash    token.Hash
	expires time.Time
	tx      *testTX
}

func (a *testAccounts) FindUserByPhone(context.Context, string) (string, error) {
	if a.failure == "find-user" {
		return "", errDependency
	}
	return a.id, nil
}
func (a *testAccounts) LockUser(context.Context, string) error {
	if !a.tx.active {
		panic("user lock outside transaction")
	}
	if a.failure == "lock-user" {
		return errDependency
	}
	return nil
}
func (a *testAccounts) CreateSession(_ context.Context, _ string, _ int32, h token.Hash, _ time.Time, e time.Time) error {
	if !a.tx.active {
		panic("session outside transaction")
	}
	if a.failure == "session" {
		return errDependency
	}
	a.created++
	a.hash = h
	a.expires = e
	return nil
}

type testGuard struct {
	alive bool
	err   error
	calls int
	tx    *testTX
}

func (g *testGuard) Guard(context.Context, string) (bool, error) {
	if !g.tx.active {
		panic("guard outside transaction")
	}
	g.calls++
	return g.alive, g.err
}

type testSigner struct {
	err   error
	calls int
}

func (s *testSigner) Sign(context.Context, string) (string, error) { s.calls++; return "access", s.err }

type testCodec struct {
	o                Outcome
	sealErr, openErr error
	seals            int
}

func (c *testCodec) Seal(_ Request, o Outcome) ([]byte, string, error) {
	if c.sealErr != nil {
		return nil, "", c.sealErr
	}
	c.o = o
	c.seals++
	return []byte("ciphertext"), "key", nil
}
func (c *testCodec) Open(Request) (Outcome, error) { return c.o, c.openErr }

type testNotifications struct {
	changed       chan struct{}
	done          chan struct{}
	err           error
	calls, closed int
	subscribe     func()
}

func (n *testNotifications) Subscribe(int32) (Subscription, error) {
	n.calls++
	if n.err != nil {
		return Subscription{}, n.err
	}
	if n.subscribe != nil {
		n.subscribe()
	}
	return Subscription{Changed: n.changed, Done: n.done, Close: func() { n.closed++ }}, nil
}

type phoneFixture struct {
	s        *Service
	r        *testRepo
	a        *testAccounts
	g        *testGuard
	c        *testCodec
	signer   *testSigner
	n        *testNotifications
	tx       *testTX
	now      time.Time
	clockMu  sync.Mutex
	tokens   int
	tokenErr error
}

func fixture(t *testing.T) *phoneFixture {
	t.Helper()
	f := &phoneFixture{now: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC), tx: &testTX{}, c: &testCodec{}, signer: &testSigner{}, n: &testNotifications{changed: make(chan struct{}, 1), done: make(chan struct{})}}
	f.r = &testRepo{tx: f.tx, r: Request{ID: 1, Phone: "+79001234567", Provider: "test", CallID: "call-1", Hash: token.Sum("secret"), CreatedAt: f.now.Add(-time.Minute), ExpiresAt: f.now.Add(4 * time.Minute)}}
	f.a = &testAccounts{tx: f.tx}
	f.g = &testGuard{tx: f.tx, alive: true}
	f.s = New(f.tx, f.r, f.a, f.g, f.signer, f.c, f.n, func() time.Time { f.clockMu.Lock(); defer f.clockMu.Unlock(); return f.now }, func() (string, error) { f.tokens++; return "generated-secret", f.tokenErr }, 60*24*time.Hour, time.Hour)
	return f
}
func goodProof() Proof { return Proof{Provider: "test", CallID: "call-1", Phone: "+79001234567"} }
func reason(err error) string {
	r, ok := finerr.ToREST(err)
	if ok {
		return r.Code
	}
	return ""
}

func TestConfirmNewExistingAndRepeatedProof(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "existing"}[existing], func(t *testing.T) {
			f := fixture(t)
			if existing {
				f.a.id = "signed-user"
			}
			changed, err := f.s.Confirm(t.Context(), goodProof())
			if err != nil || !changed {
				t.Fatal(changed, err)
			}
			r, out, err := f.s.Snapshot(t.Context(), "secret")
			if err != nil || out.State != Verified || out.IsNewUser == existing || !r.VerifiedAt.Equal(f.now) {
				t.Fatal(r, out, err)
			}
			if existing {
				if out.Pair.Access != "access" || out.RegistrationToken != "" || f.a.created != 1 || f.a.hash != token.Sum(out.Pair.Refresh) || !f.a.expires.Equal(f.now.Add(60*24*time.Hour)) {
					t.Fatal(out, f.a)
				}
			} else if out.RegistrationToken != "generated-secret" || out.Pair != (auth.Pair{}) || f.a.created != 0 {
				t.Fatal(out, f.a)
			}
			before := out
			if changed, err = f.s.Confirm(t.Context(), goodProof()); err != nil || changed || f.tokens != 1 || f.c.seals != 1 {
				t.Fatal(changed, err, f.tokens)
			}
			_, again, err := f.s.Snapshot(t.Context(), "secret")
			if err != nil || again != before {
				t.Fatal("result changed", again, err)
			}
		})
	}
}
func TestConfirmRejectionsAndDependencyFailures(t *testing.T) {
	for _, p := range []Proof{{Provider: "invalid/provider", CallID: "call-1", Phone: "+79001234567"}, {Provider: "test", CallID: strings.Repeat("x", 129), Phone: "+79001234567"}, {Provider: "test", CallID: "", Phone: "+79001234567"}, {Provider: "test", CallID: "call-1", Phone: "+7900123456x"}, {Provider: "test", CallID: "call-1", Phone: "+79001234568"}} {
		f := fixture(t)
		if changed, err := f.s.Confirm(t.Context(), p); changed || !errors.Is(err, ErrProof) || f.tokens != 0 {
			t.Fatal(changed, err)
		}
	}
	for _, stage := range []string{"tx", "by-call", "find-user", "guard", "lock-user", "lock", "token", "sign", "session", "seal", "verify"} {
		t.Run(stage, func(t *testing.T) {
			f := fixture(t)
			f.a.id = "existing"
			switch stage {
			case "tx":
				f.tx.err = errDependency
			case "by-call", "lock", "verify":
				f.r.failure = stage
			case "find-user", "lock-user", "session":
				f.a.failure = stage
			case "guard":
				f.g.err = errDependency
			case "token":
				f.tokenErr = errDependency
			case "sign":
				f.signer.err = errDependency
			case "seal":
				f.c.sealErr = errDependency
			}
			if changed, err := f.s.Confirm(t.Context(), goodProof()); changed || !errors.Is(err, errDependency) {
				t.Fatal(changed, err)
			}
		})
	}
	for _, mode := range []string{"expired", "closed", "deleted", "lock-missing", "future"} {
		t.Run(mode, func(t *testing.T) {
			f := fixture(t)
			switch mode {
			case "expired":
				f.r.r.ExpiresAt = f.now
			case "closed":
				f.r.r.Closed = true
			case "deleted":
				f.a.id = "deleted"
				f.g.alive = false
			case "lock-missing":
				f.r.failure = mode
			case "future":
				f.r.r.CreatedAt = f.now.Add(time.Second)
			}
			changed, err := f.s.Confirm(t.Context(), goodProof())
			if changed || f.tokens != 0 || (mode == "future" && err == nil) || (mode != "future" && err != nil) {
				t.Fatal(changed, err)
			}
		})
	}
}
func TestSnapshotBoundariesCorruptionAndOwnerRace(t *testing.T) {
	for _, mode := range []string{"pending", "expiry", "missing", "wrong-hash", "closed", "by-token", "lock", "lock-missing", "future-result", "result-expired", "no-cipher", "no-key", "open-error", "bad-state", "bad-owner", "bad-kind", "bad-reg-hash", "empty-reg", "reg-pair", "session-missing", "session-reg"} {
		t.Run(mode, func(t *testing.T) {
			f := fixture(t)
			if mode != "pending" && mode != "expiry" && mode != "missing" && mode != "wrong-hash" && mode != "by-token" && mode != "lock" && mode != "lock-missing" {
				if strings.HasPrefix(mode, "session-") {
					f.a.id = "existing"
				}
				if _, err := f.s.Confirm(t.Context(), goodProof()); err != nil {
					t.Fatal(err)
				}
			}
			switch mode {
			case "expiry":
				f.r.r.ExpiresAt = f.now
			case "missing", "by-token", "lock", "lock-missing":
				f.r.failure = mode
			case "wrong-hash":
				f.r.r.Hash = token.Sum("foreign")
			case "closed":
				f.r.r.Closed = true
			case "future-result":
				future := f.now.Add(time.Second)
				f.r.r.VerifiedAt = &future
			case "result-expired":
				f.now = f.now.Add(ResultTTL)
			case "no-cipher":
				f.r.r.Ciphertext = nil
			case "no-key":
				f.r.r.KeyID = ""
			case "open-error":
				f.c.openErr = errDependency
			case "bad-state":
				f.c.o.State = Pending
			case "bad-owner":
				f.c.o.UserID = "foreign"
			case "bad-kind":
				f.c.o.IsNewUser = false
			case "bad-reg-hash":
				h := token.Sum("foreign")
				f.r.r.RegistrationHash = &h
			case "empty-reg":
				f.c.o.RegistrationToken = ""
			case "reg-pair":
				f.c.o.Pair.Access = "unexpected"
			case "session-missing":
				f.c.o.Pair.Refresh = ""
			case "session-reg":
				f.c.o.RegistrationToken = "unexpected"
			}
			_, out, err := f.s.Snapshot(t.Context(), "secret")
			if mode == "future-result" {
				if err != nil || out.State != Verified {
					t.Fatal("issuer clock ahead rejected committed result", out, err)
				}
			} else if mode == "pending" {
				if err != nil || out.State != Pending {
					t.Fatal(out, err)
				}
			} else if err == nil {
				t.Fatal("invalid state accepted", out)
			}
		})
	}
	f := fixture(t)
	f.a.id = "existing"
	if _, err := f.s.Confirm(t.Context(), goodProof()); err != nil {
		t.Fatal(err)
	}
	f.r.locks = 0
	f.r.changeOwner = true
	if _, _, err := f.s.Snapshot(t.Context(), "secret"); err != nil || f.r.locks != 2 || f.tx.rollbacks != 1 {
		t.Fatal("owner race not retried", err, f.r.locks, f.tx.rollbacks)
	}
	f.g.alive = false
	if _, _, err := f.s.Snapshot(t.Context(), "secret"); reason(err) != "INVALID_TOKEN" {
		t.Fatal(err)
	}
	f.g.alive = true
	f.g.err = errDependency
	if _, _, err := f.s.Snapshot(t.Context(), "secret"); !errors.Is(err, errDependency) {
		t.Fatal(err)
	}
}
func TestWatchSubscribeBeforeSnapshotAndTerminalReplay(t *testing.T) {
	f := fixture(t)
	f.n.subscribe = func() {
		if _, err := f.s.Confirm(t.Context(), goodProof()); err != nil {
			t.Fatal(err)
		}
	}
	sent := []State{}
	if err := f.s.Watch(t.Context(), "secret", func(o Outcome) error { sent = append(sent, o.State); return nil }); err != nil || len(sent) != 1 || sent[0] != Verified || f.n.closed != 1 {
		t.Fatal(sent, err)
	}
	f.now = f.now.Add(6 * time.Minute)
	if err := f.s.Watch(t.Context(), "secret", func(o Outcome) error {
		if o.State != Verified {
			t.Fatal(o)
		}
		return nil
	}); err != nil || f.n.calls != 1 {
		t.Fatal(err, f.n.calls)
	}
}
func TestWatchExpiryCancellationFailureAndRelease(t *testing.T) {
	for _, mode := range []string{"expire", "cancel", "disconnect", "send", "subscribe", "missing"} {
		t.Run(mode, func(t *testing.T) {
			f := fixture(t)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if mode == "subscribe" {
				f.n.err = errDependency
			}
			if mode == "missing" {
				f.r.failure = "missing"
			}
			sent := make(chan Outcome, 4)
			done := make(chan error, 1)
			go func() {
				done <- f.s.Watch(ctx, "secret", func(o Outcome) error {
					sent <- o
					if mode == "send" {
						return errDependency
					}
					return nil
				})
			}()
			if mode != "subscribe" && mode != "missing" {
				select {
				case o := <-sent:
					if o.State != Pending {
						t.Fatal(o)
					}
				case <-ctx.Done():
					t.Fatal("no pending")
				}
				switch mode {
				case "expire":
					f.clockMu.Lock()
					f.now = f.r.r.ExpiresAt
					f.clockMu.Unlock()
					f.n.changed <- struct{}{}
				case "cancel":
					cancel()
				case "disconnect":
					close(f.n.done)
				}
			}
			select {
			case err := <-done:
				if mode == "expire" {
					if err != nil {
						t.Fatal(err)
					}
					if o := <-sent; o.State != Expired {
						t.Fatal(o)
					}
				} else if err == nil {
					t.Fatal("failure became success")
				}
			case <-ctx.Done():
				if mode != "cancel" {
					t.Fatal("watch leaked")
				}
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			}
			if mode != "subscribe" && mode != "missing" && f.n.closed != 1 {
				t.Fatal("subscription retained")
			}
		})
	}
}
func TestCleanupStagesCommitSeparatelyAndStopOnError(t *testing.T) {
	for _, stage := range []string{"", "results", "detach", "delete"} {
		f := fixture(t)
		f.r.stageFailure = stage
		n, err := f.s.Cleanup(t.Context(), 1000)
		if stage == "" {
			if err != nil || n != 3 || f.tx.commits != 3 {
				t.Fatal(n, err)
			}
		} else if !errors.Is(err, errDependency) || f.tx.rollbacks != 1 {
			t.Fatal(n, err)
		}
	}
	f := fixture(t)
	if _, err := f.s.Cleanup(t.Context(), 0); err == nil || f.tx.commits != 0 {
		t.Fatal(err)
	}
}

func TestPhoneAdditionalErrorAndClockEdges(t *testing.T) {
	f := fixture(t)
	f.tokenErr = errDependency
	if changed, err := f.s.Confirm(t.Context(), goodProof()); changed || !errors.Is(err, errDependency) {
		t.Fatal(changed, err)
	}
	for _, phone := range []string{"", "+8", "+89001234567", "+790012345678"} {
		p := goodProof()
		p.Phone = phone
		if _, err := f.s.Confirm(t.Context(), p); !errors.Is(err, ErrProof) {
			t.Fatal(err)
		}
	}
	f = fixture(t)
	f.n.subscribe = func() { f.r.r.Closed = true }
	if err := f.s.Watch(t.Context(), "secret", func(Outcome) error { t.Fatal("consumed result delivered"); return nil }); reason(err) != "INVALID_TOKEN" || f.n.closed != 1 {
		t.Fatal(err)
	}
	f = fixture(t)
	var states []State
	err := f.s.Watch(t.Context(), "secret", func(o Outcome) error {
		states = append(states, o.State)
		if o.State == Pending {
			f.clockMu.Lock()
			f.now = f.r.r.ExpiresAt.Add(time.Second)
			f.clockMu.Unlock()
		}
		return nil
	})
	if err != nil || len(states) != 2 || states[0] != Pending || states[1] != Expired {
		t.Fatal("clock passed expiry during send", states, err)
	}
	f = fixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := f.s.Watch(ctx, "secret", func(Outcome) error { return nil }); !errors.Is(err, context.DeadlineExceeded) || f.n.closed != 1 {
		t.Fatal("deadline retained subscription", err)
	}
}

func TestReleasedPendingCannotBecomeLiveAfterClockRollback(t *testing.T) {
	f := fixture(t)
	f.r.r.Released = true
	if changed, err := f.s.Confirm(t.Context(), goodProof()); changed || err != nil || f.tokens != 0 {
		t.Fatal(changed, err)
	}
	if _, _, err := f.s.Snapshot(t.Context(), "secret"); reason(err) != "INVALID_TOKEN" {
		t.Fatal(err)
	}
	if _, out, err := f.s.snapshot(t.Context(), token.Sum("secret"), true); err != nil || out.State != Expired {
		t.Fatal(out, err)
	}
	f = fixture(t)
	if _, err := f.s.Confirm(t.Context(), goodProof()); err != nil {
		t.Fatal(err)
	}
	f.r.r.Released = true
	if _, out, err := f.s.Snapshot(t.Context(), "secret"); err != nil || out.State != Verified {
		t.Fatal("release ended live verified result", out, err)
	}
}
