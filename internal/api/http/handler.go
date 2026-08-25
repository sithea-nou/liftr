// SPDX-License-Identifier: Apache-2.0

// Package httpapi implements Liftr's public v1 Resource and Operation HTTP
// contract on top of the standard library. The transport layer only talks to
// application use cases; it never reaches repositories, provisioners, or
// lifecycle policy directly.
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/sithea-nou/liftr/internal/application"
)

// Deps carries the application boundary the transport is allowed to use.
// Auth authenticates bearer credentials into principals. It fails closed:
// when nil, every versioned request is answered 401 and health endpoints
// remain reachable (ADR-0012).
//
// Logger and Telemetry are optional observability seams; nil keeps behavior
// identical without instrumentation. Draining reports that the process is
// shutting down; readiness flips false before HTTP draining starts (ADR-0018).
type Deps struct {
	Service   *application.Service
	Auth      Authenticator
	Logger    *slog.Logger
	Telemetry Telemetry
	// Draining reports that graceful shutdown has begun; readiness answers
	// 503 while it returns true. Optional.
	Draining func() bool
}

type handler struct {
	service    *application.Service
	logger     *slog.Logger
	telemetry  Telemetry
	drainCheck func() bool
}

// route is one entry of the versioned API surface. The table is the single
// source of truth for runtime registration, the OpenAPI drift test, and the
// bounded http.route metric labels.
type route struct {
	Method  string
	Pattern string
	Handle  http.HandlerFunc
}

func apiRoutes(h *handler) []route {
	return []route{
		{Method: http.MethodPost, Pattern: "/v1/resources", Handle: h.createResource},
		{Method: http.MethodGet, Pattern: "/v1/resources", Handle: h.listResources},
		{Method: http.MethodGet, Pattern: "/v1/resources/{id}", Handle: h.getResource},
		{Method: http.MethodPut, Pattern: "/v1/resources/{id}", Handle: h.updateResource},
		{Method: http.MethodDelete, Pattern: "/v1/resources/{id}", Handle: h.deleteResource},
		{Method: http.MethodGet, Pattern: "/v1/resources/{id}/operations", Handle: h.listResourceOperations},
		{Method: http.MethodGet, Pattern: "/v1/resource-types", Handle: h.listResourceTypes},
		{Method: http.MethodGet, Pattern: "/v1/resource-types/{name}/{version}", Handle: h.getResourceType},
		{Method: http.MethodGet, Pattern: "/v1/operations/{id}", Handle: h.getOperation},
		{Method: http.MethodPost, Pattern: "/v1/operations/{id}/retry", Handle: h.retryOperation},
		{Method: http.MethodGet, Pattern: "/healthz", Handle: h.healthz},
		{Method: http.MethodGet, Pattern: "/readyz", Handle: h.readyz},
	}
}

// NewHandler returns the HTTP handler implementing the approved v1 contract.
//
// Middleware order (outermost first):
//  1. request identity — the authoritative request ID exists before anything
//     else so credential failures still carry one;
//  2. metrics + access log — observes final status including recovered panics
//     written by the recovery layer below it;
//  3. recovery — commitment-aware panic containment for everything inward;
//  4. authentication — then routing.
func NewHandler(deps Deps) http.Handler {
	h := &handler{service: deps.Service, logger: deps.Logger, telemetry: deps.Telemetry, drainCheck: deps.Draining}
	mux := http.NewServeMux()
	routes := apiRoutes(h)
	for _, rt := range routes {
		mux.HandleFunc(rt.Method+" "+rt.Pattern, rt.Handle)
	}
	var chain http.Handler = mux
	chain = withAuthentication(deps.Auth, deps.Telemetry, chain)
	chain = withRecovery(deps.Logger, deps.Telemetry, chain)
	chain = withMetrics(deps.Logger, deps.Telemetry, newRouteMatchers(routes), chain)
	chain = withRequestIdentity(deps.Telemetry, chain)
	return chain
}
