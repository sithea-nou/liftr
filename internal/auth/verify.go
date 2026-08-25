// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/sithea-nou/liftr/internal/identity"
)

// Authenticate verifies one bearer credential against the RFC 9068 JWT
// access-token profile and returns the normalized principal, or
// ErrInvalidCredentials. Every failure — wrong typing, bad signature,
// expired, wrong issuer or audience — is indistinguishable to callers.
//
// Required profile: typ at+jwt (or application/at+jwt), configured algorithm,
// exact issuer, Liftr audience present in aud, exp present and valid, iat
// present (RFC 9068 requires it), nbf honored when present, non-empty sub,
// and non-empty client_id and jti as required by RFC 9068 section 2.2. jti is
// token-profile validation only: it is never persisted, logged, or carried
// into principals or events (ADR-0012).
func (a *OIDCAuthenticator) Authenticate(ctx context.Context, credential string) (identity.Principal, error) {
	principal, err := a.authenticate(ctx, credential)
	if a.observers.Authentication != nil {
		if err != nil {
			a.observers.Authentication(false, failureReasonOf(err))
		} else {
			a.observers.Authentication(true, identity.AuthFailureNone)
		}
	}
	return principal, err
}

// authenticate verifies one bearer credential against the RFC 9068 JWT
// access-token profile and returns the normalized principal, or
// ErrInvalidCredentials. Every failure — wrong typing, bad signature,
// expired, wrong issuer or audience — is indistinguishable to callers.
//
// Required profile: typ at+jwt (or application/at+jwt), configured algorithm,
// exact issuer, Liftr audience present in aud, exp present and valid, iat
// present (RFC 9068 requires it), nbf honored when present, non-empty sub,
// and non-empty client_id and jti as required by RFC 9068 section 2.2. jti is
// token-profile validation only: it is never persisted, logged, or carried
// into principals or events (ADR-0012).
func (a *OIDCAuthenticator) authenticate(ctx context.Context, credential string) (identity.Principal, error) {
	token, err := parseToken(credential)
	if err != nil {
		return identity.Principal{}, err
	}
	if !validAccessTokenType(token.header.Typ) {
		return identity.Principal{}, invalid(identity.AuthFailureMalformed)
	}
	if _, allowed := a.algorithms[token.header.Alg]; !allowed {
		return identity.Principal{}, invalid(identity.AuthFailureUnsupportedAlgorithm)
	}
	key, err := a.verificationKeyFor(ctx, token.header.Kid, token.header.Alg)
	if err != nil {
		return identity.Principal{}, err
	}
	if err := token.verify(key); err != nil {
		return identity.Principal{}, err
	}
	principal, err := a.principalFromClaims(token.claims)
	if err != nil {
		return identity.Principal{}, err
	}
	return principal, nil
}

// principalFromClaims validates the required claim set and collapses claims
// into the neutral principal. Raw claims never travel further than this
// function; memberships are typed OwnerRefs produced by the mapper.
func (a *OIDCAuthenticator) principalFromClaims(claims map[string]json.RawMessage) (identity.Principal, error) {
	issuer, err := claimString(claims, "iss")
	if err != nil || issuer != a.issuer {
		return identity.Principal{}, invalid(identity.AuthFailureIssuerMismatch)
	}
	subject, err := claimString(claims, "sub")
	if err != nil || subject == "" {
		return identity.Principal{}, invalid(identity.AuthFailureClaimsInvalid)
	}
	clientID, err := claimString(claims, "client_id")
	if err != nil || clientID == "" {
		return identity.Principal{}, invalid(identity.AuthFailureClaimsInvalid)
	}
	// RFC 9068 requires jti. It is validated for profile conformance and
	// then discarded: no replay cache, no persistence, no principal field.
	if _, err := claimJTI(claims); err != nil {
		return identity.Principal{}, invalid(identity.AuthFailureClaimsInvalid)
	}
	now := a.now()
	expiry, err := claimTime(claims, "exp")
	if err != nil || expiry.Add(a.skew).Before(now) {
		return identity.Principal{}, invalid(identity.AuthFailureExpired)
	}
	issuedAt, err := claimTime(claims, "iat")
	if err != nil || issuedAt.After(now.Add(a.skew)) {
		return identity.Principal{}, invalid(identity.AuthFailureClaimsInvalid)
	}
	if raw, present := claims["nbf"]; present {
		notBefore, err := claimNumber(raw)
		if err != nil || time.Unix(int64(notBefore), 0).After(now.Add(a.skew)) {
			return identity.Principal{}, invalid(identity.AuthFailureExpired)
		}
	}
	if !audienceContains(claims["aud"], a.audience) {
		return identity.Principal{}, invalid(identity.AuthFailureAudienceMismatch)
	}
	memberships := a.mapper.MembershipsOf(claims, subject)
	kind := identity.KindUser
	if a.kindClaim != "" {
		if _, present := claims[a.kindClaim]; present {
			value, err := claimString(claims, a.kindClaim)
			if err != nil {
				return identity.Principal{}, invalid(identity.AuthFailureClaimsInvalid)
			}
			switch identity.PrincipalKind(value) {
			case identity.KindUser:
				kind = identity.KindUser
			case identity.KindServiceAccount:
				kind = identity.KindServiceAccount
			default:
				return identity.Principal{}, invalid(identity.AuthFailureClaimsInvalid)
			}
		}
	}
	principal, err := identity.NewPrincipal(kind, a.issuer, subject, "oidc", memberships)
	if err != nil {
		return identity.Principal{}, ErrInvalidCredentials
	}
	return principal, nil
}

// claimString reads one JSON string claim.
func claimString(claims map[string]json.RawMessage, name string) (string, error) {
	raw, present := claims[name]
	if !present {
		return "", fmt.Errorf("claim %s missing", name)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("claim %s not a string", name)
	}
	return value, nil
}

// maxNumericClaim bounds NumericDate claims so hostile values cannot exploit
// integer-conversion wraparound to forge an infinitely distant expiry.
const maxNumericClaim = float64(1 << 59)

// maxJTICharacters bounds the RFC 9068 jti claim. Real issuers emit UUIDs or
// short opaque identifiers (well under 128 characters), so 255 is a
// conservative fixed ceiling that accepts every conformant token while
// preventing a hostile token from inflating validation work.
const maxJTICharacters = 255

// claimJTI validates the required jti claim: present, a JSON string,
// non-empty (whitespace-only counts as empty), and within the bounded
// length. The value is returned only for testability of the validation
// itself; production callers discard it.
func claimJTI(claims map[string]json.RawMessage) (string, error) {
	value, err := claimString(claims, "jti")
	if err != nil || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("claim jti missing, empty, or not a string")
	}
	if len(value) > maxJTICharacters {
		return "", fmt.Errorf("claim jti exceeds %d characters", maxJTICharacters)
	}
	return value, nil
}

// claimNumber reads one numeric claim value.
func claimNumber(raw json.RawMessage) (float64, error) {
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, fmt.Errorf("not a number")
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value < -maxNumericClaim || value > maxNumericClaim {
		return 0, fmt.Errorf("out of range")
	}
	return value, nil
}

// claimTime reads one NumericDate claim.
func claimTime(claims map[string]json.RawMessage, name string) (time.Time, error) {
	raw, present := claims[name]
	if !present {
		return time.Time{}, fmt.Errorf("claim %s missing", name)
	}
	value, err := claimNumber(raw)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return time.Time{}, fmt.Errorf("claim %s invalid", name)
	}
	return time.Unix(int64(value), 0).UTC(), nil
}

// audienceContains accepts aud as a single string or an array of strings and
// reports whether the Liftr API audience is among them.
func audienceContains(raw json.RawMessage, audience string) bool {
	if len(raw) == 0 {
		return false
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		return single == audience
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		for _, candidate := range list {
			if candidate == audience {
				return true
			}
		}
	}
	return false
}
