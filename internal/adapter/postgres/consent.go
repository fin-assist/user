package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/fin-assist/contracts/gen/go/finerr"
	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/user/internal/consent"
)

// ConsentDocuments reads current documents on primary, where their versions switch.
type ConsentDocuments struct{ db *DB }

// NewConsentDocuments uses the same primary pool as registration and consent writes.
func NewConsentDocuments(db *DB) *ConsentDocuments { return &ConsentDocuments{db: db} }

// Current returns the current stored version without inventing a default document.
func (r *ConsentDocuments) Current(ctx context.Context, kind string) (consent.Document, error) {
	d := consent.Document{Type: kind}
	err := r.db.Writer(ctx).QueryRow(ctx, `SELECT version,title,url FROM consent_document WHERE type=$1 AND is_current`, kind).Scan(&d.Version, &d.Title, &d.URL)
	if errors.Is(err, pgx.ErrNoRows) {
		return consent.Document{}, finerr.NotFound()
	}
	if err != nil {
		return consent.Document{}, fmt.Errorf("read current consent document: %w", err)
	}
	return d, nil
}
