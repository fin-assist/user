package profile

import (
	"context"
	"errors"
	"testing"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"

	"github.com/fin-assist/go-grpc/reqctx"
)

type deletionFunc func(context.Context, string) error

func (f deletionFunc) Delete(ctx context.Context, id string) error { return f(ctx, id) }
func TestUnitDeletionIdentityAndErrors(t *testing.T) {
	calls := 0
	want := errors.New("database failure")
	h := &handler{deletions: deletionFunc(func(_ context.Context, id string) error {
		calls++
		if id != "signed-user" {
			t.Fatal(id)
		}
		if calls == 2 {
			return want
		}
		return nil
	})}
	if _, err := h.DeleteAccount(t.Context(), &userv1.DeleteAccountRequest{}); err == nil || calls != 0 {
		t.Fatal(err)
	} else if r, ok := finerr.ToREST(err); !ok || r.Code != "INVALID_TOKEN" {
		t.Fatal(err)
	}
	ctx := reqctx.WithUserID(t.Context(), "signed-user")
	if out, err := h.DeleteAccount(ctx, &userv1.DeleteAccountRequest{}); err != nil || out == nil {
		t.Fatal(out, err)
	}
	if _, err := h.DeleteAccount(ctx, &userv1.DeleteAccountRequest{}); !errors.Is(err, want) {
		t.Fatal(err)
	}
}
