// SPDX-License-Identifier: Apache-2.0

// Package server assembles the Liftr HTTP surface. The versioned v1 API
// contract lives in internal/api/http; this package stays the process-level
// composition point until application dependencies are wired in production.
package server

import (
	"net/http"

	apihttp "github.com/sithea-nou/liftr/internal/api/http"
)

// NewHandler returns the bare HTTP handler for degraded health-only mode.
// Without a configured application service, health endpoints report readiness
// accurately. Versioned endpoints fail closed: without an authenticator they
// answer UNAUTHENTICATED problems, never open access (ADR-0012).
func NewHandler() http.Handler {
	return apihttp.NewHandler(apihttp.Deps{})
}
