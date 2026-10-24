package consent

import (
	"context"
	"errors"
	"testing"

	"github.com/fin-assist/user/internal/adapter/grpc/server"
)

type documents struct {
	calls    int
	document Document
	err      error
}

func (d *documents) Current(context.Context, string) (Document, error) {
	d.calls++
	return d.document, d.err
}

func TestUnitCurrentDocument(t *testing.T) {
	for _, kind := range []string{"personal_data", "assistant"} {
		repo := &documents{document: Document{Type: kind, Version: "v1", Title: "Consent", URL: "https://example.test/consent"}}
		got, err := New(repo).Current(t.Context(), kind)
		if err != nil || got != repo.document || repo.calls != 1 {
			t.Fatalf("%s: %+v %v", kind, got, err)
		}
	}
	repo := &documents{}
	if _, err := New(repo).Current(t.Context(), "unknown"); server.Reason(err) != "NOT_FOUND" {
		t.Fatal("unknown type accepted")
	}
	if repo.calls != 0 {
		t.Fatal("unknown type queried")
	}
	want := errors.New("storage unavailable")
	repo.err = want
	if _, err := New(repo).Current(t.Context(), "personal_data"); !errors.Is(err, want) {
		t.Fatalf("error lost: %v", err)
	}
}
