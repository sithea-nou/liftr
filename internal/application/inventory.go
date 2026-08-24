// SPDX-License-Identifier: Apache-2.0

package application

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
)

// ListResourcesRequest is the transport-facing inventory request. Cursor is
// the opaque public continuation token; every semantic cursor check happens
// inside ListResources after its single authoritative authorization decision,
// so a stale or foreign cursor can never influence which visibility scope is
// evaluated (ADR-0016).
type ListResourcesRequest struct {
	Principal      identity.Principal
	OwnerFilter    *domain.OwnerRef
	TypeName       string
	TypeVersion    string
	StateFilter    *domain.ResourceState
	IncludeDeleted bool
	Limit          int
	Cursor         string
}

// ResourceInventoryPageView is the use-case result: one ordered page plus the
// fully encoded opaque next-page cursor (empty on the final page). Transports
// never see cursor internals or private sequences.
type ResourceInventoryPageView struct {
	Items      []ResourceInventoryItem
	NextCursor string
}

// authorizeResourceList enforces the authentication precondition and delegates
// the collection decision to the configured authorizer exactly once per list
// request. Like authorize it fails closed: a missing principal or a missing
// authorizer denies listing.
func (s *Service) authorizeResourceList(ctx context.Context, principal identity.Principal) (identity.ResourceVisibility, error) {
	if principal.ID == "" || principal.Kind == "" {
		return identity.ResourceVisibility{}, ErrNotAuthorized
	}
	if s.Authorizer == nil {
		return identity.ResourceVisibility{}, ErrNotAuthorized
	}
	visibility, err := s.Authorizer.AuthorizeResourceList(ctx, principal)
	if err != nil {
		return identity.ResourceVisibility{}, fmt.Errorf("%w", ErrNotAuthorized)
	}
	return visibility.Canonical(), nil
}

// ListResources returns one ownership-scoped inventory page. The flow is
// fixed: authenticate (transport) → authorize resource:list exactly once
// inside the page transaction → validate cursor semantics against the current
// filters and current visibility → execute the bounded keyset query.
//
// Authorization denial and empty visibility are distinct outcomes: a denial
// returns ErrNotAuthorized for the transport's honest 403, while an authorized
// principal with zero visible owners receives a valid empty page (ADR-0016).
func (s *Service) ListResources(ctx context.Context, req ListResourcesRequest) (ResourceInventoryPageView, error) {
	limit := req.Limit
	if limit == 0 {
		limit = DefaultResourcePageSize
	}
	if limit < 1 || limit > MaxResourcePageSize {
		return ResourceInventoryPageView{}, fmt.Errorf("%w: resource page limit must be between 1 and %d", ErrInvalidApplicationCall, MaxResourcePageSize)
	}
	if err := validateListFilters(req); err != nil {
		return ResourceInventoryPageView{}, err
	}

	var page ResourceInventoryPage
	var visibility identity.ResourceVisibility
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		var err error
		visibility, err = s.authorizeResourceList(ctx, req.Principal)
		if err != nil {
			return err
		}
		var after uint64
		if req.Cursor != "" {
			after, err = decodeResourceListCursor(req.Cursor, req, visibility)
			if err != nil {
				return err
			}
		}
		page, err = tx.Resources().ListResources(ctx, ResourceListQuery{
			AllowedOwners:  visibility.Owners,
			Unrestricted:   visibility.AllOwners,
			OwnerFilter:    req.OwnerFilter,
			TypeName:       req.TypeName,
			TypeVersion:    req.TypeVersion,
			StateFilter:    req.StateFilter,
			IncludeDeleted: req.IncludeDeleted,
			AfterSequence:  after,
			Limit:          limit,
		})
		return err
	})
	if err != nil {
		return ResourceInventoryPageView{}, err
	}
	view := ResourceInventoryPageView{Items: page.Items}
	if view.Items == nil {
		view.Items = []ResourceInventoryItem{}
	}
	if page.NextSequence != 0 {
		view.NextCursor = encodeResourceListCursor(req, visibility, page.NextSequence)
	}
	return view, nil
}

// validateListFilters enforces the structural filter contract shared by every
// transport: owner filters are complete pairs, version requires type, state
// values come from the public vocabulary, and requesting Deleted tombstones
// through the state filter without includeDeleted is a contradiction rather
// than a silently empty result (ADR-0016).
func validateListFilters(req ListResourcesRequest) error {
	if req.OwnerFilter != nil {
		if strings.TrimSpace(req.OwnerFilter.Kind) == "" || strings.TrimSpace(req.OwnerFilter.ID) == "" ||
			req.OwnerFilter.Kind != strings.TrimSpace(req.OwnerFilter.Kind) || req.OwnerFilter.ID != strings.TrimSpace(req.OwnerFilter.ID) {
			return fmt.Errorf("%w: owner filter kind and ID must be canonical non-empty strings", ErrInvalidApplicationCall)
		}
	}
	if req.TypeVersion != "" && req.TypeName == "" {
		return fmt.Errorf("%w: filtering by type version requires a type name", ErrInvalidApplicationCall)
	}
	for _, value := range []string{req.TypeName, req.TypeVersion} {
		if len(value) > maxListFilterLength {
			return fmt.Errorf("%w: filter values are bounded to %d characters", ErrInvalidApplicationCall, maxListFilterLength)
		}
	}
	if req.OwnerFilter != nil {
		for _, value := range []string{req.OwnerFilter.Kind, req.OwnerFilter.ID} {
			if len(value) > maxListFilterLength {
				return fmt.Errorf("%w: filter values are bounded to %d characters", ErrInvalidApplicationCall, maxListFilterLength)
			}
		}
	}
	if req.StateFilter != nil {
		if !validResourceState(*req.StateFilter) {
			return fmt.Errorf("%w: unknown resource state filter", ErrInvalidApplicationCall)
		}
		if *req.StateFilter == domain.ResourceStateDeleted && !req.IncludeDeleted {
			return fmt.Errorf("%w: state Deleted requires includeDeleted", ErrInvalidApplicationCall)
		}
	}
	return nil
}

// validResourceState mirrors the domain's public ResourceState vocabulary.
func validResourceState(state domain.ResourceState) bool {
	switch state {
	case domain.ResourceStateUnknown, domain.ResourceStatePending, domain.ResourceStateReady,
		domain.ResourceStateDeleting, domain.ResourceStateDeleted, domain.ResourceStateFailed:
		return true
	default:
		return false
	}
}

const maxListFilterLength = 512

// The inventory cursor is an opaque, unsigned, versioned envelope:
//
//	r1_ <base64url( kind(1) | filterDigest(32) | visibilityDigest(32) | position(8) )>
//
// It binds three things only: the canonicalized client filter tuple, the
// canonical closed visibility scope as of issuance, and the keyset position.
// It never encodes principals, claims, tokens, or individual decisions; on
// every page the CURRENT visibility is derived fresh from the presented
// credential and must reproduce the bound digest exactly, so authorization
// changes invalidate the continuation instead of silently reshaping the
// traversal (ADR-0016).
const (
	resourceCursorPrefix     = "r1_"
	resourceCursorKind       = byte(2)
	resourceCursorBytes      = 1 + sha256.Size + sha256.Size + 8
	maxResourceCursorEncoded = 128
)

var errInvalidResourceCursor = errors.New("invalid resource inventory cursor")

// encodeResourceListCursor renders the next-page cursor for the traversal
// that produced NextSequence under exactly these filters and this visibility.
func encodeResourceListCursor(req ListResourcesRequest, visibility identity.ResourceVisibility, next uint64) string {
	payload := make([]byte, resourceCursorBytes)
	payload[0] = resourceCursorKind
	copy(payload[1:1+sha256.Size], resourceListFilterDigest(req))
	copy(payload[1+sha256.Size:1+2*sha256.Size], resourceListVisibilityDigest(visibility))
	binary.BigEndian.PutUint64(payload[1+2*sha256.Size:], next)
	return resourceCursorPrefix + base64.RawURLEncoding.EncodeToString(payload)
}

// decodeResourceListCursor validates the full envelope against the request's
// current filters and the freshly derived current visibility, returning the
// exclusive keyset position. Every failure — malformed envelope, wrong
// version, trailing data, out-of-range position, foreign filters, changed
// authorization scope — normalizes to one invalid-call error so responses
// never disclose which part failed or what any previous scope contained.
func decodeResourceListCursor(cursor string, req ListResourcesRequest, visibility identity.ResourceVisibility) (uint64, error) {
	if len(cursor) > maxResourceCursorEncoded || !strings.HasPrefix(cursor, resourceCursorPrefix) {
		return 0, fmt.Errorf("%w: %v", ErrInvalidApplicationCall, errInvalidResourceCursor)
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(cursor, resourceCursorPrefix))
	if err != nil || len(payload) != resourceCursorBytes || payload[0] != resourceCursorKind {
		return 0, fmt.Errorf("%w: %v", ErrInvalidApplicationCall, errInvalidResourceCursor)
	}
	filterMatch := equalDigest(payload[1:1+sha256.Size], resourceListFilterDigest(req))
	visibilityMatch := equalDigest(payload[1+sha256.Size:1+2*sha256.Size], resourceListVisibilityDigest(visibility))
	position := binary.BigEndian.Uint64(payload[1+2*sha256.Size:])
	if !filterMatch || !visibilityMatch {
		return 0, fmt.Errorf("%w: %v", ErrInvalidApplicationCall, errInvalidResourceCursor)
	}
	// Positions name PostgreSQL bigint identity values; anything beyond the
	// signed range cannot exist and is refused before it reaches persistence.
	if position == 0 || position > math.MaxInt64 {
		return 0, fmt.Errorf("%w: %v", ErrInvalidApplicationCall, errInvalidResourceCursor)
	}
	return position, nil
}

func equalDigest(a, b []byte) bool {
	return subtle.ConstantTimeCompare(a, b) == 1
}

// resourceListFilterDigest digests the canonicalized client filter tuple.
// Length-prefixed fields make delimiter ambiguity impossible; absent filters
// carry zero length so "unset" and "empty" encode identically.
func resourceListFilterDigest(req ListResourcesRequest) []byte {
	canonical := appendPrefixed(nil, "liftr-resource-list-filters-v1")
	kind, id := "", ""
	if req.OwnerFilter != nil {
		kind, id = req.OwnerFilter.Kind, req.OwnerFilter.ID
	}
	canonical = appendPrefixed(canonical, kind)
	canonical = appendPrefixed(canonical, id)
	canonical = appendPrefixed(canonical, req.TypeName)
	canonical = appendPrefixed(canonical, req.TypeVersion)
	state := ""
	if req.StateFilter != nil {
		state = string(*req.StateFilter)
	}
	canonical = appendPrefixed(canonical, state)
	deletedMarker := byte(0)
	if req.IncludeDeleted {
		deletedMarker = 1
	}
	digest := sha256.Sum256(append(canonical, deletedMarker))
	return digest[:]
}

// resourceListVisibilityDigest digests the canonical closed visibility scope:
// either the explicit AllOwners marker or the normalized sorted owner set.
// Reordered input therefore always produces the identical digest, and the
// marker can never collide with any explicit owner set because the marker
// byte precedes a zero-length owner count (ADR-0016).
func resourceListVisibilityDigest(visibility identity.ResourceVisibility) []byte {
	canonical := visibility.Canonical()
	encoded := appendPrefixed(nil, "liftr-resource-list-visibility-v1")
	if canonical.AllOwners {
		encoded = append(encoded, 1)
		encoded = binary.BigEndian.AppendUint32(encoded, 0)
	} else {
		encoded = append(encoded, 0)
		encoded = binary.BigEndian.AppendUint32(encoded, uint32(len(canonical.Owners)))
		for _, owner := range canonical.Owners {
			encoded = appendPrefixed(encoded, owner.Kind)
			encoded = appendPrefixed(encoded, owner.ID)
		}
	}
	digest := sha256.Sum256(encoded)
	return digest[:]
}

func appendPrefixed(dst []byte, value string) []byte {
	dst = binary.BigEndian.AppendUint32(dst, uint32(len(value)))
	return append(dst, value...)
}
