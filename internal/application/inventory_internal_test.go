// SPDX-License-Identifier: Apache-2.0

// Package-level tests for the inventory cursor internals: digest
// canonicalization and envelope validation are invariants of the codec
// itself, not of any single transport (ADR-0016).
package application

import (
	"encoding/base64"
	"encoding/binary"
	"math"
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
)

func listRequestFor(filters ListResourcesRequest) ListResourcesRequest { return filters }

func TestResourceListVisibilityDigestIsOrderInsensitive(t *testing.T) {
	first := identity.ResourceVisibility{Owners: []domain.OwnerRef{
		{Kind: "team", ID: "payments"}, {Kind: "team", ID: "platform"}, {Kind: "org", ID: "acme"},
	}}
	second := identity.ResourceVisibility{Owners: []domain.OwnerRef{
		{Kind: "org", ID: "acme"}, {Kind: "team", ID: "platform"}, {Kind: "team", ID: "payments"},
	}}
	if resourceListVisibilityDigest(first) == nil {
		t.Fatal("digest is empty")
	}
	if string(resourceListVisibilityDigest(first)) != string(resourceListVisibilityDigest(second)) {
		t.Fatal("reordered owner sets produced different visibility digests")
	}
	duplicated := identity.ResourceVisibility{Owners: []domain.OwnerRef{
		{Kind: "team", ID: "platform"}, {Kind: "team", ID: "platform"},
	}}
	unique := identity.ResourceVisibility{Owners: []domain.OwnerRef{{Kind: "team", ID: "platform"}}}
	if string(resourceListVisibilityDigest(duplicated)) != string(resourceListVisibilityDigest(unique)) {
		t.Fatal("duplicate owners were not canonicalized before digesting")
	}
}

func TestResourceListVisibilityDigestAllOwnersCannotCollideWithExplicitSets(t *testing.T) {
	unrestricted := identity.ResourceVisibility{AllOwners: true}
	candidates := []identity.ResourceVisibility{
		{},
		{Owners: []domain.OwnerRef{}},
		{Owners: []domain.OwnerRef{{Kind: "team", ID: "payments"}}},
		{Owners: []domain.OwnerRef{{Kind: "", ID: ""}}},
	}
	unrestrictedDigest := string(resourceListVisibilityDigest(unrestricted))
	for i, candidate := range candidates {
		if unrestrictedDigest == string(resourceListVisibilityDigest(candidate)) {
			t.Fatalf("AllOwners digest collided with explicit set %d", i)
		}
	}
}

func TestDecodeResourceListCursorRejectsPositionBeyondBigintRange(t *testing.T) {
	request := listRequestFor(ListResourcesRequest{TypeName: "PostgreSQLDatabase"})
	visibility := identity.ResourceVisibility{Owners: []domain.OwnerRef{{Kind: "team", ID: "payments"}}}

	payload := make([]byte, resourceCursorBytes)
	payload[0] = resourceCursorKind
	copy(payload[1:1+32], resourceListFilterDigest(request))
	copy(payload[1+32:1+64], resourceListVisibilityDigest(visibility))
	binary.BigEndian.PutUint64(payload[1+64:], math.MaxUint64)
	overflowing := resourceCursorPrefix + base64.RawURLEncoding.EncodeToString(payload)

	position, err := decodeResourceListCursor(overflowing, request, visibility)
	if err == nil {
		t.Fatalf("accepted position beyond PostgreSQL bigint range (decoded %d)", position)
	}
	if !strings.Contains(err.Error(), ErrInvalidApplicationCall.Error()) {
		t.Fatalf("cursor rejection lost the invalid-call sentinel: %v", err)
	}

	binary.BigEndian.PutUint64(payload[1+64:], 0)
	zero := resourceCursorPrefix + base64.RawURLEncoding.EncodeToString(payload)
	if _, err := decodeResourceListCursor(zero, request, visibility); err == nil {
		t.Fatal("accepted a zero keyset position")
	}
}

func TestDecodeResourceListCursorRejectsWrongVersionAndTrailingData(t *testing.T) {
	request := listRequestFor(ListResourcesRequest{})
	visibility := identity.ResourceVisibility{AllOwners: true}
	valid := encodeResourceListCursor(request, visibility, 7)

	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(valid, resourceCursorPrefix))
	if err != nil {
		t.Fatal(err)
	}
	wrongKind := append([]byte(nil), raw...)
	wrongKind[0] = 3
	if _, err := decodeResourceListCursor(resourceCursorPrefix+base64.RawURLEncoding.EncodeToString(wrongKind), request, visibility); err == nil {
		t.Fatal("accepted an unknown cursor version")
	}
	trailing := resourceCursorPrefix + base64.RawURLEncoding.EncodeToString(append(append([]byte(nil), raw...), 0xAA))
	if _, err := decodeResourceListCursor(trailing, request, visibility); err == nil {
		t.Fatal("accepted trailing data in the cursor payload")
	}
	legacyOperationStyle := strings.Replace(valid, resourceCursorPrefix, "c1_", 1)
	if _, err := decodeResourceListCursor(legacyOperationStyle, request, visibility); err == nil {
		t.Fatal("accepted an operation-history cursor as an inventory cursor")
	}
	if _, err := decodeResourceListCursor(strings.ToUpper(valid), request, visibility); err == nil {
		t.Fatal("accepted a mutated cursor")
	}
}

func TestResourceCursorBindsFiltersAndVisibilityTogether(t *testing.T) {
	request := listRequestFor(ListResourcesRequest{OwnerFilter: &domain.OwnerRef{Kind: "team", ID: "payments"}})
	visibility := identity.ResourceVisibility{Owners: []domain.OwnerRef{{Kind: "team", ID: "payments"}}}
	cursor := encodeResourceListCursor(request, visibility, 9)

	sameFiltersOtherScope := identity.ResourceVisibility{Owners: []domain.OwnerRef{{Kind: "team", ID: "payments"}, {Kind: "team", ID: "platform"}}}
	if _, err := decodeResourceListCursor(cursor, request, sameFiltersOtherScope); err == nil {
		t.Fatal("cursor continued under a changed visibility scope")
	}
	sameScopeOtherFilters := listRequestFor(ListResourcesRequest{})
	if _, err := decodeResourceListCursor(cursor, sameScopeOtherFilters, visibility); err == nil {
		t.Fatal("cursor continued under changed filters")
	}
	if position, err := decodeResourceListCursor(cursor, request, visibility); err != nil || position != 9 {
		t.Fatalf("identical filters and scope failed to continue: position=%d err=%v", position, err)
	}
}
