package verificationcrypto

import (
	"bytes"
	"testing"

	"github.com/fin-assist/user/internal/auth"
	"github.com/fin-assist/user/internal/token"
	"github.com/fin-assist/user/internal/verification"
)

func TestResultCipherAuthenticatedBindingAndKeySeparation(t *testing.T) {
	c, err := New(bytes.Repeat([]byte{42}, 32), "v1")
	if err != nil {
		t.Fatal(err)
	}
	r := verification.Request{ID: 7, Hash: token.Sum("secret")}
	want := verification.Outcome{State: verification.Verified, IsNewUser: true, RegistrationToken: "registration-secret"}
	r.Ciphertext, r.KeyID, err = c.Seal(r, want)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(r.Ciphertext, []byte(want.RegistrationToken)) {
		t.Fatal("plaintext stored")
	}
	if got, err := c.Open(r); err != nil || got != want {
		t.Fatal(got, err)
	}
	second, _, err := c.Seal(r, want)
	if err != nil || bytes.Equal(second, r.Ciphertext) {
		t.Fatal("nonce reused", err)
	}
	for _, mutate := range []func(*verification.Request){func(r *verification.Request) { r.ID++ }, func(r *verification.Request) { r.Hash = token.Sum("foreign") }, func(r *verification.Request) { r.KeyID = "old-key" }, func(r *verification.Request) { r.Ciphertext = []byte{1} }, func(r *verification.Request) { r.Ciphertext = make([]byte, 16449) }, func(r *verification.Request) { b := bytes.Clone(r.Ciphertext); b[len(b)-1] ^= 1; r.Ciphertext = b }} {
		bad := r
		mutate(&bad)
		if _, err := c.Open(bad); err == nil {
			t.Fatal("invalid envelope accepted")
		}
	}
	other, _ := New(bytes.Repeat([]byte{1}, 32), "v1")
	if _, err := other.Open(r); err == nil {
		t.Fatal("wrong key accepted")
	}
	session := verification.Outcome{State: verification.Verified, UserID: "user", Pair: auth.Pair{Access: "access", Refresh: "refresh"}}
	r.Ciphertext, r.KeyID, err = c.Seal(r, session)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.Open(r); err != nil || got != session {
		t.Fatal(got, err)
	}
}
func TestResultCipherRejectsMalformedConfigurationAndEncodings(t *testing.T) {
	for _, key := range [][]byte{nil, make([]byte, 16), make([]byte, 33)} {
		if _, err := New(key, "v1"); err == nil {
			t.Fatal("bad key accepted")
		}
	}
	for _, id := range []string{"", "bad/id", "with spaces"} {
		if _, err := New(make([]byte, 32), id); err == nil {
			t.Fatal("bad ID accepted")
		}
	}
	c, _ := New(make([]byte, 32), "v1")
	if _, _, err := c.Seal(verification.Request{}, verification.Outcome{}); err == nil {
		t.Fatal("no request binding")
	}
	r := verification.Request{ID: 1, Hash: token.Sum("secret"), KeyID: "v1"}
	for _, body := range []string{"broken", `{"unknown":"field"}`, `{} {}`} {
		nonce := make([]byte, c.aead.NonceSize())
		r.Ciphertext = c.aead.Seal(nonce, nonce, []byte(body), aad(r))
		if _, err := c.Open(r); err == nil {
			t.Fatal("malformed plaintext accepted")
		}
	}
}

func TestResultCipherBoundsOutputSize(t *testing.T) {
	c, _ := New(make([]byte, 32), "v1")
	if _, _, err := c.Seal(verification.Request{ID: 1, Hash: token.Sum("secret")}, verification.Outcome{RegistrationToken: string(bytes.Repeat([]byte{'x'}, 20000))}); err == nil {
		t.Fatal("unbounded result accepted")
	}
}
