// Package signing issues user access tokens and publishes only public signing keys.
package signing

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Options pins the active key and the public keys kept during rotation. Files are
// loaded once at startup; changing keys requires a coordinated restart.
type Options struct {
	PrivateKeyFile, PublicKeysFile, KeyID string
	Issuer, Audience                      string
	Clock                                 func() time.Time
}

// Keys is an immutable signing key and its public JWKS snapshot.
type Keys struct {
	options Options
	private ed25519.PrivateKey
	public  []byte
}

type publicKey struct {
	Type       string   `json:"kty"`
	Curve      string   `json:"crv"`
	X          string   `json:"x"`
	ID         string   `json:"kid"`
	Algorithm  string   `json:"alg,omitempty"`
	Use        string   `json:"use,omitempty"`
	Operations []string `json:"key_ops,omitempty"`
}

type keySet struct {
	Keys []publicKey `json:"keys"`
}

// Load reads a bounded PKCS#8 Ed25519 private key and optional public JWKS. Unknown
// JWK members (including private material) are rejected, never republished.
func Load(o Options) (*Keys, error) {
	if o.Clock == nil || strings.TrimSpace(o.Issuer) == "" || strings.TrimSpace(o.Audience) == "" || strings.TrimSpace(o.KeyID) == "" || len(o.KeyID) > 256 {
		return nil, errors.New("signing: issuer, audience, key ID and clock required")
	}
	b, err := readFile(o.PrivateKeyFile, 8192, true)
	if err != nil {
		return nil, err
	}
	block, rest := pem.Decode(b)
	if block == nil || block.Type != "PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("signing: invalid PKCS#8 key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	private, ok := parsed.(ed25519.PrivateKey)
	if err != nil || !ok {
		return nil, errors.New("signing: Ed25519 key required")
	}
	active := publicKey{Type: "OKP", Curve: "Ed25519", X: base64.RawURLEncoding.EncodeToString(private.Public().(ed25519.PublicKey)), ID: o.KeyID, Algorithm: "EdDSA", Use: "sig"}
	set := keySet{Keys: []publicKey{active}}
	if o.PublicKeysFile != "" {
		other, err := loadPublic(o.PublicKeysFile)
		if err != nil {
			return nil, err
		}
		for _, key := range other.Keys {
			if key.ID == active.ID {
				if key.X != active.X {
					return nil, errors.New("signing: active public key mismatch")
				}
				continue
			}
			set.Keys = append(set.Keys, key)
		}
	}
	if len(set.Keys) > 64 {
		return nil, errors.New("signing: too many public keys")
	}
	public, err := json.Marshal(set)
	if err != nil {
		return nil, errors.New("signing: cannot encode public keys")
	}
	return &Keys{options: o, private: private, public: public}, nil
}

// loadPublic reads trusted Ed25519 public JWKS. It is also used for stage service
// keys, whose key IDs must be their service names (checked by go-identity).
func loadPublic(path string) (keySet, error) {
	b, err := readFile(path, 1<<20, false)
	if err != nil {
		return keySet{}, err
	}
	var set keySet
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	if err := d.Decode(&set); err != nil {
		return keySet{}, errors.New("signing: invalid public JWKS")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return keySet{}, errors.New("signing: trailing JWKS data")
	}
	if len(set.Keys) == 0 || len(set.Keys) > 64 {
		return keySet{}, errors.New("signing: public key count outside 1..64")
	}
	seen := map[string]bool{}
	for _, key := range set.Keys {
		x, err := base64.RawURLEncoding.DecodeString(key.X)
		if err != nil || len(x) != ed25519.PublicKeySize || key.Type != "OKP" || key.Curve != "Ed25519" || strings.TrimSpace(key.ID) == "" || len(key.ID) > 256 || seen[key.ID] || (key.Algorithm != "" && key.Algorithm != "EdDSA") || (key.Use != "" && key.Use != "sig") {
			return keySet{}, errors.New("signing: invalid public key")
		}
		for _, op := range key.Operations {
			if op != "verify" {
				return keySet{}, errors.New("signing: invalid public key operation")
			}
		}
		seen[key.ID] = true
	}
	return set, nil
}

func readFile(path string, maxBytes int64, secret bool) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // Operator-supplied trusted key path, never request input.
	if err != nil {
		return nil, errors.New("signing: key file unavailable")
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBytes || (secret && info.Mode().Perm()&0o077 != 0) {
		return nil, errors.New("signing: invalid key file or permissions")
	}
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil || int64(len(b)) > maxBytes {
		return nil, errors.New("signing: invalid key file")
	}
	return b, nil
}

// Sign creates an Ed25519 access token valid for exactly 15 minutes.
func (k *Keys) Sign(ctx context.Context, userID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if strings.TrimSpace(userID) == "" || len(userID) > 256 {
		return "", errors.New("signing: invalid subject")
	}
	now := k.options.Clock().UTC().Truncate(time.Second)
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.RegisteredClaims{Issuer: k.options.Issuer, Subject: userID, Audience: jwt.ClaimStrings{k.options.Audience}, IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(15 * time.Minute))})
	token.Header["kid"] = k.options.KeyID
	return token.SignedString(k.private)
}

// JWKS implements the local provider used by go-identity without a self-RPC.
func (k *Keys) JWKS(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]byte(nil), k.public...), nil
}
