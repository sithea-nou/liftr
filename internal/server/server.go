// SPDX-License-Identifier: Apache-2.0

// Package server assembles the Liftr HTTP surface. The versioned v1 API
// contract lives in internal/api/http; this package stays the process-level
// composition point until application dependencies are wired in production.
package server

import (
	"net/http"

	apihttp "github.com/sithea-nou/liftr/internal/api/http"
)

// NewHandler returns the HTTP handler for the Liftr server. Without a
// configured application service, health endpoints report readiness
// accurately and versioned endpoints answer with persistence-unavailable
// problems instead of failing silently.
func NewHandler() http.Handler {
	return apihttp.NewHandler(apihttp.Deps{})
}
