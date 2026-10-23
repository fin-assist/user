package signing

import (
	"context"
	"errors"
	"testing"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
)

type keys struct{ err error }

func (k keys) JWKS(context.Context) ([]byte, error) { return []byte(`{"keys":[]}`), k.err }

func TestSigningKeys(t *testing.T) {
	h := &handler{keys: keys{}}
	got, err := h.GetSigningKeys(t.Context(), &userv1.GetSigningKeysRequest{})
	if err != nil || got.GetJwksJson() != `{"keys":[]}` {
		t.Fatalf("%v %v", got, err)
	}
	want := errors.New("keys")
	h.keys = keys{err: want}
	if _, err := h.GetSigningKeys(t.Context(), &userv1.GetSigningKeysRequest{}); !errors.Is(err, want) {
		t.Fatal(err)
	}
}
