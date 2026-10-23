package consent

import (
	"context"
	"errors"
	"testing"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/grpc/metadata"

	"github.com/fin-assist/user/internal/adapter/grpc/server"
	"github.com/fin-assist/user/internal/consent"
)

type fakeDocuments struct {
	calls int
	err   error
}

func (f *fakeDocuments) Current(_ context.Context, kind string) (consent.Document, error) {
	f.calls++
	return consent.Document{Type: kind, Version: "v1", Title: "Consent", URL: "https://example.test/legal"}, f.err
}

type fakeUsers struct {
	id  string
	err error
}

func (f fakeUsers) VerifyUser(context.Context, string) (string, error) { return f.id, f.err }

type fakeDeleted struct {
	deleted bool
	err     error
}

func (f fakeDeleted) Deleted(context.Context, string) (bool, error) { return f.deleted, f.err }

func TestDocumentAuthorization(t *testing.T) {
	for _, tt := range []struct {
		name    string
		token   []string
		users   fakeUsers
		deleted fakeDeleted
		reason  string
		fail    bool
	}{
		{name: "missing", reason: "INVALID_TOKEN"},
		{name: "duplicate", token: []string{"Bearer valid", "Bearer other"}, reason: "INVALID_TOKEN"},
		{name: "scheme", token: []string{"Basic value"}, reason: "INVALID_TOKEN"},
		{name: "empty token", token: []string{"Bearer "}, reason: "INVALID_TOKEN"},
		{name: "bad token", token: []string{"Bearer bad"}, users: fakeUsers{err: finerr.InvalidToken()}, reason: "INVALID_TOKEN"},
		{name: "empty subject", token: []string{"Bearer valid"}, reason: "INVALID_TOKEN"},
		{name: "deleted", token: []string{"Bearer valid"}, users: fakeUsers{id: "u"}, deleted: fakeDeleted{deleted: true}, reason: "TOKEN_REVOKED"},
		{name: "storage error", token: []string{"Bearer valid"}, users: fakeUsers{id: "u"}, deleted: fakeDeleted{err: errors.New("DB")}, fail: true},
		{name: "accepted", token: []string{"bEaReR valid"}, users: fakeUsers{id: "u"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo := &fakeDocuments{}
			h := &handler{documents: repo, users: tt.users, deleted: tt.deleted}
			md := metadata.MD{}
			md.Set("authorization", tt.token...)
			got, err := h.GetConsentDocument(metadata.NewIncomingContext(t.Context(), md), &userv1.GetConsentDocumentRequest{Type: userv1.ConsentType_CONSENT_TYPE_ASSISTANT})
			if tt.reason != "" || tt.fail {
				if err == nil || (!tt.fail && server.Reason(err) != tt.reason) || repo.calls != 0 {
					t.Fatalf("got=%v err=%v calls=%d", got, err, repo.calls)
				}
			} else if err != nil || got.GetDocument().GetVersion() != "v1" || repo.calls != 1 {
				t.Fatalf("%v %v", got, err)
			}
		})
	}
}

func TestPublicUnknownAndStorageError(t *testing.T) {
	repo := &fakeDocuments{}
	h := &handler{documents: repo}
	if _, err := h.GetConsentDocument(t.Context(), &userv1.GetConsentDocumentRequest{Type: userv1.ConsentType_CONSENT_TYPE_PERSONAL_DATA}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.GetConsentDocument(t.Context(), &userv1.GetConsentDocumentRequest{Type: userv1.ConsentType(123)}); server.Reason(err) != "NOT_FOUND" {
		t.Fatalf("%v", err)
	}
	repo.err = errors.New("repository")
	if _, err := h.GetConsentDocument(t.Context(), &userv1.GetConsentDocumentRequest{Type: userv1.ConsentType_CONSENT_TYPE_PERSONAL_DATA}); !errors.Is(err, repo.err) {
		t.Fatalf("%v", err)
	}
}
