// Package token creates opaque authentication secrets and their lookup hashes.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
)

// Hash is a SHA-256 lookup key; repositories cannot accept a truncated hash.
type Hash [sha256.Size]byte

// New returns 256 random bits, encoded as unpadded base64url.
func New() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// Sum hashes the entire secret, without a data-dependent prefix comparison.
func Sum(secret string) Hash { return sha256.Sum256([]byte(secret)) }
