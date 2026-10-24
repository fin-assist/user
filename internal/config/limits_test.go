package config

import (
	"reflect"
	"testing"
	"time"

	"github.com/fin-assist/user/internal/ratelimit"
)

func TestUnitLimitDefaultsAndOverrides(t *testing.T) {
	get, list := env(base())
	c, err := load(get, noFiles, list)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Limits, ratelimit.Defaults()) {
		t.Fatalf("defaults: %v", c.Limits)
	}
	vars := base()
	vars["LIMIT_WATCH_IP"] = "9"
	vars["WINDOW_WATCH_IP"] = "2m"
	get, list = env(vars)
	c, err = load(get, noFiles, list)
	if err != nil {
		t.Fatal(err)
	}
	if c.Limits[ratelimit.WatchIP] != (ratelimit.Rule{Limit: 9, Window: 2 * time.Minute}) {
		t.Fatal(c.Limits)
	}
}

func TestUnitInvalidLimitConfiguration(t *testing.T) {
	for name, values := range map[string][]string{"LIMIT_WATCH_IP": {"0", "-1", "2147483648", "not-a-number"}, "WINDOW_WATCH_IP": {"0s", "-1s", "1ms", "24h1s", "1.5s", "bad"}} {
		for _, v := range values {
			vars := base()
			vars[name] = v
			get, list := env(vars)
			if _, err := load(get, noFiles, list); err == nil {
				t.Fatalf("%s=%s accepted", name, v)
			}
		}
	}
}
