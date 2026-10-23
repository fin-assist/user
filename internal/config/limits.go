package config

import "github.com/fin-assist/user/internal/ratelimit"

func (r *reader) limits() ratelimit.Rules {
	rules := ratelimit.Rules{
		ratelimit.VerificationPhoneShort: {Limit: r.int("LIMIT_VERIFICATION_PHONE_SHORT"), Window: r.positiveDuration("WINDOW_VERIFICATION_PHONE_SHORT")},
		ratelimit.VerificationPhoneLong:  {Limit: r.int("LIMIT_VERIFICATION_PHONE_LONG"), Window: r.positiveDuration("WINDOW_VERIFICATION_PHONE_LONG")},
		ratelimit.VerificationIPShort:    {Limit: r.int("LIMIT_VERIFICATION_IP_SHORT"), Window: r.positiveDuration("WINDOW_VERIFICATION_IP_SHORT")},
		ratelimit.VerificationIPLong:     {Limit: r.int("LIMIT_VERIFICATION_IP_LONG"), Window: r.positiveDuration("WINDOW_VERIFICATION_IP_LONG")},
		ratelimit.WatchIP:                {Limit: r.int("LIMIT_WATCH_IP"), Window: r.positiveDuration("WINDOW_WATCH_IP")},
		ratelimit.RegisterIP:             {Limit: r.int("LIMIT_REGISTER_IP"), Window: r.positiveDuration("WINDOW_REGISTER_IP")},
		ratelimit.RefreshIP:              {Limit: r.int("LIMIT_REFRESH_IP"), Window: r.positiveDuration("WINDOW_REFRESH_IP")},
	}
	if err := rules.Validate(); err != nil {
		r.errs = append(r.errs, err)
	}
	return rules
}
