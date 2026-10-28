package apitest_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/fin-assist/user/internal/app/apitest"
	"github.com/fin-assist/user/internal/verification"
)

type malformedCreationProvider struct {
	mu   sync.Mutex
	mode string
}

func (*malformedCreationProvider) Name() string { return "test" }
func (p *malformedCreationProvider) Create(_ context.Context, a verification.Attempt) (verification.Call, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c := verification.Call{ID: "call:" + strconv.Itoa(int(a.ID)), Phone: a.Phone, StartedAt: a.FirstDispatchAt, ReceivedAt: time.Now().UTC(), State: verification.Pending, Campaign: a.Campaign, Numbers: []string{"+74951234567"}}
	switch p.mode {
	case "empty-id":
		c.ID = ""
	case "foreign-phone":
		c.Phone = "+79007654321"
	case "future-start":
		c.StartedAt = time.Now().Add(time.Minute)
	case "expired-start":
		c.StartedAt = a.CreatedAt.Add(-verification.RequestTTL)
	case "empty-pool":
		c.Numbers = nil
	case "bad-number":
		c.Numbers = []string{"not-a-phone"}
	case "nondigit-number":
		c.Numbers = []string{"+7notdigits"}
	case "large-pool":
		c.Numbers = make([]string, verification.MaxCallbackPoolNumbers+1)
	}
	return c, nil
}
func (p *malformedCreationProvider) Recover(ctx context.Context, a verification.Attempt) (verification.Call, error) {
	return p.Create(ctx, a)
}
func (p *malformedCreationProvider) healthy() { p.mu.Lock(); defer p.mu.Unlock(); p.mode = "" }

func TestAPIProviderMalformedRepliesCannotPublishVerificationSecrets(t *testing.T) {
	for _, mode := range []string{"empty-id", "foreign-phone", "future-start", "expired-start", "empty-pool", "bad-number", "nondigit-number", "large-pool"} {
		t.Run(mode, func(t *testing.T) {
			p := &malformedCreationProvider{mode: mode}
			env := apitest.StartWithPhone(t, p, creationSelector{})
			seedDocuments(t, env)
			key, phone := authKey(), "+79001234567"
			got, err := startCall(t, env, key, phone)
			if err == nil || got != nil {
				t.Fatal("malformed provider result exposed", got, err)
			}
			var exposed int
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT count(*) FROM phone_verification WHERE provider_call_id IS NOT NULL OR verified_at IS NOT NULL`).Scan(&exposed); err != nil || exposed != 0 {
				t.Fatal(exposed, err)
			}
			p.healthy()
			retry, err := startCall(t, env, key, phone)
			if err != nil || retry.GetVerificationToken() == "" {
				t.Fatal("owned uncertain attempt cannot recover", retry, err)
			}
			if hits := startHits(t, env); hits != 1 {
				t.Fatal("retry charged second attempt", hits)
			}
		})
	}
}
