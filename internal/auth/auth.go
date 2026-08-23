// SPDX-License-Identifier: Apache-2.0

// Package auth provides Liftr's M11 authentication and authorization
// implementations for startup composition: an RFC 9068 JWT access-token
// verifier against a single OIDC issuer, the claim-to-membership mapper,
// and the owner-membership authorizer.
//
// The package imports only identity, domain, and the standard library. It
// never imports application, api/http, or persistence: concrete
// implementations satisfy those consumers' ports structurally, and
// composition proves satisfaction at startup (ADR-0012).
package auth

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// accessTokenTypes are the access-token media types accepted by the RFC 9068
// profile. Comparison is case-insensitive; anything else — including "JWT",
// ID-token typing, and absent typing — is refused so a differently typed but
// otherwise validly signed token can never cross-authenticate (ADR-0012).
var accessTokenTypes = []string{"at+jwt", "application/at+jwt"}

// supportedAlgorithms are the JWS algorithms this verifier can check.
// HMAC algorithms are deliberately absent: shared-secret verification invites
// algorithm-confusion bugs and has no deployment story in Liftr's model.
var supportedAlgorithms = map[string]struct{}{
	"RS256": {},
	"PS256": {},
	"ES256": {},
}

const (
	defaultClockSkew        = 30 * time.Second
	defaultJWKSRefreshEvery = 15 * time.Minute
	defaultFetchBytes       = 1 << 20
	defaultMaxJWKSKeys      = 64
	// minForcedRefreshInterval rate-limits refreshes triggered by unknown key
	// IDs so a flood of forged kid values cannot turn into unbounded JWKS
	// refetching (ADR-0012).
	minForcedRefreshInterval = 30 * time.Second
	fetchTimeout             = 10 * time.Second
)

// ErrInvalidCredentials is the only error surface of Authenticate. It never
// carries raw JWT/JWKS/library detail: transports render one curated problem
// for every credential failure (ADR-0012).
var ErrInvalidCredentials = errors.New("invalid credentials")

func invalid(reason string) error {
	return fmt.Errorf("%w (%s)", ErrInvalidCredentials, reason)
}

func validAccessTokenType(typ string) bool {
	for _, accepted := range accessTokenTypes {
		if strings.EqualFold(typ, accepted) {
			return true
		}
	}
	return false
}
