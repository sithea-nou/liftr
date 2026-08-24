// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"sort"

	"github.com/sithea-nou/liftr/internal/domain"
)

// ResourceVisibility is the closed result of collection authorization: it is
// the scope of Resources that inventory may disclose to one authenticated
// principal. It carries authorization-scope vocabulary only — never
// principals, claims, tokens, pagination, or persistence concepts (ADR-0016).
//
// The type is deliberately closed. A future authorization model that cannot
// express itself as an owner set or as unrestricted visibility requires a new
// explicit decision rather than an open-ended extension.
type ResourceVisibility struct {
	// Owners restricts inventory visibility to exactly these references.
	// Producers should supply normalized values; Canonical normalizes
	// defensively so every consumer observes one canonical form.
	Owners []domain.OwnerRef
	// AllOwners reports visibility across every owner without enumeration.
	// Only a deliberately privileged policy may set it; the secured
	// owner-membership policy never does. Explicit insecure development
	// composition is the one shipped producer, mirroring its allow-everything
	// single-target decisions.
	AllOwners bool
}

// Canonical returns the normalized form of the visibility: owners are
// deduplicated and sorted deterministically by kind then ID — the same
// canonical order as Principal.Memberships. The receiver is never mutated.
func (v ResourceVisibility) Canonical() ResourceVisibility {
	canonical := ResourceVisibility{AllOwners: v.AllOwners}
	if len(v.Owners) == 0 {
		return canonical
	}
	seen := make(map[domain.OwnerRef]struct{}, len(v.Owners))
	owners := make([]domain.OwnerRef, 0, len(v.Owners))
	for _, owner := range v.Owners {
		if owner.Kind == "" || owner.ID == "" {
			continue
		}
		if _, exists := seen[owner]; exists {
			continue
		}
		seen[owner] = struct{}{}
		owners = append(owners, owner)
	}
	sort.Slice(owners, func(i, j int) bool {
		if owners[i].Kind != owners[j].Kind {
			return owners[i].Kind < owners[j].Kind
		}
		return owners[i].ID < owners[j].ID
	})
	canonical.Owners = owners
	return canonical
}

// IsEmpty reports whether the visibility discloses nothing at all: the list
// action may be authorized while the principal still sees zero Resources,
// which ADR-0016 pins as an empty 200 collection rather than a denial.
func (v ResourceVisibility) IsEmpty() bool {
	return !v.AllOwners && len(v.Owners) == 0
}
