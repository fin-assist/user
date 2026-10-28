package config

import (
	"errors"
	"regexp"
	"time"
)

// PhoneConfig bounds pending streams and holds the independent result-encryption key.
type PhoneConfig struct {
	ResultKeyFile, ResultKeyID string
	MaxStreams                 int
	ResyncInterval             time.Duration
	UnknownTimeout             time.Duration
}

func (r *reader) phone() PhoneConfig {
	c := PhoneConfig{ResultKeyFile: r.str("PHONE_RESULT_KEY_FILE"), ResultKeyID: r.str("PHONE_RESULT_KEY_ID"), MaxStreams: r.int("PHONE_WATCH_MAX_STREAMS"), ResyncInterval: r.positiveDuration("PHONE_WATCH_RESYNC_INTERVAL"), UnknownTimeout: r.positiveDuration("PHONE_VERIFICATION_UNKNOWN_TIMEOUT")}
	if err := c.Validate(); err != nil {
		r.errs = append(r.errs, err)
	}
	return c
}
func (c PhoneConfig) Validate() error {
	if !regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`).MatchString(c.ResultKeyID) || c.MaxStreams < 1 || c.MaxStreams > 100000 || c.ResyncInterval <= 0 || c.ResyncInterval > 30*time.Second || c.UnknownTimeout < time.Second || c.UnknownTimeout > 5*time.Minute {
		return errors.New("phone: invalid key ID, stream capacity or resync interval")
	}
	return nil
}
func LoadPhoneKey(c PhoneConfig) ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return loadAESKey(c.ResultKeyFile, "phone result")
}
