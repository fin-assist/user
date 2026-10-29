package app

import (
	"testing"
	"time"
)

func TestUnitBusinessClockDefaultsAndTrustedOverride(t *testing.T) {
	a := &App{}
	before := time.Now()
	if got := a.clock()(); got.Before(before) || got.After(time.Now()) {
		t.Fatal("default clock is not current")
	}
	want := time.Date(2032, time.February, 29, 23, 59, 0, 0, time.UTC)
	limit := want.Add(-time.Hour)
	a.limitNow = func() time.Time { return limit }
	WithClock(func() time.Time { return want })(a)
	if got := a.clock()(); !got.Equal(want) || !a.limitNow().Equal(limit) {
		t.Fatal("business and limiter clocks are not independent")
	}
	WithClock(nil)(a)
	if got := a.clock()(); got.Before(before) || got.After(time.Now()) {
		t.Fatal("nil override did not restore the default")
	}
}
