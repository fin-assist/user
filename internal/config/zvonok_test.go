package config

import (
	"strings"
	"testing"
	"time"
)

func TestUnitZvonokConfigurationAndProdBoundary(t *testing.T) {
	vars := base()
	vars["PHONE_PROVIDER"] = "zvonok"
	vars["ZVONOK_PUBLIC_KEY"] = "private-test-key"
	vars["ZVONOK_CAMPAIGN_ID"] = "123"
	vars["ENV"], vars["AUTH_MODE"] = "dev", "insecure-dev"
	vars["ZVONOK_TRUSTED_PROXY_CIDRS"] = "10.0.0.0/24, 192.168.1.0/24"
	g, l := env(vars)
	cfg, err := load(g, noFiles, l)
	if err != nil || len(cfg.Zvonok.TrustedProxies) != 2 || cfg.Zvonok.PollInterval != 30*time.Second || cfg.Zvonok.CallbackAddr != ":9095" {
		t.Fatal(cfg.Zvonok, err)
	}
	if err = cfg.Zvonok.Validate(EnvProd); err == nil {
		t.Fatal("prod paid backend enabled")
	}
	for k, values := range map[string][]string{"PHONE_PROVIDER": {"unknown", "disabled"}, "ZVONOK_PUBLIC_KEY": {"", "bad\nkey"}, "ZVONOK_CAMPAIGN_ID": {"", "0", "-1", "a"}, "ZVONOK_POLL_INTERVAL": {"0s", "61s", "invalid"}, "ZVONOK_TRUSTED_PROXY_CIDRS": {"0.0.0.0/0", "bad"}} {
		for _, v := range values {
			copy := map[string]string{}
			for k, v := range vars {
				copy[k] = v
			}
			copy[k] = v
			g, l := env(copy)
			_, err = load(g, noFiles, l)
			if err == nil || strings.Contains(err.Error(), "private-test-key") {
				t.Fatal(k, v, err)
			}
		}
	}
}
