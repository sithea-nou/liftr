// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/sithea-nou/liftr/internal/application"
)

// healthz reports process liveness only. It performs no dependency probes and
// represents no Resource, so it carries neither Liftr-Generation nor
// versioned-response cache semantics.
func (h *handler) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// readyz reports whether the control-plane core is ready: PostgreSQL usable,
// schema verified at startup, and the process not draining. It deliberately
// does NOT gate on provisioners or authentication infrastructure — Resource
// reads and durable control-plane behavior remain available through backend
// and IdP outages, which surface through metrics and logs instead (ADR-0018).
func (h *handler) readyz(w http.ResponseWriter, r *http.Request) {
	if h.service == nil {
		writeProblem(w, r, CodePersistenceUnavailable, "durable state is not configured for this instance", nil)
		return
	}
	if h.draining() {
		writeProblem(w, r, CodePersistenceUnavailable, "the control plane is shutting down", nil)
		return
	}
	err := h.service.Transactions.Within(r.Context(), func(application.UnitOfWork) error { return nil })
	if err != nil {
		writeProblem(w, r, CodePersistenceUnavailable, "the control plane cannot currently reach durable state", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

// draining consults the composition-provided drain flag; nil means never.
func (h *handler) draining() bool {
	if h.drainCheck == nil {
		return false
	}
	return h.drainCheck()
}
