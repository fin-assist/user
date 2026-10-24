package config

import (
	"errors"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

// ZvonokConfig selects the real provider and its separate ingress listener.
// Prod activation is unavailable until the FIN-81 budget gate lands.
type ZvonokConfig struct {
	Provider, PublicKey, CampaignID, CallbackAddr string
	TrustedProxies                                []netip.Prefix
	PollInterval                                  time.Duration
}

func (r *reader) zvonok() ZvonokConfig {
	c := ZvonokConfig{Provider: r.str("PHONE_PROVIDER"), PublicKey: r.secret("ZVONOK_PUBLIC_KEY"), CampaignID: r.str("ZVONOK_CAMPAIGN_ID"), CallbackAddr: r.str("ZVONOK_CALLBACK_ADDR"), PollInterval: r.positiveDuration("ZVONOK_POLL_INTERVAL")}
	for _, raw := range strings.Split(r.str("ZVONOK_TRUSTED_PROXY_CIDRS"), ",") {
		if raw == "" {
			continue
		}
		p, err := netip.ParsePrefix(strings.TrimSpace(raw))
		if err != nil || p.Bits() == 0 {
			r.errs = append(r.errs, errors.New("zvonok: invalid trusted proxy CIDR"))
			continue
		}
		c.TrustedProxies = append(c.TrustedProxies, p.Masked())
	}
	return c
}

// Validate enforces credentials and the agreed prod activation boundary.
func (c ZvonokConfig) Validate(env Env) error {
	if c.Provider == "" || c.Provider == "disabled" {
		if c.PublicKey != "" || c.CampaignID != "" {
			return errors.New("zvonok: credentials configured for a disabled provider")
		}
		return nil
	}
	if c.Provider != "zvonok" {
		return errors.New("phone: unknown provider")
	}
	if env == EnvProd {
		return errors.New("zvonok: prod activation requires FIN-81 budget protection")
	}
	if len(c.PublicKey) < 1 || len(c.PublicKey) > 4096 || strings.ContainsAny(c.PublicKey, "\r\n") || !regexp.MustCompile(`^[1-9][0-9]{0,19}$`).MatchString(c.CampaignID) || c.CallbackAddr == "" || c.PollInterval < time.Second || c.PollInterval > time.Minute {
		return errors.New("zvonok: invalid credentials, campaign, listener or polling interval")
	}
	return nil
}
