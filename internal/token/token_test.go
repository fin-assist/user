package token

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestOpaqueSecretsHave256BitsAndStableWholeHash(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		s, err := New()
		if err != nil {
			t.Fatal(err)
		}
		b, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil || len(b) != 32 || len(s) != 43 || seen[s] {
			t.Fatal("invalid or repeated random token")
		}
		seen[s] = true
		if Sum(s) != Hash(sha256.Sum256([]byte(s))) || Sum(s) == Sum(s+"x") {
			t.Fatal("hash does not cover entire token")
		}
	}
}
