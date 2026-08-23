// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
)

type createResourceEnvelope struct {
	ID    string          `json:"id"`
	Type  resourceTypeDTO `json:"type"`
	Owner ownerDTO        `json:"owner"`
	Spec  json.RawMessage `json:"spec"`
}

type updateResourceEnvelope struct {
	Spec json.RawMessage `json:"spec"`
}

// requirePrincipal returns the authenticated principal assigned by the
// authentication middleware. With the middleware composed this always
// succeeds; it fails closed rather than acting unauthenticated if a handler
// were ever reached without one (ADR-0012).
func (h *handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (identity.Principal, bool) {
	principal, ok := PrincipalFromContext(r.Context())
	if !ok {
		writeUnauthenticated(w, r, false)
		return identity.Principal{}, false
	}
	return principal, true
}

// hideResource renders the Resource-absent problem. It is used for true
// absence AND for authorization denial so forbidden reads are externally
// indistinguishable from missing records under the approved hidden-404
// policy (ADR-0012).
func hideResource(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, CodeResourceNotFound, "no retained Resource record exists with this ID", nil)
}

func hideOperation(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, CodeOperationNotFound, "no Operation exists with this ID", nil)
}

// denyCreate renders the honest capability denial for create admissions.
// There is no existing record whose existence could leak, and the requested
// owner names come from the caller's own request body.
func denyCreate(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, CodeForbidden, "you are not authorized to create Resources for the requested owner", nil)
}

// createResource admits an asynchronous create request. Ordering:
// authentication (middleware) -> structural parsing -> application
// authorization of the requested type+owner -> principal-scoped idempotency
// replay -> catalog -> contract validation -> admission (ADR-0012). The first
// five steps happen inside the admission call; an unauthorized caller is
// answered 403 before any catalog or replay state is consulted.
func (h *handler) createResource(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w, r) {
		return
	}
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	key, rerr := requireIdempotencyKey(r)
	if rerr != nil {
		writeProblem(w, r, rerr.code, rerr.detail, nil)
		return
	}
	var env createResourceEnvelope
	if rerr := decodeEnvelope(r, &env); rerr != nil {
		writeProblem(w, r, rerr.code, rerr.detail, nil)
		return
	}
	if err := validateTransportID(env.ID); err != nil {
		writeProblem(w, r, CodeInvalidArgument, err.Error(), nil)
		return
	}
	if strings.TrimSpace(env.Type.Name) == "" || strings.TrimSpace(env.Type.Version) == "" {
		writeProblem(w, r, CodeInvalidArgument, "type.name and type.version are required", nil)
		return
	}
	if strings.TrimSpace(env.Owner.Kind) == "" || strings.TrimSpace(env.Owner.ID) == "" {
		writeProblem(w, r, CodeInvalidArgument, "owner.kind and owner.id are required", nil)
		return
	}
	specValues, rerr := rawSpec(env.Spec)
	if rerr != nil {
		writeProblem(w, r, rerr.code, rerr.detail, nil)
		return
	}
	spec, err := domain.NewResourceSpec(specValues)
	if err != nil {
		writeProblem(w, r, CodeInvalidArgument, "spec is not a valid resource specification", nil)
		return
	}
	id := domain.ResourceID(env.ID)

	operationID, eventID, ok := mintLifecycleIDs()
	if !ok {
		writeProblem(w, r, CodeInternal, "an unexpected internal error occurred", nil)
		return
	}
	result, err := h.service.AdmitCreateResource(r.Context(), application.CreateResourceCommand{
		Actor:          principal,
		ID:             id,
		Type:           domain.ResourceTypeRef{Name: env.Type.Name, Version: env.Type.Version},
		Owner:          domain.OwnerRef{Kind: env.Owner.Kind, ID: env.Owner.ID},
		Spec:           spec,
		OperationID:    operationID,
		EventID:        eventID,
		RequestedAt:    nowUTC(),
		IdempotencyKey: key,
	})
	if err != nil {
		if errors.Is(err, application.ErrNotAuthorized) {
			denyCreate(w, r)
			return
		}
		h.mapMutationError(w, r, principal, err, id)
		return
	}
	h.writeMutationResponse(w, r, principal, result, http.StatusCreated)
}

// getResource returns one retained Resource for an authenticated principal
// authorized on its stored owner. A Deleted Resource is a real representation
// with status.state = Deleted; 404 means Liftr has no readable record for
// this ID — which is also the answer for a record the caller cannot access,
// so existence is never disclosed to unauthorized callers (ADR-0012).
func (h *handler) getResource(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w, r) {
		return
	}
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	view, err := h.service.GetResourceOperation(r.Context(), principal, domain.ResourceID(r.PathValue("id")))
	if err != nil {
		if errors.Is(err, application.ErrNotAuthorized) {
			hideResource(w, r)
			return
		}
		h.mapReadError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Liftr-Generation", strconv.FormatUint(view.Resource.Resource.Generation(), 10))
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(newResourceDTO(view.Resource, view.Latest, view.Outputs))
}

// updateResource admits an asynchronous spec revision for a principal
// authorized on the stored owner. Ordering: authenticate (middleware),
// structural parsing of the submitted revision, load+authorize the stored
// Resource, then evaluate Idempotency-Key and If-Liftr-Generation
// requirements, then admit. An unauthorized caller is answered with the same
// 404 as an unknown ID before any precondition or conflict state can be
// observed (ADR-0012). The client must supply a concrete generation
// precondition; wildcard semantics do not exist in v1.
func (h *handler) updateResource(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w, r) {
		return
	}
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var env updateResourceEnvelope
	if rerr := decodeEnvelope(r, &env); rerr != nil {
		writeProblem(w, r, rerr.code, rerr.detail, nil)
		return
	}
	specValues, rerr := rawSpec(env.Spec)
	if rerr != nil {
		writeProblem(w, r, rerr.code, rerr.detail, nil)
		return
	}
	spec, err := domain.NewResourceSpec(specValues)
	if err != nil {
		writeProblem(w, r, CodeInvalidArgument, "spec is not a valid resource specification", nil)
		return
	}
	target, key, expectedGeneration, ok := h.requireAuthorizedMutation(w, r, principal, identity.ActionResourceUpdate)
	if !ok {
		return
	}
	operationID, eventID, ok := mintLifecycleIDs()
	if !ok {
		writeProblem(w, r, CodeInternal, "an unexpected internal error occurred", nil)
		return
	}
	result, err := h.service.AdmitUpdateResource(r.Context(), application.UpdateResourceCommand{
		Actor:              target.actor,
		ID:                 target.resourceID,
		ExpectedGeneration: expectedGeneration,
		Spec:               spec,
		OperationID:        operationID,
		EventID:            eventID,
		RequestedAt:        nowUTC(),
		IdempotencyKey:     key,
	})
	if err != nil {
		h.mapMutationDenied(w, r, target.actor, err, target.resourceID)
		return
	}
	h.writeMutationResponse(w, r, target.actor, result, http.StatusAccepted)
}

// deleteResource admits an asynchronous delete request using the same rules
// as updates: structural parsing, then authorize the stored owner before any
// precondition semantics.
func (h *handler) deleteResource(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w, r) {
		return
	}
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	var extra json.RawMessage
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeProblem(w, r, CodeInvalidArgument, "delete requests take no body", nil)
		return
	}
	target, key, expectedGeneration, ok := h.requireAuthorizedMutation(w, r, principal, identity.ActionResourceDelete)
	if !ok {
		return
	}
	operationID, eventID, ok := mintLifecycleIDs()
	if !ok {
		writeProblem(w, r, CodeInternal, "an unexpected internal error occurred", nil)
		return
	}
	result, err := h.service.AdmitDeleteResource(r.Context(), application.DeleteResourceCommand{
		Actor:              target.actor,
		ID:                 target.resourceID,
		ExpectedGeneration: expectedGeneration,
		OperationID:        operationID,
		EventID:            eventID,
		RequestedAt:        nowUTC(),
		IdempotencyKey:     key,
	})
	if err != nil {
		h.mapMutationDenied(w, r, target.actor, err, target.resourceID)
		return
	}
	h.writeMutationResponse(w, r, target.actor, result, http.StatusAccepted)
}

// mutationContext bundles the authenticated principal with the addressed
// Resource through the shared admission preconditions.
type mutationContext struct {
	actor      identity.Principal
	resourceID domain.ResourceID
}

// requireAuthorizedMutation enforces the M11 admission ordering for PUT and
// DELETE after structural parsing has completed:
//
//  1. the stored Resource is loaded and its stored owner authorized — absence
//     and denial both render the identical Resource-not-found problem BEFORE
//     any Idempotency-Key or generation requirement can be observed,
//  2. only then are the Idempotency-Key and If-Liftr-Generation requirements
//     evaluated, preserving today's exact semantics for authorized callers.
//
// The returned context carries the actor into the admission command, where
// the decision is re-checked atomically inside the admission transaction.
func (h *handler) requireAuthorizedMutation(w http.ResponseWriter, r *http.Request, principal identity.Principal, action identity.Action) (mutationContext, string, uint64, bool) {
	resourceID := domain.ResourceID(r.PathValue("id"))
	if err := h.service.CheckResourceAccess(r.Context(), principal, resourceID, action); err != nil {
		hideResource(w, r)
		return mutationContext{}, "", 0, false
	}
	key, rerr := requireIdempotencyKey(r)
	if rerr != nil {
		writeProblem(w, r, rerr.code, rerr.detail, nil)
		return mutationContext{}, "", 0, false
	}
	generation, rerr := parseGenerationPrecondition(r)
	if rerr != nil {
		writeProblem(w, r, rerr.code, rerr.detail, nil)
		return mutationContext{}, "", 0, false
	}
	return mutationContext{actor: principal, resourceID: resourceID}, key, generation, true
}

func (h *handler) requireService(w http.ResponseWriter, r *http.Request) bool {
	if h.service == nil {
		writeProblem(w, r, CodePersistenceUnavailable, "the control plane cannot currently reach durable state", nil)
		return false
	}
	return true
}

// mapMutationDenied distinguishes authorization denials (hidden 404, matching
// the probe above) from ordinary admission failures for mutations against an
// existing Resource.
func (h *handler) mapMutationDenied(w http.ResponseWriter, r *http.Request, principal identity.Principal, err error, id domain.ResourceID) {
	if errors.Is(err, application.ErrNotAuthorized) {
		hideResource(w, r)
		return
	}
	h.mapMutationError(w, r, principal, err, id)
}

// writeMutationResponse renders an admitted mutation. Location points at the
// Resource for creates and at the lifecycle Operation for updates and
// deletes; Link rel="monitor" always names the Operation returned by this
// request, which on a replay is the original Operation. The body carries the
// current Resource snapshot, so latestOperation may already be newer than the
// monitor link target after later mutations.
func (h *handler) writeMutationResponse(w http.ResponseWriter, r *http.Request, principal identity.Principal, result application.Result, status int) {
	resourceID := result.Resource.Resource.ID()
	operationID := string(result.Operation.ID())

	view := application.ResourceView{Resource: result.Resource}
	if current, err := h.service.GetResourceOperation(r.Context(), principal, resourceID); err == nil {
		view = current
	} else if operationID != "" {
		latest := result.Operation
		view.Latest = &latest
	}

	body := newResourceDTO(view.Resource, view.Latest, view.Outputs)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Liftr-Generation", strconv.FormatUint(body.Generation, 10))
	if result.Replay {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	if status == http.StatusCreated {
		w.Header().Set("Location", "/v1/resources/"+string(resourceID))
	} else {
		w.Header().Set("Location", "/v1/operations/"+operationID)
	}
	w.Header().Set("Link", `</v1/operations/`+operationID+`>; rel="monitor"`)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func requireIdempotencyKey(r *http.Request) (string, *requestError) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		return "", &requestError{code: CodeInvalidArgument, detail: "the Idempotency-Key header is required"}
	}
	return key, nil
}

// parseGenerationPrecondition accepts only a concrete unsigned 64-bit decimal
// generation. A missing header is a precondition failure; anything
// unparsable as uint64, including any wildcard spelling, is invalid.
func parseGenerationPrecondition(r *http.Request) (uint64, *requestError) {
	value := strings.TrimSpace(r.Header.Get("If-Liftr-Generation"))
	if value == "" {
		return 0, &requestError{code: CodePreconditionRequired, detail: "the If-Liftr-Generation header carrying a concrete generation is required"}
	}
	generation, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, &requestError{code: CodeInvalidArgument, detail: "If-Liftr-Generation must be a concrete unsigned 64-bit integer generation; wildcards are not supported"}
	}
	return generation, nil
}

// validateTransportID keeps Resource IDs addressable as a single URL path
// segment and safe to echo in response headers. It is transport safety only;
// domain identity rules remain authoritative.
func validateTransportID(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("id is required")
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c <= 0x20 || c == 0x7f || c == '/' {
			return errors.New("id must be a URL path-segment-safe string without whitespace")
		}
	}
	return nil
}

func mintLifecycleIDs() (domain.OperationID, domain.EventID, bool) {
	operationToken, ok := randomToken()
	if !ok {
		return "", "", false
	}
	eventToken, ok := randomToken()
	if !ok {
		return "", "", false
	}
	return domain.OperationID("op-" + operationToken), domain.EventID("evt-" + eventToken), true
}

func randomToken() (string, bool) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", false
	}
	return hex.EncodeToString(raw[:]), true
}

func nowUTC() time.Time {
	return time.Now().UTC()
}
