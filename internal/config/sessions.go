package config

import (
	"errors"
	"io"
	"math"
	"os"
	"time"
)

// SessionsConfig configures the shared encrypted replay and sliding refresh lifetime.
type SessionsConfig struct {
	IdempotencyKeyFile string
	RefreshTTL         time.Duration
	MaxConns           int32
}

func (r *reader) sessions() SessionsConfig {
	n := r.int("AUTH_DB_MAX_CONNS")
	if n < 1 || n > math.MaxInt32 {
		r.errs = append(r.errs, errors.New("AUTH_DB_MAX_CONNS must be a positive int32"))
		n = 1
	}
	return SessionsConfig{IdempotencyKeyFile: r.str("AUTH_IDEMPOTENCY_KEY_FILE"), RefreshTTL: r.positiveDuration("REFRESH_TOKEN_TTL"), MaxConns: int32(n)}
}

// LoadSessionKey reads exactly one AES-256 key. No error includes key bytes.
func LoadSessionKey(c SessionsConfig) ([]byte, error) {
	if c.RefreshTTL <= 0 || c.MaxConns < 1 || c.IdempotencyKeyFile == "" {
		return nil, errors.New("auth: key file, positive refresh TTL and pool size required")
	}
	f, err := os.Open(c.IdempotencyKeyFile) //nolint:gosec // Operator-supplied secret path, never request input.
	if err != nil {
		return nil, errors.New("auth: cannot open idempotency key file")
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("auth: key must be a regular owner-only file")
	}
	b, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil || len(b) != 32 {
		return nil, errors.New("auth: idempotency key must contain exactly 32 raw bytes")
	}
	return b, nil
}
