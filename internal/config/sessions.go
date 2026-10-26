package config

import (
	"errors"
	"io"
	"math"
	"time"

	"github.com/fin-assist/user/internal/secretfile"
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
	return loadAESKey(c.IdempotencyKeyFile, "auth")
}

func loadAESKey(path, purpose string) ([]byte, error) {
	if path == "" {
		return nil, errors.New(purpose + ": key file required")
	}
	// Owner-only, or a systemd credential readable through its ACL (secretfile.Open).
	f, _, err := secretfile.Open(path)
	if errors.Is(err, secretfile.ErrUnavailable) {
		return nil, errors.New(purpose + ": cannot open key file")
	}
	if err != nil {
		return nil, errors.New(purpose + ": key must be a regular owner-only file")
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 33))
	if err != nil || len(b) != 32 {
		return nil, errors.New(purpose + ": key must contain exactly 32 raw bytes")
	}
	return b, nil
}
