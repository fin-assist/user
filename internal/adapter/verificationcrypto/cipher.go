// Package verificationcrypto encrypts re-deliverable phone results with an independent key.
package verificationcrypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"

	"github.com/fin-assist/user/internal/verification"
)

// Cipher is immutable. Keep the key and its ID until every 15-minute result expires.
type Cipher struct {
	aead cipher.AEAD
	id   string
}

func New(key []byte, id string) (*Cipher, error) {
	if len(key) != 32 || !regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`).MatchString(id) {
		return nil, errors.New("verification cipher: AES-256 key and bounded key ID required")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead, id: id}, nil
}
func aad(r verification.Request) []byte {
	b := strconv.AppendInt([]byte("user/phone-result/v1/"), int64(r.ID), 10)
	return append(b, r.Hash[:]...)
}

// Seal never chooses an account or changes a result; only the confirmation usecase does.
func (c *Cipher) Seal(r verification.Request, out verification.Outcome) ([]byte, string, error) {
	if r.ID < 1 {
		return nil, "", errors.New("verification cipher: invalid request ID")
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, "", err
	}
	if len(body) > 16384 {
		return nil, "", errors.New("verification cipher: result too large")
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, "", errors.New("verification cipher: randomness unavailable")
	}
	return c.aead.Seal(nonce, nonce, body, aad(r)), c.id, nil
}

// Open refuses wrong-key, modified and cross-request results without exposing bytes.
func (c *Cipher) Open(r verification.Request) (verification.Outcome, error) {
	var out verification.Outcome
	if r.ID < 1 || r.KeyID != c.id || len(r.Ciphertext) < c.aead.NonceSize()+c.aead.Overhead() || len(r.Ciphertext) > 16448 {
		return out, errors.New("verification cipher: invalid result envelope")
	}
	n := c.aead.NonceSize()
	body, err := c.aead.Open(nil, r.Ciphertext[:n], r.Ciphertext[n:], aad(r))
	if err != nil {
		return out, errors.New("verification cipher: result authentication failed")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&out); err != nil {
		return verification.Outcome{}, errors.New("verification cipher: invalid result encoding")
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return verification.Outcome{}, errors.New("verification cipher: trailing result data")
	}
	return out, nil
}
