// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
)

// getOperation returns the public v1 representation of one lifecycle
// Operation to a principal authorized on the owning Resource. Operations have
// no independent ACL: authorization flows through the Resource's stored owner
// (ADR-0012), and denial renders the identical OPERATION_NOT_FOUND problem as
// absence so operation activity is never disclosed. OperationPhase is
// deliberately not part of v1, and no Liftr-Generation header is emitted
// because this response does not represent a Resource.
func (h *handler) getOperation(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w, r) {
		return
	}
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	record, err := h.service.GetOperation(r.Context(), principal, domain.OperationID(r.PathValue("id")))
	if err != nil {
		if errors.Is(err, application.ErrNotAuthorized) {
			hideOperation(w, r)
			return
		}
		h.mapReadError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(newOperationDTO(record.Operation))
}
