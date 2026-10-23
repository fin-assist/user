package signing

import (
	"context"
	"errors"
	"testing"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"

	"github.com/fin-assist/user/internal/profile"
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

type consents struct {
	err error
	id  string
}

func (c *consents) Status(_ context.Context, id string) (profile.Consent, error) {
	c.id = id
	return profile.Consent{Granted: true, CurrentVersion: "v2"}, c.err
}
func TestAssistantConsentStatus(t *testing.T) {
	c := &consents{}
	h := &handler{consents: c}
	id := "system-user"
	r, err := h.GetAssistantConsentStatus(t.Context(), &userv1.GetAssistantConsentStatusRequest{UserId: &id})
	if err != nil || !r.GetConsent().GetGranted() || c.id != id || r.GetConsent().GetCurrentVersion() != "v2" {
		t.Fatalf("%v %v", r, err)
	}
	c.err = errors.New("consent lookup failed")
	if _, err := h.GetAssistantConsentStatus(t.Context(), &userv1.GetAssistantConsentStatusRequest{}); !errors.Is(err, c.err) {
		t.Fatal(err)
	}
}
