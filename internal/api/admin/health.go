// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"encoding/json"
	"net/http"

	"github.com/sithea-nou/liftr/internal/application"
)

func (h *handler) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (h *handler) readyz(w http.ResponseWriter, r *http.Request) {
	if h.service == nil {
		writeProblem(w, r, codePersistenceUnavailable, "durable operator state is not configured", nil)
		return
	}
	if h.draining() {
		writeProblem(w, r, codeAdminDraining, "the operator plane is shutting down", nil)
		return
	}
	if err := h.service.Transactions.Within(r.Context(), func(application.UnitOfWork) error { return nil }); err != nil {
		writeProblem(w, r, codePersistenceUnavailable, "the operator plane cannot currently reach durable state", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
