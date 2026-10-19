package server

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/fin-assist/contracts/gen/go/finerr"
)

// ServiceVerifier checks the ServiceAccount token from x-service-authorization and returns
// the caller name (grpc-api.md 3.3). Any error refuses the call with a bare
// UNAUTHENTICATED: internal refusals carry no ErrorInfo (5.1, rule 3).
//
// The implementation comes from the identity library (FIN-119): local check against the
// JWKS of the API server — signature, exp, iss, aud finassist-internal.
type ServiceVerifier interface {
	VerifyService(ctx context.Context, token string) (caller string, err error)
}

// UserVerifier checks the user access token from authorization and returns its sub. Its
// errors reach the client, so they are registry errors: finerr.InvalidToken(),
// finerr.TokenExpired(), finerr.TokenRevoked(). Implementation — FIN-119 (Ed25519, keys
// from user.UserInternalService/GetSigningKeys).
type UserVerifier interface {
	VerifyUser(ctx context.Context, token string) (userID string, err error)
}

// Tombstones answers whether a user was deleted (FIN-110). A user call of a deleted user
// gets TOKEN_REVOKED (grpc-api.md 7, «Tombstone в каждом сервисе данных»): otherwise a
// still valid access token could write data back after PurgeUserData.
type Tombstones interface {
	IsDeleted(ctx context.Context, userID string) (bool, error)
}

// NoTombstones is for a service that stores no user data. A data service must not use it.
type NoTombstones struct{}

// IsDeleted always reports false.
func (NoTombstones) IsDeleted(context.Context, string) (bool, error) { return false, nil }

// devToken is the shape of a dev token: "dev:<name>".
var devToken = regexp.MustCompile(`^dev:([a-z0-9][a-z0-9-]{0,62})$`)

// DevServiceVerifier trusts "dev:<caller>" as the caller name. Only for AUTH_MODE=insecure-dev,
// which config allows only with ENV=dev.
type DevServiceVerifier struct{}

// VerifyService parses a dev token.
func (DevServiceVerifier) VerifyService(_ context.Context, token string) (string, error) {
	m := devToken.FindStringSubmatch(token)
	if m == nil {
		return "", errors.New("not a dev token")
	}
	return m[1], nil
}

// DevUserVerifier trusts "dev:<user_id>". Only for AUTH_MODE=insecure-dev.
type DevUserVerifier struct{}

// VerifyUser parses a dev token.
func (DevUserVerifier) VerifyUser(_ context.Context, token string) (string, error) {
	m := devToken.FindStringSubmatch(token)
	if m == nil {
		return "", finerr.InvalidToken()
	}
	return m[1], nil
}

// bearer extracts the token of "Bearer <token>"; the scheme is case-insensitive.
func bearer(v string) (string, bool) {
	scheme, token, ok := strings.Cut(v, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}
