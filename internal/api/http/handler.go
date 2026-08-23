// SPDX-License-Identifier: Apache-2.0

// Package httpapi implements Liftr's public v1 Resource and Operation HTTP
// contract on top of the standard library. The transport layer only talks to
// application use cases; it never reaches repositories, provisioners, or
// lifecycle policy directly.
package httpapi

import (
	"net/http"

	"github.com/sithea-nou/liftr/internal/application"
)

// Deps carries the application boundary the transport is allowed to use.
// Auth authenticates bearer credentials into principals. It fails closed:
// when nil, every versioned request is answered 401 and health endpoints
// remain reachable (ADR-0012).
type Deps struct {
	Service *application.Service
	Auth    Authenticator
}

type handler struct {
	service *application.Service
}

// route is one entry of the versioned API surface. The table is the single
// source of truth for runtime registration and the OpenAPI drift test.
type route struct {
	Method  string
	Pattern string
	Handle  http.HandlerFunc
}

func apiRoutes(h *handler) []route {
	return []route{
		{Method: http.MethodPost, Pattern: "/v1/resources", Handle: h.createResource},
		{Method: http.MethodGet, Pattern: "/v1/resources/{id}", Handle: h.getResource},
		{Method: http.MethodPut, Pattern: "/v1/resources/{id}", Handle: h.updateResource},
		{Method: http.MethodDelete, Pattern: "/v1/resources/{id}", Handle: h.deleteResource},
		{Method: http.MethodGet, Pattern: "/v1/resource-types", Handle: h.listResourceTypes},
		{Method: http.MethodGet, Pattern: "/v1/resource-types/{name}/{version}", Handle: h.getResourceType},
		{Method: http.MethodGet, Pattern: "/v1/operations/{id}", Handle: h.getOperation},
		{Method: http.MethodGet, Pattern: "/healthz", Handle: h.healthz},
		{Method: http.MethodGet, Pattern: "/readyz", Handle: h.readyz},
	}
}

// NewHandler returns the HTTP handler implementing the approved v1 contract.
// Middleware order is request identity first (so credential failures still
// carry an authoritative X-Request-ID), then authentication, then routing.
func NewHandler(deps Deps) http.Handler {
	h := &handler{service: deps.Service}
	mux := http.NewServeMux()
	for _, rt := range apiRoutes(h) {
		mux.HandleFunc(rt.Method+" "+rt.Pattern, rt.Handle)
	}
	return withRequestIdentity(withAuthentication(deps.Auth, mux))
}
