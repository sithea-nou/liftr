// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/sithea-nou/liftr/internal/domain"
)

// Membership bounds make claim processing deterministic and immune to
// unbounded input: a hostile token cannot inflate principal size through its
// group claim (ADR-0012).
const (
	DefaultMaxGroupClaimEntries = 256
	DefaultMaxGroupEntryLength  = 512
	DefaultMaxGroupClaimBytes   = 64 << 10
	defaultGroupClaim           = "groups"
)

// ClaimMapper normalizes identity-provider group claims plus optional static
// grants into typed owner memberships. String encodings such as
// "liftr:team:payments" are a deployment convention that terminates here:
// authorization only ever sees domain.OwnerRef values.
//
// Malformed, oversized, or unmapped entries grant nothing; they never fail
// authentication.
type ClaimMapper struct {
	// GroupClaim names the token claim carrying group values. Empty means
	// "groups"; the name is configuration, never a hard-coded assumption.
	GroupClaim string
	// Prefix is stripped from each entry before interpretation, enabling the
	// "liftr:team:payments" convention against IdPs that require prefixed
	// claims.
	Prefix string
	// Grants maps raw group values and subjects directly to typed OwnerRefs
	// for IdPs whose identifiers must not leak into OwnerRef.
	Grants StaticGrants
	// MaxEntries, MaxEntryLength, and MaxTotalBytes bound processing. Zero
	// selects the defaults above.
	MaxEntries     int
	MaxEntryLength int
	MaxTotalBytes  int
}

// StaticGrants is the optional deployment-supplied mapping file. Both fields
// map directly onto typed OwnerRefs; no string encoding is involved.
type StaticGrants struct {
	Subjects map[string][]domain.OwnerRef `json:"subjects"`
	Groups   map[string][]domain.OwnerRef `json:"groups"`
}

func (m ClaimMapper) withDefaults() ClaimMapper {
	if m.GroupClaim == "" {
		m.GroupClaim = defaultGroupClaim
	}
	if m.MaxEntries == 0 {
		m.MaxEntries = DefaultMaxGroupClaimEntries
	}
	if m.MaxEntryLength == 0 {
		m.MaxEntryLength = DefaultMaxGroupEntryLength
	}
	if m.MaxTotalBytes == 0 {
		m.MaxTotalBytes = DefaultMaxGroupClaimBytes
	}
	return m
}

// MembershipsOf computes the principal's typed owner memberships. Output is
// deduplicated and sorted deterministically by kind then ID (NewPrincipal
// re-applies the same normalization defensively).
func (m ClaimMapper) MembershipsOf(claims map[string]json.RawMessage, subject string) []domain.OwnerRef {
	mapper := m.withDefaults()
	memberships := mapper.staticSubjectGrants(subject)
	raw, present := claims[mapper.GroupClaim]
	if present {
		var entries []string
		// The expected JSON shape is an array of strings. Any other type —
		// scalar, object, array of non-strings — grants nothing rather than
		// failing authentication.
		if err := json.Unmarshal(raw, &entries); err == nil {
			parsed := mapper.groupMemberships(entries)
			memberships = append(memberships, parsed...)
			for rawGroup, granted := range mapper.Grants.Groups {
				if containsEntry(entries, rawGroup) {
					memberships = append(memberships, granted...)
				}
			}
		}
	}
	return normalize(memberships)
}

// staticSubjectGrants returns grants keyed by subject, independent of any
// group claim presence.
func (m ClaimMapper) staticSubjectGrants(subject string) []domain.OwnerRef {
	granted, ok := m.Grants.Subjects[subject]
	if !ok {
		return nil
	}
	return append([]domain.OwnerRef(nil), granted...)
}

// groupMemberships interprets bounded group entries under the prefix
// convention. Sorting before applying caps keeps truncation deterministic;
// the entry-count cap bounds how many entries are processed at all.
func (m ClaimMapper) groupMemberships(entries []string) []domain.OwnerRef {
	sorted := append([]string(nil), entries...)
	sort.Strings(sorted)
	totalBytes := 0
	processed := 0
	memberships := make([]domain.OwnerRef, 0, len(sorted))
	for _, entry := range sorted {
		if processed >= m.MaxEntries {
			break
		}
		processed++
		totalBytes += len(entry)
		if totalBytes > m.MaxTotalBytes || len(entry) > m.MaxEntryLength {
			break
		}
		owner, ok := m.interpret(entry)
		if !ok {
			continue
		}
		memberships = append(memberships, owner)
	}
	return memberships
}

// interpret applies the configured prefix strip and the "kind:id" convention.
// The string representation terminates here: everything downstream is typed.
func (m ClaimMapper) interpret(entry string) (domain.OwnerRef, bool) {
	if m.Prefix != "" {
		if !strings.HasPrefix(entry, m.Prefix) {
			return domain.OwnerRef{}, false
		}
		entry = strings.TrimPrefix(entry, m.Prefix)
	}
	kind, id, found := strings.Cut(entry, ":")
	if !found || kind == "" || id == "" {
		return domain.OwnerRef{}, false
	}
	if strings.TrimSpace(kind) == "" || strings.TrimSpace(id) == "" ||
		kind != strings.TrimSpace(kind) || id != strings.TrimSpace(id) {
		return domain.OwnerRef{}, false
	}
	return domain.OwnerRef{Kind: kind, ID: id}, true
}

// normalize deduplicates and orders memberships deterministically.
func normalize(memberships []domain.OwnerRef) []domain.OwnerRef {
	seen := make(map[domain.OwnerRef]struct{}, len(memberships))
	unique := make([]domain.OwnerRef, 0, len(memberships))
	for _, membership := range memberships {
		if membership.Kind == "" || membership.ID == "" {
			continue
		}
		if _, exists := seen[membership]; exists {
			continue
		}
		seen[membership] = struct{}{}
		unique = append(unique, membership)
	}
	sort.Slice(unique, func(i, j int) bool {
		if unique[i].Kind != unique[j].Kind {
			return unique[i].Kind < unique[j].Kind
		}
		return unique[i].ID < unique[j].ID
	})
	return unique
}

// containsEntry reports whether the entry list carries exactly this raw
// value, used for static group-grant lookups without re-interpreting entries.
func containsEntry(entries []string, value string) bool {
	for _, entry := range entries {
		if entry == value {
			return true
		}
	}
	return false
}
