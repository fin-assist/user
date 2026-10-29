package auth

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/fin-assist/contracts/gen/go/finerr"

	"github.com/fin-assist/user/internal/token"
)

var errDependency = errors.New("test dependency")

type fakeTx struct{ commitErr error }

func (x *fakeTx) Run(ctx context.Context, fn func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fn(ctx); err != nil {
		return err
	}
	return x.commitErr
}

type fakeRepo struct {
	l                 Link
	v                 Registration
	lockedV           *Registration
	version           string
	created           bool
	fail              map[string]error
	calls             []string
	newHash           token.Hash
	writeTime, expiry time.Time
	newZone           string
	logoutUser        string
}

func (r *fakeRepo) call(name string) error { r.calls = append(r.calls, name); return r.fail[name] }
func (r *fakeRepo) FindLink(context.Context, token.Hash, time.Time) (Link, error) {
	return r.l, r.call("find")
}
func (r *fakeRepo) LockLink(context.Context, token.Hash, time.Time) (Link, error) {
	return r.l, r.call("lock")
}
func (r *fakeRepo) Rotate(_ context.Context, _, h token.Hash, _ Link, now, expiry time.Time) error {
	r.newHash = h
	r.writeTime = now
	r.expiry = expiry
	return r.call("rotate")
}
func (r *fakeRepo) Revoke(_ context.Context, _ Link, now time.Time) error {
	r.writeTime = now
	return r.call("revoke")
}
func (r *fakeRepo) Logout(_ context.Context, id string, _ token.Hash) error {
	r.logoutUser = id
	return r.call("logout")
}
func (r *fakeRepo) Registration(_ context.Context, _ token.Hash, lock bool) (Registration, error) {
	if lock {
		v := r.v
		if r.lockedV != nil {
			v = *r.lockedV
		}
		return v, r.call("reg-lock")
	}
	return r.v, r.call("reg-read")
}
func (r *fakeRepo) CurrentPD(context.Context) (string, error) { return r.version, r.call("document") }
func (r *fakeRepo) CreateUser(_ context.Context, _, zone string, now time.Time) (string, bool, error) {
	r.newZone = zone
	r.writeTime = now
	return "user", r.created, r.call("create")
}
func (r *fakeRepo) LockUser(context.Context, string) error { return r.call("user-lock") }
func (r *fakeRepo) GrantPD(context.Context, string, string, time.Time) error {
	return r.call("consent")
}
func (r *fakeRepo) CreateSession(_ context.Context, _ string, _ int32, h token.Hash, now, expiry time.Time) error {
	r.newHash = h
	r.writeTime = now
	r.expiry = expiry
	return r.call("session")
}
func (r *fakeRepo) CloseVerification(context.Context, int32, string, time.Time) error {
	return r.call("close")
}

type fakeGuard struct {
	alive bool
	err   error
	calls int
}

func (g *fakeGuard) Guard(context.Context, string) (bool, error) { g.calls++; return g.alive, g.err }

type fakeSigner struct{ err error }

func (s *fakeSigner) Sign(context.Context, string) (string, error) { return "access", s.err }

type fakeEvents struct {
	calls int
	zone  string
	err   error
}

func (e *fakeEvents) TimezoneChanged(_ context.Context, _ string, zone string, version int32) error {
	e.calls++
	e.zone = zone
	if version != 1 {
		panic("initial version")
	}
	return e.err
}

type fixture struct {
	s    *Service
	r    *fakeRepo
	g    *fakeGuard
	tx   *fakeTx
	sign *fakeSigner
	e    *fakeEvents
	now  time.Time
}

func newFixture() fixture {
	now := time.Date(2026, 10, 23, 23, 59, 59, 0, time.UTC)
	verified := now.Add(-2 * time.Minute)
	id := int32(3)
	r := &fakeRepo{l: Link{SessionID: 2, Generation: 1, UserID: "user", VerificationID: &id, IssuedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}, v: Registration{ID: 3, ConsentVersion: "v1", ConsentAcceptedAt: now.Add(-2 * time.Minute), AgeConfirmedAt: now.Add(-2 * time.Minute), Phone: "+79001234567", VerifiedAt: &verified, IsNewUser: true}, version: "v1", created: true, fail: map[string]error{}}
	g := &fakeGuard{alive: true}
	tx := &fakeTx{}
	sign := &fakeSigner{}
	e := &fakeEvents{}
	s := New(tx, r, g, sign, e, func() time.Time { return now }, func() (string, error) { return "refresh", nil }, 60*24*time.Hour)
	return fixture{s, r, g, tx, sign, e, now}
}
func reason(err error) string {
	if r, ok := finerr.ToREST(err); ok {
		return r.Code
	}
	return ""
}
func validRegister() RegisterInput {
	return RegisterInput{Token: "registration", Accepted: true, ConsentVersion: "v1"}
}

func TestUnitRefreshRotatesAndClosesResult(t *testing.T) {
	f := newFixture()
	out, err := f.s.Refresh(t.Context(), "old")
	if err != nil || out.Pair != (Pair{"access", "refresh"}) || out.UserID != "user" || out.Reused {
		t.Fatal(out, err)
	}
	if f.r.newHash != token.Sum("refresh") || !f.r.expiry.Equal(f.now.Add(60*24*time.Hour)) || !reflect.DeepEqual(f.r.calls, []string{"find", "lock", "rotate", "close"}) {
		t.Fatal(f.r)
	}
	f = newFixture()
	f.r.l.VerificationID = nil
	f.r.l.IssuedAt = f.now.Add(time.Minute)
	if _, err := f.s.Refresh(t.Context(), "old"); err != nil || !f.r.writeTime.Equal(f.r.l.IssuedAt) {
		t.Fatal(err)
	}
}

func TestUnitRefreshRejectsAndCompletesReuse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*fixture)
		want   string
		reuse  bool
	}{
		{"unknown", func(f *fixture) { f.r.fail["find"] = ErrMissing }, "INVALID_TOKEN", false},
		{"deleted", func(f *fixture) { f.g.alive = false }, "TOKEN_REVOKED", false},
		{"revoked", func(f *fixture) { f.r.l.RevokedAt = &f.now }, "TOKEN_REVOKED", false},
		{"reused", func(f *fixture) { f.r.l.RotatedAt = &f.now }, "TOKEN_REVOKED", true},
		{"zero-generation", func(f *fixture) { f.r.l.Generation = 0 }, "", false},
		{"overflow", func(f *fixture) { f.r.l.Generation = math.MaxInt32 }, "", false},
		{"expired", func(f *fixture) { f.r.l.ExpiresAt = f.now }, "INVALID_TOKEN", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			tc.mutate(&f)
			out, err := f.s.Refresh(t.Context(), "old")
			if err == nil || reason(err) != tc.want || out.Reused != tc.reuse || out.Pair != (Pair{}) {
				t.Fatal(out, err)
			}
			for _, c := range f.r.calls {
				if c == "rotate" || c == "close" {
					t.Fatal("refusal wrote tokens")
				}
			}
		})
	}
	f := newFixture()
	future := f.now.Add(time.Minute)
	f.r.l.RotatedAt = &future
	if err := f.s.CompleteReuse(t.Context(), "old"); err != nil || !f.r.writeTime.Equal(future) {
		t.Fatal(err)
	}
	for _, mutate := range []func(*fixture){func(f *fixture) { f.r.fail["find"] = ErrMissing }, func(f *fixture) { f.r.l.RevokedAt = &f.now }, func(f *fixture) { f.r.l.RotatedAt = nil }} {
		f := newFixture()
		mutate(&f)
		if err := f.s.CompleteReuse(t.Context(), "old"); err != nil {
			t.Fatal(err)
		}
		for _, c := range f.r.calls {
			if c == "revoke" {
				t.Fatal("unrelated chain revoked")
			}
		}
	}
	for _, stage := range []string{"find", "lock", "revoke"} {
		f := newFixture()
		f.r.l.RotatedAt = &f.now
		f.r.fail[stage] = errDependency
		if err := f.s.CompleteReuse(t.Context(), "old"); !errors.Is(err, errDependency) {
			t.Fatal(stage, err)
		}
	}
}

func TestUnitRefreshAndLogoutDependencyFailures(t *testing.T) {
	for _, stage := range []string{"find", "lock", "rotate", "close"} {
		f := newFixture()
		f.r.fail[stage] = errDependency
		if _, err := f.s.Refresh(t.Context(), "old"); !errors.Is(err, errDependency) {
			t.Fatal(stage, err)
		}
	}
	for _, mutate := range []func(*fixture){func(f *fixture) { f.g.err = errDependency }, func(f *fixture) { f.sign.err = errDependency }, func(f *fixture) { f.tx.commitErr = errDependency }, func(f *fixture) { f.s.newToken = func() (string, error) { return "", errDependency } }} {
		f := newFixture()
		mutate(&f)
		if _, err := f.s.Refresh(t.Context(), "old"); !errors.Is(err, errDependency) {
			t.Fatal(err)
		}
	}
	f := newFixture()
	if err := f.s.Logout(t.Context(), "user", "unknown"); err != nil || f.r.logoutUser != "user" {
		t.Fatal(f.r.logoutUser, err)
	}
	f.r.fail["logout"] = errDependency
	if err := f.s.Logout(t.Context(), "user", "old"); !errors.Is(err, errDependency) {
		t.Fatal(err)
	}
	// Without a verified owner nothing is deleted.
	calls := len(f.r.calls)
	if err := f.s.Logout(t.Context(), "", "old"); reason(err) != "INVALID_TOKEN" || len(f.r.calls) != calls {
		t.Fatal(err, f.r.calls)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.s.Refresh(ctx, "old"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestUnitRegistrationNewAccountAndExistingAccount(t *testing.T) {
	f := newFixture()
	out, err := f.s.Register(t.Context(), validRegister())
	if err != nil || out.UserID != "user" || out.Pair.Refresh != "refresh" || f.e.calls != 1 || f.e.zone != "Europe/Moscow" || !f.r.expiry.Equal(f.now.Add(60*24*time.Hour)) {
		t.Fatal(out, err)
	}
	if !reflect.DeepEqual(f.r.calls, []string{"reg-read", "create", "user-lock", "reg-lock", "consent", "session", "close"}) {
		t.Fatal(f.r.calls)
	}
	f = newFixture()
	f.r.created = false
	in := validRegister()
	zone := "Asia/Tokyo"
	in.Timezone = &zone
	if _, err := f.s.Register(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if f.e.calls != 0 {
		t.Fatal("existing timezone changed")
	}
	for _, c := range f.r.calls {
		if c == "consent" {
			t.Fatal("existing consent changed")
		}
	}
}

func TestUnitRegistrationValidationOrderAndOneTimeResult(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*fixture, *RegisterInput)
		want   string
	}{
		{"missing", func(f *fixture, _ *RegisterInput) { f.r.fail["reg-read"] = ErrMissing }, "INVALID_TOKEN"},
		{"unconfirmed", func(f *fixture, _ *RegisterInput) { f.r.v.VerifiedAt = nil }, "INVALID_TOKEN"},
		{"existing-result", func(f *fixture, _ *RegisterInput) { f.r.v.IsNewUser = false }, "INVALID_TOKEN"},
		{"closed", func(f *fixture, _ *RegisterInput) { f.r.v.Closed = true }, "INVALID_TOKEN"},
		{"expired", func(f *fixture, _ *RegisterInput) { v := f.now.Add(-15 * time.Minute); f.r.v.VerifiedAt = &v }, "TOKEN_EXPIRED"},
		{"zone-before-consent", func(_ *fixture, in *RegisterInput) { v := "Local"; in.Timezone = &v; in.Accepted = false }, "VALIDATION_ERROR"},
		{"consent", func(_ *fixture, in *RegisterInput) { in.Accepted = false }, "CONSENT_REQUIRED"},
		{"old-version", func(_ *fixture, in *RegisterInput) { in.ConsentVersion = "old" }, "CONSENT_OUTDATED"},
		{"missing-evidence", func(f *fixture, _ *RegisterInput) { f.r.v.ConsentVersion = "" }, "INVALID_TOKEN"},
		{"deleted", func(f *fixture, _ *RegisterInput) { f.g.alive = false }, "INVALID_TOKEN"},
		{"consumed-during-lock", func(f *fixture, _ *RegisterInput) { v := f.r.v; v.Closed = true; f.r.lockedV = &v }, "INVALID_TOKEN"},
		{"future-verified", func(f *fixture, _ *RegisterInput) { v := f.now.Add(time.Minute); f.r.v.VerifiedAt = &v }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			in := validRegister()
			tc.mutate(&f, &in)
			_, err := f.s.Register(t.Context(), in)
			if err == nil || reason(err) != tc.want {
				t.Fatal(err)
			}
		})
	}
}

func TestUnitRegistrationRollsBackDependencyFailures(t *testing.T) {
	for _, stage := range []string{"reg-read", "create", "user-lock", "reg-lock", "consent", "session", "close"} {
		f := newFixture()
		f.r.fail[stage] = errDependency
		if _, err := f.s.Register(t.Context(), validRegister()); !errors.Is(err, errDependency) {
			t.Fatal(stage, err)
		}
	}
	for _, mutate := range []func(*fixture){func(f *fixture) { f.g.err = errDependency }, func(f *fixture) { f.e.err = errDependency }, func(f *fixture) { f.sign.err = errDependency }, func(f *fixture) { f.tx.commitErr = errDependency }, func(f *fixture) { f.s.newToken = func() (string, error) { return "", errDependency } }} {
		f := newFixture()
		mutate(&f)
		if _, err := f.s.Register(t.Context(), validRegister()); !errors.Is(err, errDependency) {
			t.Fatal(err)
		}
	}
}
