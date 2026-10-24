package postgres

import (
	"context"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	idempotency "github.com/fin-assist/go-idempotency"
	pgstore "github.com/fin-assist/go-postgres"
	"github.com/jackc/pgx/v5"

	"github.com/fin-assist/user/internal/auth"
	"github.com/fin-assist/user/internal/profile"
)

// SessionTransactions chooses the complete operation's mapper before idempotency,
// effects and COMMIT. Nested usecase transactions use the plain DB runner.
type SessionTransactions struct{ db *DB }

// NewSessionTransactions binds authentication's outer transaction to its pool.
func NewSessionTransactions(db *DB) SessionTransactions { return SessionTransactions{db} }

// Run selects the mapper from the idempotency adapter's verified operation context.
func (r SessionTransactions) Run(ctx context.Context, fn func(context.Context) error) error {
	request, ok := idempotency.FromContext(ctx)
	if !ok {
		return r.db.Run(ctx, fn)
	}
	var mapper pgstore.ErrorMapper
	switch request.Endpoint {
	case userv1.AuthService_Register_FullMethodName:
		mapper = registrationViolation
	case userv1.AuthService_RefreshTokens_FullMethodName:
		mapper = refreshViolation
	}
	return r.db.RunMapped(ctx, pgx.TxOptions{}, mapper, fn)
}
func registrationViolation(err error) error {
	if pgstore.IsViolation(err, "23503", "user_consent_document_fkey") {
		return auth.ErrConsentOutdated
	}
	if pgstore.IsViolation(err, "23503", "user_consent_user_fkey") {
		return auth.ErrMissing
	}
	return refreshViolation(err)
}
func refreshViolation(err error) error {
	for _, name := range []string{"auth_session_user_fkey", "auth_session_phone_verification_fkey", "phone_verification_user_fkey"} {
		if pgstore.IsViolation(err, "23503", name) {
			return auth.ErrMissing
		}
	}
	return nil
}

// ProfileTransactions keeps ordinary profile work unmapped. Only assistant-consent
// grants select the grant mapper, so another operation cannot inherit its refusals.
type ProfileTransactions struct{ *DB }

// NewProfileTransactions binds profile operations to the ordinary service pool.
func NewProfileTransactions(db *DB) ProfileTransactions { return ProfileTransactions{db} }

// RunGrant covers all grant statements and their actual COMMIT.
func (r ProfileTransactions) RunGrant(ctx context.Context, fn func(context.Context) error) error {
	return r.RunMapped(ctx, pgx.TxOptions{}, grantViolation, fn)
}
func grantViolation(err error) error {
	if pgstore.IsViolation(err, "23503", "user_consent_user_fkey") {
		return profile.ErrConsentUserMissing
	}
	if pgstore.IsViolation(err, "23503", "user_consent_document_fkey") {
		return profile.ErrConsentOutdated
	}
	if pgstore.IsViolation(err, "23505", "user_consent_active_key") {
		return profile.ErrConsentConflict
	}
	return nil
}
