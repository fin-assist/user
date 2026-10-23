// Package outbox serializes user events for the shared transactional publisher.
package outbox

import (
	"context"

	usersettingsv1 "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"google.golang.org/protobuf/proto"
)

// TimezoneTopic is stable across producers, dispatchers and rolling upgrades.
const TimezoneTopic = "user.timezone_changed"

// Publisher writes an event in the caller's transaction.
type Publisher interface {
	Publish(context.Context, string, string, []byte) error
}

// Settings encodes the generated user-settings message without choosing recipients.
type Settings struct{ publisher Publisher }

// NewSettings wraps the shared outbox library publisher.
func NewSettings(p Publisher) *Settings { return &Settings{publisher: p} }

// TimezoneChanged writes the complete versioned snapshot for every configured recipient.
func (s *Settings) TimezoneChanged(ctx context.Context, id, zone string, version int32) error {
	payload, err := proto.Marshal(&usersettingsv1.SetUserTimezoneRequest{UserId: id, Timezone: zone, SettingsVersion: version})
	if err != nil {
		return err
	}
	return s.publisher.Publish(ctx, TimezoneTopic, id, payload)
}
