package deletion

import (
	"context"
	"errors"
	"testing"
	"time"
)

type txFunc func(context.Context, func(context.Context) error) error

func (f txFunc) Run(ctx context.Context, fn func(context.Context) error) error { return f(ctx, fn) }

type purgeFunc func(context.Context, string, func(context.Context, string) error) error

func (f purgeFunc) Purge(ctx context.Context, id string, fn func(context.Context, string) error) error {
	return f(ctx, id, fn)
}

type removeFunc func(context.Context, string) error

func (f removeFunc) Remove(ctx context.Context, id string) error { return f(ctx, id) }

type eventFunc func(context.Context, string) error

func (f eventFunc) UserDeleted(ctx context.Context, id string) error { return f(ctx, id) }

func TestDeleteAtomicBoundariesAndErrors(t *testing.T) {
	want := errors.New("dependency failure")
	for _, mode := range []string{"ok", "repeat", "tx", "purger", "remove", "events"} {
		t.Run(mode, func(t *testing.T) {
			removed, published := 0, 0
			inTx := false
			tx := txFunc(func(ctx context.Context, fn func(context.Context) error) error {
				if mode == "tx" {
					return want
				}
				inTx = true
				defer func() { inTx = false }()
				return fn(ctx)
			})
			purger := purgeFunc(func(ctx context.Context, id string, fn func(context.Context, string) error) error {
				if !inTx || id != "verified-user" {
					t.Fatal("purge outside transaction or foreign identity")
				}
				if mode == "purger" {
					return want
				}
				if mode == "repeat" {
					return nil
				}
				return fn(ctx, id)
			})
			repo := removeFunc(func(_ context.Context, id string) error {
				if !inTx || id != "verified-user" {
					t.Fatal("remove outside transaction")
				}
				removed++
				if mode == "remove" {
					return want
				}
				return nil
			})
			events := eventFunc(func(_ context.Context, id string) error {
				if !inTx || id != "verified-user" || removed != 1 {
					t.Fatal("event before local purge")
				}
				published++
				if mode == "events" {
					return want
				}
				return nil
			})
			err := New(tx, purger, repo, events).Delete(t.Context(), "verified-user")
			if mode == "ok" || mode == "repeat" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, want) {
				t.Fatal(err)
			}
			if mode == "repeat" && (removed != 0 || published != 0) {
				t.Fatal("repeat produced effects")
			}
		})
	}
}
func TestDeleteRollsBackBeforeContentionRetryAndStops(t *testing.T) {
	attempts := 0
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		if cancelled {
			cancel()
		}
		inTx := false
		tx := txFunc(func(ctx context.Context, fn func(context.Context) error) error {
			attempts++
			inTx = true
			defer func() { inTx = false }()
			return fn(ctx)
		})
		purger := purgeFunc(func(ctx context.Context, id string, fn func(context.Context, string) error) error { return fn(ctx, id) })
		repo := removeFunc(func(context.Context, string) error {
			if cancelled || attempts == 1 {
				return ErrReplayBusy
			}
			return nil
		})
		published := 0
		err := New(tx, purger, repo, eventFunc(func(context.Context, string) error { published++; return nil })).Delete(ctx, "user")
		cancel()
		if inTx {
			t.Fatal("retry retained transaction")
		}
		if cancelled {
			if !errors.Is(err, context.Canceled) || published != 0 {
				t.Fatal(err, published)
			}
		} else if err != nil || attempts != 2 || published != 1 {
			t.Fatal(err, attempts, published)
		}
	}
}

func TestDeleteContentionHonorsCallerDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	tx := txFunc(func(ctx context.Context, fn func(context.Context) error) error { return fn(ctx) })
	purge := purgeFunc(func(ctx context.Context, id string, fn func(context.Context, string) error) error { return fn(ctx, id) })
	repo := removeFunc(func(context.Context, string) error { return ErrReplayBusy })
	events := eventFunc(func(context.Context, string) error { t.Fatal("deadline produced event"); return nil })
	if err := New(tx, purge, repo, events).Delete(ctx, "user"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
