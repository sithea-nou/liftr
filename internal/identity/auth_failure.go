// SPDX-License-Identifier: Apache-2.0

package identity

import "fmt"

// AuthFailureReason is the closed, typed classification of one rejected
// authentication attempt. It is neutral authentication vocabulary owned by
// the identity/authentication boundary: the concrete verifier reports it and
// composition-layer telemetry renders it as bounded metric labels or log
// fields. It is never serialized to API clients — public credential failures
// stay indistinguishable (ADR-0012) — and it must never carry free-form
// provider or claim text.
type AuthFailureReason string

const (
	// AuthFailureNone marks a successful authentication.
	AuthFailureNone AuthFailureReason = ""
	// AuthFailureMissingCredential means no usable Authorization header was
	// presented at all.
	AuthFailureMissingCredential AuthFailureReason = "missing_credential"
	// AuthFailureMalformed covers structurally unusable credentials:
	// non-JWT syntax, bad headers, disallowed token type, oversized input.
	AuthFailureMalformed AuthFailureReason = "malformed"
	// AuthFailureUnsupportedAlgorithm means the token's alg is outside the
	// configured allowlist.
	AuthFailureUnsupportedAlgorithm AuthFailureReason = "unsupported_algorithm"
	// AuthFailureUnknownKey means verification resolved a fresh key set and
	// still found no usable key for the token's kid/alg pair.
	AuthFailureUnknownKey AuthFailureReason = "unknown_kid"
	// AuthFailureInvalidSignature means a key was resolved but signature
	// verification failed.
	AuthFailureInvalidSignature AuthFailureReason = "invalid_signature"
	// AuthFailureExpired means exp/nbf window checks failed.
	AuthFailureExpired AuthFailureReason = "expired"
	// AuthFailureIssuerMismatch means iss did not match the configured trust.
	AuthFailureIssuerMismatch AuthFailureReason = "issuer_mismatch"
	// AuthFailureAudienceMismatch means aud did not contain Liftr's audience.
	AuthFailureAudienceMismatch AuthFailureReason = "audience_mismatch"
	// AuthFailureClaimsInvalid covers required-claim validation failures
	// (sub, client_id, jti, iat) and unmappable principal construction.
	AuthFailureClaimsInvalid AuthFailureReason = "claims_invalid"
	// AuthFailureJWKSUnavailable means key material could not be fetched and
	// no cached key could verify the token.
	AuthFailureJWKSUnavailable AuthFailureReason = "jwks_unavailable"
	// AuthFailureRefreshRateLimited means the unknown-kid forced-refetch rate
	// window suppressed the refresh and the cached set could not verify the
	// token.
	AuthFailureRefreshRateLimited AuthFailureReason = "refresh_rate_limited"
	// AuthFailureOther is the bounded fallback; it must remain last-resort so
	// new verifier failure paths cannot silently mint unbounded labels.
	AuthFailureOther AuthFailureReason = "other"
)

// Valid reports whether r is one of the enumerated reasons.
func (r AuthFailureReason) Valid() bool {
	switch r {
	case AuthFailureNone,
		AuthFailureMissingCredential,
		AuthFailureMalformed,
		AuthFailureUnsupportedAlgorithm,
		AuthFailureUnknownKey,
		AuthFailureInvalidSignature,
		AuthFailureExpired,
		AuthFailureIssuerMismatch,
		AuthFailureAudienceMismatch,
		AuthFailureClaimsInvalid,
		AuthFailureJWKSUnavailable,
		AuthFailureRefreshRateLimited,
		AuthFailureOther:
		return true
	default:
		return false
	}
}

func (r AuthFailureReason) String() string {
	if !r.Valid() {
		return string(AuthFailureOther)
	}
	return string(r)
}

// invalidCredentials builds the single collapsed caller-facing error for
// transport-level credential rejections (missing or structurally unusable
// credentials) that occur before the concrete verifier is reached. It uses
// the same sentinel and message shape as the verifier so callers can never
// distinguish reasons (ADR-0012).
func invalidCredentials(r AuthFailureReason) error {
	return fmt.Errorf("invalid credentials (%s)", r)
}
