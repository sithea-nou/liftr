// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/sithea-nou/liftr/internal/domain"
)

// getOperation returns the public v1 representation of one lifecycle
// Operation. OperationPhase is deliberately not part of v1, and no
// Liftr-Generation header is emitted because this response does not represent
// a Resource.
func (h *handler) getOperation(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w, r) {
		return
	}
	record, err := h.service.GetOperation(r.Context(), domain.OperationID(r.PathValue("id")))
	if err != nil {
		h.mapReadError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(newOperationDTO(record.Operation))
}
