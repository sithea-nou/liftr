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

// createResource admits an asynchronous create request. Infrastructure
// readiness stays asynchronous; the response reports the accepted desired
// state, not provider progress.
func (h *handler) createResource(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w, r) {
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
		h.mapMutationError(w, r, err, id)
		return
	}
	h.writeMutationResponse(w, r, result, http.StatusCreated)
}

// getResource returns one retained Resource. A Deleted Resource is a real
// representation with status.state = Deleted; 404 means Liftr has no retained
// record for this ID at all.
func (h *handler) getResource(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w, r) {
		return
	}
	view, err := h.service.GetResourceOperation(r.Context(), domain.ResourceID(r.PathValue("id")))
	if err != nil {
		h.mapReadError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Liftr-Generation", strconv.FormatUint(view.Resource.Resource.Generation(), 10))
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(newResourceDTO(view.Resource, view.Latest))
}

// updateResource admits an asynchronous spec revision. The client must supply
// a concrete generation precondition; wildcard semantics do not exist in v1.
func (h *handler) updateResource(w http.ResponseWriter, r *http.Request) {
	id, key, expectedGeneration, ok := h.requireMutationPreconditions(w, r)
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
	operationID, eventID, ok := mintLifecycleIDs()
	if !ok {
		writeProblem(w, r, CodeInternal, "an unexpected internal error occurred", nil)
		return
	}
	result, err := h.service.AdmitUpdateResource(r.Context(), application.UpdateResourceCommand{
		ID:                 id,
		ExpectedGeneration: expectedGeneration,
		Spec:               spec,
		OperationID:        operationID,
		EventID:            eventID,
		RequestedAt:        nowUTC(),
		IdempotencyKey:     key,
	})
	if err != nil {
		h.mapMutationError(w, r, err, id)
		return
	}
	h.writeMutationResponse(w, r, result, http.StatusAccepted)
}

// deleteResource admits an asynchronous delete request using the same
// concrete generation precondition rules as updates.
func (h *handler) deleteResource(w http.ResponseWriter, r *http.Request) {
	id, key, expectedGeneration, ok := h.requireMutationPreconditions(w, r)
	if !ok {
		return
	}
	var extra json.RawMessage
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeProblem(w, r, CodeInvalidArgument, "delete requests take no body", nil)
		return
	}
	operationID, eventID, ok := mintLifecycleIDs()
	if !ok {
		writeProblem(w, r, CodeInternal, "an unexpected internal error occurred", nil)
		return
	}
	result, err := h.service.AdmitDeleteResource(r.Context(), application.DeleteResourceCommand{
		ID:                 id,
		ExpectedGeneration: expectedGeneration,
		OperationID:        operationID,
		EventID:            eventID,
		RequestedAt:        nowUTC(),
		IdempotencyKey:     key,
	})
	if err != nil {
		h.mapMutationError(w, r, err, id)
		return
	}
	h.writeMutationResponse(w, r, result, http.StatusAccepted)
}

// requireMutationPreconditions enforces the shared admission rules: the
// service boundary exists, an Idempotency-Key is present, and a concrete
// uint64 If-Liftr-Generation precondition is present.
func (h *handler) requireMutationPreconditions(w http.ResponseWriter, r *http.Request) (domain.ResourceID, string, uint64, bool) {
	if !h.requireService(w, r) {
		return "", "", 0, false
	}
	key, rerr := requireIdempotencyKey(r)
	if rerr != nil {
		writeProblem(w, r, rerr.code, rerr.detail, nil)
		return "", "", 0, false
	}
	generation, rerr := parseGenerationPrecondition(r)
	if rerr != nil {
		writeProblem(w, r, rerr.code, rerr.detail, nil)
		return "", "", 0, false
	}
	return domain.ResourceID(r.PathValue("id")), key, generation, true
}

func (h *handler) requireService(w http.ResponseWriter, r *http.Request) bool {
	if h.service == nil {
		writeProblem(w, r, CodePersistenceUnavailable, "the control plane cannot currently reach durable state", nil)
		return false
	}
	return true
}

// writeMutationResponse renders an admitted mutation. Location points at the
// Resource for creates and at the lifecycle Operation for updates and
// deletes; Link rel="monitor" always names the Operation returned by this
// request, which on a replay is the original Operation. The body carries the
// current Resource snapshot, so latestOperation may already be newer than the
// monitor link target after later mutations.
func (h *handler) writeMutationResponse(w http.ResponseWriter, r *http.Request, result application.Result, status int) {
	resourceID := result.Resource.Resource.ID()
	operationID := string(result.Operation.ID())

	view := application.ResourceView{Resource: result.Resource}
	if current, err := h.service.GetResourceOperation(r.Context(), resourceID); err == nil {
		view = current
	} else if operationID != "" {
		latest := result.Operation
		view.Latest = &latest
	}

	body := newResourceDTO(view.Resource, view.Latest)
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
