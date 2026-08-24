// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
)

// resourceListQuery is the bounded, structurally validated transport view of
// one inventory request. Parsing here is purely syntactic: every semantic
// decision — authorization, cursor validity against the current visibility —
// happens exactly once inside the application use case (ADR-0016).
type resourceListQuery struct {
	limit          int
	cursor         string
	ownerKind      string
	ownerID        string
	typeName       string
	typeVersion    string
	stateFilter    *domain.ResourceState
	includeDeleted bool
}

var resourceStateFilterValues = map[string]domain.ResourceState{
	string(domain.ResourceStateUnknown):  domain.ResourceStateUnknown,
	string(domain.ResourceStatePending):  domain.ResourceStatePending,
	string(domain.ResourceStateReady):    domain.ResourceStateReady,
	string(domain.ResourceStateDeleting): domain.ResourceStateDeleting,
	string(domain.ResourceStateDeleted):  domain.ResourceStateDeleted,
	string(domain.ResourceStateFailed):   domain.ResourceStateFailed,
}

const maxResourceFilterLength = 512

// parseResourceListQuery enforces the approved parameter surface: unknown or
// duplicated parameters are invalid, owner filters must be complete pairs,
// version requires type, and state=Deleted without includeDeleted is a
// contradictory query rather than a silently empty result (ADR-0016).
func parseResourceListQuery(r *http.Request) (resourceListQuery, *requestError) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return resourceListQuery{}, badRequest("query parameters are malformed")
	}
	allowed := map[string]bool{
		"limit": true, "cursor": true,
		"ownerKind": true, "ownerId": true,
		"type": true, "version": true,
		"state": true, "includeDeleted": true,
	}
	for name, values := range query {
		if !allowed[name] {
			return resourceListQuery{}, badRequest("only limit, cursor, ownerKind, ownerId, type, version, state, and includeDeleted query parameters are supported")
		}
		if len(values) != 1 {
			return resourceListQuery{}, badRequest("query parameters may be supplied at most once")
		}
	}
	parsed := resourceListQuery{limit: application.DefaultResourcePageSize}
	if values, present := query["limit"]; present {
		value := values[0]
		if value == "" || strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return resourceListQuery{}, badRequest("limit must be an integer between 1 and 100")
		}
		limit, err := strconv.ParseUint(value, 10, 8)
		if err != nil || limit < 1 || limit > application.MaxResourcePageSize {
			return resourceListQuery{}, badRequest("limit must be an integer between 1 and 100")
		}
		parsed.limit = int(limit)
	}
	if values, present := query["cursor"]; present {
		if values[0] == "" {
			return resourceListQuery{}, badRequest("cursor must not be empty")
		}
		parsed.cursor = values[0]
	}
	if rawKind, kindPresent := query["ownerKind"]; kindPresent && rawKind[0] == "" {
		return resourceListQuery{}, badRequest("ownerKind must not be empty")
	}
	if rawID, idPresent := query["ownerId"]; idPresent && rawID[0] == "" {
		return resourceListQuery{}, badRequest("ownerId must not be empty")
	}
	_, kindPresent := query["ownerKind"]
	_, idPresent := query["ownerId"]
	if kindPresent != idPresent {
		return resourceListQuery{}, badRequest("ownerKind and ownerId must be supplied together")
	}
	if kindPresent {
		parsed.ownerKind = query.Get("ownerKind")
		parsed.ownerID = query.Get("ownerId")
		for key, value := range map[string]string{"ownerKind": parsed.ownerKind, "ownerId": parsed.ownerID} {
			if len(value) > maxResourceFilterLength || strings.TrimSpace(value) != value {
				return resourceListQuery{}, badRequest(key + " must be a canonical bounded string")
			}
		}
	}
	if rawType, present := query["type"]; present {
		if len(rawType[0]) > maxResourceFilterLength || strings.TrimSpace(rawType[0]) != rawType[0] {
			return resourceListQuery{}, badRequest("type must be a canonical bounded string")
		}
		parsed.typeName = rawType[0]
	}
	if rawVersion, present := query["version"]; present {
		if parsed.typeName == "" {
			return resourceListQuery{}, badRequest("version requires the type query parameter")
		}
		if len(rawVersion[0]) > maxResourceFilterLength || strings.TrimSpace(rawVersion[0]) != rawVersion[0] {
			return resourceListQuery{}, badRequest("version must be a canonical bounded string")
		}
		parsed.typeVersion = rawVersion[0]
	}
	if rawState, present := query["state"]; present {
		state, known := resourceStateFilterValues[rawState[0]]
		if !known {
			return resourceListQuery{}, badRequest("state must be one of Unknown, Pending, Ready, Deleting, Deleted, Failed")
		}
		parsed.stateFilter = &state
	}
	if rawDeleted, present := query["includeDeleted"]; present {
		switch rawDeleted[0] {
		case "true":
			parsed.includeDeleted = true
		case "false":
			parsed.includeDeleted = false
		default:
			return resourceListQuery{}, badRequest("includeDeleted must be true or false")
		}
	}
	if parsed.stateFilter != nil && *parsed.stateFilter == domain.ResourceStateDeleted && !parsed.includeDeleted {
		return resourceListQuery{}, badRequest("state=Deleted requires includeDeleted=true")
	}
	return parsed, nil
}

// denyList renders the honest collection denial. Unlike single-Resource reads,
// a collection discloses no per-record existence to probe, so FORBIDDEN is
// truthful here — matching forbidden creates and discovery denials (ADR-0012,
// ADR-0016). The detail never reveals whether any Resources exist.
func denyList(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, CodeForbidden, "you are not authorized to list Resources", nil)
}

// listResources serves GET /v1/resources: the ownership-scoped Resource
// inventory for an authenticated principal. The flow is fixed by ADR-0016:
// authenticate (middleware), parse the bounded structural query, then delegate
// everything else — one authoritative resource:list decision, cursor
// validation against the current visibility scope, keyset paging — to
// Service.ListResources. An authorized caller whose visibility is empty and a
// caller whose owner filter lies outside their visibility both receive the
// same 200 empty collection; only whole-action denial answers 403.
func (h *handler) listResources(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w, r) {
		return
	}
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	parsed, rerr := parseResourceListQuery(r)
	if rerr != nil {
		writeProblem(w, r, rerr.code, rerr.detail, nil)
		return
	}
	request := application.ListResourcesRequest{Principal: principal, Limit: parsed.limit, Cursor: parsed.cursor, IncludeDeleted: parsed.includeDeleted}
	if parsed.ownerKind != "" {
		request.OwnerFilter = &domain.OwnerRef{Kind: parsed.ownerKind, ID: parsed.ownerID}
	}
	if parsed.typeName != "" {
		request.TypeName = parsed.typeName
		request.TypeVersion = parsed.typeVersion
	}
	if parsed.stateFilter != nil {
		request.StateFilter = parsed.stateFilter
	}
	page, err := h.service.ListResources(r.Context(), request)
	if err != nil {
		switch {
		case errors.Is(err, application.ErrNotAuthorized):
			denyList(w, r)
		case errors.Is(err, application.ErrInvalidApplicationCall):
			// Semantic cursor/filter failures normalize here after the single
			// authorization: the response never reveals which part of the
			// cursor mismatched, nor anything about previous scopes.
			writeProblem(w, r, CodeInvalidArgument, "the request is not valid for this endpoint", nil)
		default:
			h.mapReadError(w, r, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, newResourceListDTO(page))
}
