package postgres

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/fin-assist/user/internal/ratelimit"
	"github.com/fin-assist/user/internal/token"
)

func TestUnitDecodeStartRecordWithoutDatabase(t *testing.T) {
	hash := token.Sum("logical request")
	start := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	windows := []ratelimit.Window{
		{Scope: ratelimit.VerificationPhoneShort, Subject: "+79001234567", Start: start, End: start.Add(time.Hour), Limit: 3},
		{Scope: ratelimit.VerificationPhoneLong, Subject: "+79001234567", Start: start, End: start.Add(24 * time.Hour), Limit: 10},
		{Scope: ratelimit.VerificationIPShort, Subject: "192.0.2.1", Start: start, End: start.Add(time.Minute), Limit: 30},
		{Scope: ratelimit.VerificationIPLong, Subject: "192.0.2.1", Start: start, End: start.Add(time.Hour), Limit: 300},
	}
	encoded, err := json.Marshal(windows)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data []byte
		want []ratelimit.Window
	}{
		{"unfinished", encoded, windows},
		{"completed", []byte("[]"), []ratelimit.Window{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decodeStartRecord(startKeyA, hash[:], tc.data)
			if err != nil || got.Key != startKeyA || got.Hash != hash || !reflect.DeepEqual(got.Windows, tc.want) {
				t.Fatalf("stored attempt changed: %+v %v", got, err)
			}
		})
	}
	for _, tc := range []struct {
		name          string
		hash, windows []byte
	}{
		{"missing_hash", nil, encoded},
		{"short_hash", hash[:31], encoded},
		{"long_hash", append(hash[:], 1), encoded},
		{"missing_windows", hash[:], nil},
		{"null_windows", hash[:], []byte("null")},
		{"object_windows", hash[:], []byte("{}")},
		{"malformed_windows", hash[:], []byte("[")},
		{"one_window", hash[:], []byte("[{}]")},
		{"three_windows", hash[:], []byte("[{},{},{}]")},
		{"five_windows", hash[:], []byte("[{},{},{},{},{}]")},
		{"invalid_time", hash[:], []byte(`[{"Start":"invalid"},{},{},{}]`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := decodeStartRecord(startKeyA, tc.hash, tc.windows); err == nil {
				t.Fatal("corrupt attempt accepted")
			}
		})
	}
}
