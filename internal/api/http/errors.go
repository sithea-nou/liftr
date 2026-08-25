// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
	"github.com/sithea-nou/liftr/internal/lifecycle"
)

// mapReadError translates a read-use-case failure into a Problem response.
func (h *handler) mapReadError(w http.ResponseWriter, r *http.Request, err error) {
	if h.service == nil {
		writeProblem(w, r, CodePersistenceUnavailable, "the control plane cannot currently reach durable state", nil)
		return
	}
	if errors.Is(err, application.ErrResourceNotFound) {
		writeProblem(w, r, CodeResourceNotFound, "no retained Resource record exists with this ID", nil)
		return
	}
	if errors.Is(err, application.ErrOperationNotFound) {
		writeProblem(w, r, CodeOperationNotFound, "no Operation exists with this ID", nil)
		return
	}
	// Collection and cursor semantics normalize to one invalid-argument form
	// after the single authorization decision, so responses never disclose
	// which part of a request mismatched (ADR-0016).
	if errors.Is(err, application.ErrInvalidApplicationCall) {
		writeProblem(w, r, CodeInvalidArgument, "the request is not valid for this endpoint", nil)
		return
	}
	// Discovery addresses a ResourceType entity directly, so an unknown
	// name/version pair is a 404 rather than the 422 used by mutations.
	if errors.Is(err, application.ErrResourceTypeNotFound) {
		writeProblem(w, r, CodeResourceTypeNotFound, "no ResourceType is registered with this name and version", nil)
		return
	}
	mapTransportFailure(w, r, err)
}

// mapMutationError translates an admission failure into a Problem response.
// The target Resource ID is supplied by the caller because create requests
// carry it in their body rather than their path.
func (h *handler) mapMutationError(w http.ResponseWriter, r *http.Request, principal identity.Principal, err error, id domain.ResourceID) {
	if h.service == nil {
		writeProblem(w, r, CodePersistenceUnavailable, "the control plane cannot currently reach durable state", nil)
		return
	}
	if errors.Is(err, application.ErrResourceTypeNotFound) {
		writeProblem(w, r, CodeUnsupportedResourceType, "the requested resource type is not registered", nil)
		return
	}
	var invalidSpec *application.InvalidSpecError
	if errors.As(err, &invalidSpec) {
		detail := fmt.Sprintf("the submitted spec does not satisfy the %s/%s contract",
			invalidSpec.TypeRef.Name, invalidSpec.TypeRef.Version)
		writeSpecProblem(w, r, detail, invalidSpec)
		return
	}
	if errors.Is(err, application.ErrProvisionerNotFound) {
		writeProblem(w, r, CodeProvisionerUnavailable, "no provisioner is available for this request", nil)
		return
	}
	if errors.Is(err, application.ErrPolicyDenied) {
		writeProblem(w, r, CodePolicyDenied, "platform policy does not permit this Resource mutation", nil)
		return
	}
	if errors.Is(err, application.ErrQuotaExceeded) {
		writeProblem(w, r, CodeQuotaExceeded, "the applicable Resource count quota has been reached", nil)
		return
	}
	if errors.Is(err, application.ErrPersistenceUnavailable) {
		writeProblem(w, r, CodePersistenceUnavailable, "the control plane cannot currently verify admission against durable state", nil)
		return
	}
	if errors.Is(err, application.ErrIdempotencyConflict) {
		writeProblem(w, r, CodeIdempotencyConflict, "this Idempotency-Key was already used with different request content", nil)
		return
	}
	if errors.Is(err, application.ErrResourceNotFound) {
		writeProblem(w, r, CodeResourceNotFound, "no retained Resource record exists with this ID", nil)
		return
	}
	if errors.Is(err, lifecycle.ErrOperationActive) {
		writeProblem(w, r, CodeOperationActive, "the Resource already has an active Operation; monitor it before requesting another mutation", nil)
		return
	}
	if errors.Is(err, application.ErrConcurrencyConflict) {
		h.mapConcurrencyConflict(w, r, principal, id)
		return
	}
	if errors.Is(err, lifecycle.ErrInvalidTransition) {
		h.mapStateConflict(w, r, principal, id)
		return
	}
	mapTransportFailure(w, r, err)
}

// mapConcurrencyConflict distinguishes the approved conflict codes. A stale
// generation precondition yields GENERATION_CONFLICT with the current
// generation when one can be read; a create colliding with any retained
// record yields RESOURCE_ALREADY_EXISTS; remaining optimistic conflicts are
// RESOURCE_STATE_CONFLICT. Details are curated sentences; underlying storage
// or provider errors are never echoed to clients.
func (h *handler) mapConcurrencyConflict(w http.ResponseWriter, r *http.Request, principal identity.Principal, id domain.ResourceID) {
	switch {
	case r.Method == http.MethodPost:
		h.mapCreateCollision(w, r, principal, id)
	case r.Method == http.MethodPut || r.Method == http.MethodDelete:
		var current *uint64
		detail := "the supplied If-Liftr-Generation does not match the current generation of the Resource"
		record, readErr := h.service.GetResource(r.Context(), principal, id)
		if readErr == nil {
			generation := record.Resource.Generation()
			current = &generation
		} else {
			detail = "the supplied If-Liftr-Generation no longer matches any retained state of the Resource"
		}
		writeProblem(w, r, CodeGenerationConflict, detail, current)
	default:
		writeProblem(w, r, CodeResourceStateConflict, "the Resource changed concurrently; retry after re-reading its current state", nil)
	}
}

// mapStateConflict renders lifecycle precondition failures. For creates, a
// state conflict on an ID that already has a retained record is reported as
// RESOURCE_ALREADY_EXISTS because a Resource can only ever be created once.
func (h *handler) mapStateConflict(w http.ResponseWriter, r *http.Request, principal identity.Principal, id domain.ResourceID) {
	if r.Method == http.MethodPost {
		h.mapCreateCollision(w, r, principal, id)
		return
	}
	writeProblem(w, r, CodeResourceStateConflict, "the current Resource state does not permit this operation", nil)
}

// mapCreateCollision probes the retained record for the requested ID.
// Tombstones are retained records, so recreation under a used ID always
// conflicts instead of resurrecting or duplicating.
func (h *handler) mapCreateCollision(w http.ResponseWriter, r *http.Request, principal identity.Principal, id domain.ResourceID) {
	detail := "a retained Resource record already exists with this ID; tombstones are never resurrected under the same ID"
	var current *uint64
	if view, readErr := h.service.GetResourceOperation(r.Context(), principal, id); readErr == nil && view.Resource.Resource.ID() != "" {
		generation := view.Resource.Resource.Generation()
		current = &generation
	}
	writeProblem(w, r, CodeResourceAlreadyExists, detail, current)
}

// mapTransportFailure renders failures the transport cannot classify more
// precisely. Client disconnects get no Problem body because the connection is
// gone; timeouts surface as persistence unavailability; everything else stays
// an opaque internal error.
func mapTransportFailure(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeProblem(w, r, CodePersistenceUnavailable, "the control plane could not complete this request in time", nil)
		return
	}
	writeProblem(w, r, CodeInternal, "an unexpected internal error occurred", nil)
}
