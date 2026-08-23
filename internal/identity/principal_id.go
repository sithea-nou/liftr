// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strings"
)

// ErrInvalidPrincipal reports that principal inputs failed validation.
var ErrInvalidPrincipal = errors.New("invalid principal")

// PrincipalID is a stable, opaque, versioned caller identity derived from the
// authenticated issuer and subject. It scopes idempotency namespaces, carries
// actor attribution, and appears in logs — so it must never change for the
// same issuer+subject pair and must never collide across different pairs.
//
// The v1 derivation digests a length-prefixed encoding of issuer and subject
// (the same injective scheme as application fingerprints), so delimiter
// concatenation ambiguities are impossible:
//
//	prn_v1_<64 lowercase hex characters>
//
// Any change to the digest input encoding or hash requires a new "prn_vN_"
// prefix; v1 values are never reinterpreted under a different scheme.
type PrincipalID string

const principalIDPrefixV1 = "prn_v1_"

// NewPrincipalID deterministically derives the v1 principal identifier.
func NewPrincipalID(issuer, subject string) PrincipalID {
	hasher := sha256.New()
	writeFramed(hasher, issuer)
	writeFramed(hasher, subject)
	return PrincipalID(principalIDPrefixV1 + hex.EncodeToString(hasher.Sum(nil)))
}

// writeFramed writes one length-prefixed field so no value can be confused
// with a delimiter or with another field's content.
func writeFramed(hasher hash.Hash, part string) {
	hasher.Write([]byte(fmt.Sprintf("%08x", len(part))))
	hasher.Write([]byte(part))
}

// ValidatePrincipalID reports whether id has the exact shape produced by
// NewPrincipalID. It guards persistence boundaries against malformed scopes.
func ValidatePrincipalID(id PrincipalID) error {
	value := string(id)
	if !strings.HasPrefix(value, principalIDPrefixV1) {
		return fmt.Errorf("%w: missing %q prefix", ErrInvalidPrincipal, principalIDPrefixV1)
	}
	digest := strings.TrimPrefix(value, principalIDPrefixV1)
	if len(digest) != sha256.Size*2 {
		return fmt.Errorf("%w: digest length", ErrInvalidPrincipal)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return fmt.Errorf("%w: digest is not hexadecimal", ErrInvalidPrincipal)
	}
	return nil
}
