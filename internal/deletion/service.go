// Package deletion atomically removes an account and schedules every remote purge.
package deletion

import (
	"context"
	"errors"
	"time"
)

// ErrReplayBusy means a cached authentication reply is locked by another request.
// Retrying requires rollback first so that request can finish its tombstone guard.
var ErrReplayBusy = errors.New("deletion: authentication reply busy")

// Transactions owns primary READ COMMITTED transactions.
type Transactions interface {
	Run(context.Context, func(context.Context) error) error
}

// Tombstones serializes deletion with writers and skips a completed deletion.
type Tombstones interface {
	Purge(context.Context, string, func(context.Context, string) error) error
}

// Repository deletes local data, including the owner's cached credentials.
type Repository interface {
	Remove(context.Context, string) error
}

// Events schedules the generated PurgeUserData message in the same transaction.
type Events interface {
	UserDeleted(context.Context, string) error
}

// Service is safe for concurrent use when its injected dependencies are.
type Service struct {
	tx         Transactions
	tombstones Tombstones
	repo       Repository
	events     Events
}

// New assembles the account-deletion usecase without starting background work.
func New(tx Transactions, tombstones Tombstones, repo Repository, events Events) *Service {
	return &Service{tx: tx, tombstones: tombstones, repo: repo, events: events}
}

// Delete commits the tombstone, local purge and deletion event together. A repeat
// succeeds without another event. Cache-lock contention rolls back before a short
// retry, bounded by the caller's context and a ten-second operation deadline.
func (s *Service) Delete(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		err := s.tx.Run(ctx, func(ctx context.Context) error {
			return s.tombstones.Purge(ctx, id, func(ctx context.Context, id string) error {
				if err := s.repo.Remove(ctx, id); err != nil {
					return err
				}
				return s.events.UserDeleted(ctx, id)
			})
		})
		if !errors.Is(err, ErrReplayBusy) {
			return err
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
