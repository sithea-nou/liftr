// SPDX-License-Identifier: Apache-2.0

// Package identity defines the provider-neutral vocabulary for authenticated
// principals and authorization actions. It deliberately knows nothing about
// JWT, OIDC, HTTP, or any identity provider: transport authenticators collapse
// their protocol-specific evidence into these types once, at the edge.
//
// The package imports only the domain and the standard library. The
// Authorizer port itself is consumer-owned by internal/application; concrete
// policy implementations satisfy that interface structurally without
// importing application (ADR-0012).
package identity

import (
	"sort"
	"strings"

	"github.com/sithea-nou/liftr/internal/domain"
)

// PrincipalKind is the coarse class of an authenticated caller. Kind is audit
// metadata in M11; no authorization rule branches on it yet.
type PrincipalKind string

const (
	KindUser           PrincipalKind = "user"
	KindServiceAccount PrincipalKind = "serviceAccount"
	KindSystem         PrincipalKind = "system"
)

func (k PrincipalKind) valid() bool {
	switch k {
	case KindUser, KindServiceAccount, KindSystem:
		return true
	default:
		return false
	}
}

// Principal is the normalized result of one successful authentication. It
// carries deliberately selected fields only — never raw tokens or raw claims.
//
// Identity is issuer-qualified: two issuers may both use subject "alice" and
// remain distinct callers because PrincipalID derives from issuer and subject
// together. Memberships are typed OwnerRefs, normalized at the claim-mapping
// boundary; string encodings such as "team:payments" never cross into
// authorization logic (ADR-0012).
type Principal struct {
	ID          PrincipalID
	Kind        PrincipalKind
	Issuer      string
	Subject     string
	Memberships []domain.OwnerRef
	Method      string
}

// principalMethodBounds bound the free-form method label so a hostile
// authenticator cannot inflate principal size.
const maxMethodLength = 64

// NewPrincipal validates its inputs, normalizes memberships deterministically,
// and derives the stable PrincipalID. It is the only sanctioned way to build
// a Principal that crosses the application boundary.
func NewPrincipal(kind PrincipalKind, issuer, subject, method string, memberships []domain.OwnerRef) (Principal, error) {
	if !kind.valid() {
		return Principal{}, ErrInvalidPrincipal
	}
	if strings.TrimSpace(issuer) == "" {
		return Principal{}, ErrInvalidPrincipal
	}
	if strings.TrimSpace(subject) == "" {
		return Principal{}, ErrInvalidPrincipal
	}
	if len(method) > maxMethodLength || strings.ContainsAny(method, "\x00\n\r") {
		return Principal{}, ErrInvalidPrincipal
	}
	normalized := normalizeMemberships(memberships)
	return Principal{
		ID:          NewPrincipalID(issuer, subject),
		Kind:        kind,
		Issuer:      issuer,
		Subject:     subject,
		Memberships: normalized,
		Method:      method,
	}, nil
}

// IsMember reports whether the principal holds exactly this owner membership.
// Comparison is structural over the typed OwnerRef pair; there is no string
// re-parsing anywhere in authorization.
func (p Principal) IsMember(owner domain.OwnerRef) bool {
	for _, membership := range p.Memberships {
		if membership == owner {
			return true
		}
	}
	return false
}

// normalizeMemberships drops invalid entries, deduplicates, and orders
// deterministically by kind then ID so identical inputs always produce
// identical principals.
func normalizeMemberships(memberships []domain.OwnerRef) []domain.OwnerRef {
	seen := make(map[domain.OwnerRef]struct{}, len(memberships))
	normalized := make([]domain.OwnerRef, 0, len(memberships))
	for _, membership := range memberships {
		if err := validateMembership(membership); err != nil {
			continue
		}
		if _, exists := seen[membership]; exists {
			continue
		}
		seen[membership] = struct{}{}
		normalized = append(normalized, membership)
	}
	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i].Kind != normalized[j].Kind {
			return normalized[i].Kind < normalized[j].Kind
		}
		return normalized[i].ID < normalized[j].ID
	})
	return normalized
}

func validateMembership(owner domain.OwnerRef) error {
	// Reuse the domain's own validation rules by round-tripping through
	// NewResource's cheapest path? No: OwnerRef validation lives behind
	// resource construction, so identity keeps a local copy of the same
	// contract (non-empty canonical kind and ID).
	if strings.TrimSpace(owner.Kind) == "" || strings.TrimSpace(owner.ID) == "" {
		return errInvalidMembership
	}
	if owner.Kind != strings.TrimSpace(owner.Kind) || owner.ID != strings.TrimSpace(owner.ID) {
		return errInvalidMembership
	}
	return nil
}

type membershipError string

func (e membershipError) Error() string { return string(e) }

const errInvalidMembership = membershipError("invalid owner membership")
