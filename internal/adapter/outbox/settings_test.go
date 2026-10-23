package outbox

import (
	"context"
	"errors"
	"testing"

	userdeletionv1 "github.com/fin-assist/contracts/gen/go/finassist/userdeletion/v1"
	usersettingsv1 "github.com/fin-assist/contracts/gen/go/finassist/usersettings/v1"
	"google.golang.org/protobuf/proto"
)

type publishFunc func(context.Context, string, string, []byte) error

func (f publishFunc) Publish(ctx context.Context, topic, key string, b []byte) error {
	return f(ctx, topic, key, b)
}
func TestTimezoneEventEncodingAndErrors(t *testing.T) {
	want := errors.New("publisher failed")
	fail := false
	calls := 0
	s := NewEvents(publishFunc(func(_ context.Context, topic, key string, b []byte) error {
		calls++
		var m usersettingsv1.SetUserTimezoneRequest
		if err := proto.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		if topic != TimezoneTopic || key != "user" || m.GetUserId() != key || m.GetTimezone() != "UTC" || m.GetSettingsVersion() != 42 {
			t.Fatalf("%s %s %v", topic, key, &m)
		}
		if fail {
			return want
		}
		return nil
	}))
	if err := s.TimezoneChanged(t.Context(), "user", "UTC", 42); err != nil {
		t.Fatal(err)
	}
	fail = true
	if err := s.TimezoneChanged(t.Context(), "user", "UTC", 42); !errors.Is(err, want) {
		t.Fatal(err)
	}
	if err := s.TimezoneChanged(t.Context(), "user", string([]byte{255}), 42); err == nil || calls != 2 {
		t.Fatal("invalid protobuf reached publisher")
	}
}

func TestDeletionEventEncodingAndErrors(t *testing.T) {
	want := errors.New("publisher unavailable")
	calls := 0
	events := NewEvents(publishFunc(func(_ context.Context, topic, key string, b []byte) error {
		calls++
		var m userdeletionv1.PurgeUserDataRequest
		if err := proto.Unmarshal(b, &m); err != nil || topic != DeletionTopic || key != "user" || m.GetUserId() != key {
			t.Fatal(topic, key, err)
		}
		return want
	}))
	if err := events.UserDeleted(t.Context(), "user"); !errors.Is(err, want) || calls != 1 {
		t.Fatal(err, calls)
	}
	if err := events.UserDeleted(t.Context(), string([]byte{255})); err == nil || calls != 1 {
		t.Fatal(err, calls)
	}
}
