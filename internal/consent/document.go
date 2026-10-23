// Package consent reads versioned legal documents used by sign-in and the assistant.
package consent

import (
	"context"

	"github.com/fin-assist/contracts/gen/go/finerr"
)

// Document is a published version of a consent document.
type Document struct{ Type, Version, Title, URL string }

// Documents reads current documents from service-owned storage.
type Documents interface {
	Current(context.Context, string) (Document, error)
}

// Service reads the current legal document; publishing versions is a data migration.
type Service struct{ documents Documents }

// New uses the service's document repository.
func New(documents Documents) *Service { return &Service{documents: documents} }

// Current returns NOT_FOUND for unknown types, without querying storage.
func (s *Service) Current(ctx context.Context, kind string) (Document, error) {
	if kind != "personal_data" && kind != "assistant" {
		return Document{}, finerr.NotFound()
	}
	return s.documents.Current(ctx, kind)
}
