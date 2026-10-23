// Package outbox serializes user events for the shared transactional publisher.
package outbox

import (
	"context"

	userdeletionv1 "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	usersettingsv1 "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"google.golang.org/protobuf/proto"
)

// TimezoneTopic is stable across producers, dispatchers and rolling upgrades.
const TimezoneTopic = "user.timezone_changed"

// Publisher writes an event in the caller's transaction.
type Publisher interface {
	Publish(context.Context, string, string, []byte) error
}

// Events encodes generated settings and deletion messages without choosing recipients.
type Events struct{ publisher Publisher }

// NewEvents wraps the shared outbox library publisher.
func NewEvents(p Publisher) *Events { return &Events{publisher: p} }

// TimezoneChanged writes the complete versioned snapshot for every configured recipient.
func (s *Events) TimezoneChanged(ctx context.Context, id, zone string, version int32) error {
	payload, err := proto.Marshal(&usersettingsv1.SetUserTimezoneRequest{UserId: id, Timezone: zone, SettingsVersion: version})
	if err != nil {
		return err
	}
	return s.publisher.Publish(ctx, TimezoneTopic, id, payload)
}

// DeletionTopic identifies the durable account-deletion event.
const DeletionTopic = "user.deleted"

// UserDeleted publishes one logical event; the dispatcher expands its recipients.
func (s *Events) UserDeleted(ctx context.Context, id string) error {
	payload, err := proto.Marshal(&userdeletionv1.PurgeUserDataRequest{UserId: id})
	if err != nil {
		return err
	}
	return s.publisher.Publish(ctx, DeletionTopic, id, payload)
}
