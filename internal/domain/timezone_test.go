package domain

import (
	"strings"
	"testing"
)

func TestUnitTimezoneContract(t *testing.T) {
	for _, s := range []string{"Europe/Moscow", "Asia/Kathmandu", "UTC", "Etc/GMT+12"} {
		if !ValidTimezone(s) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"", "Local", "../UTC", "/etc/passwd", "not-a-zone", strings.Repeat("x", 65)} {
		if ValidTimezone(s) {
			t.Fatal(s)
		}
	}
}
