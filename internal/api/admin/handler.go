// SPDX-License-Identifier: Apache-2.0

// Package admin implements the separate operator-only /admin/v1 HTTP plane.
// It talks only to application use cases: repositories, lifecycle policy, and
// provisioners are never reached directly.
package admin

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/identity"
)

// Authenticator is the admin transport's consumer port. Composition supplies
// a distinct RFC 9068 verifier configured with the required operator audience.
type Authenticator interface {
	Authenticate(context.Context, string) (identity.Principal, error)
}

type AnonymousAuthenticator interface {
	Authenticator
	AllowsAnonymous() bool
}

// Telemetry accepts bounded operator-plane request outcomes only. IDs,
// principals, refs, and diagnostic revisions never enter metric labels.
type Telemetry interface {
	OperatorRequest(action, result string)
	OperatorRecovery(kind, result string)
}

type Deps struct {
	Service   *application.Service
	Auth      Authenticator
	Logger    *slog.Logger
	Telemetry Telemetry
	Draining  func() bool
	// ProvisionerKinds is a diagnostic presentation map from private refs to
	// the bounded software-defined kind. Refs themselves never become labels.
	ProvisionerKinds map[application.ProvisionerRef]string
}

type handler struct {
	service          *application.Service
	logger           *slog.Logger
	telemetry        Telemetry
	drainCheck       func() bool
	provisionerKinds map[application.ProvisionerRef]string
}

type route struct {
	Method  string
	Pattern string
	Action  string
	Handle  http.HandlerFunc
}

func apiRoutes(h *handler) []route {
	return []route{
		{http.MethodGet, "/admin/v1/resources/{id}/diagnostics", "diagnostics_read", h.resourceDiagnostics},
		{http.MethodGet, "/admin/v1/operations/{id}/diagnostics", "diagnostics_read", h.operationDiagnostics},
		{http.MethodGet, "/admin/v1/work/{id}/diagnostics", "diagnostics_read", h.workDiagnostics},
		{http.MethodPost, "/admin/v1/operations/{id}/observe", "trigger_observe", h.triggerObserve},
		{http.MethodPost, "/admin/v1/resources/{id}/observe", "trigger_passive_observe", h.triggerPassiveObserve},
		{http.MethodPost, "/admin/v1/work/{id}/recover", "recover_dead_work", h.recoverDeadWork},
		{http.MethodGet, "/healthz", "health", h.healthz},
		{http.MethodGet, "/readyz", "health", h.readyz},
	}
}

// NewHandler returns only the admin routes plus tiny health/readiness probes.
// It never registers developer /v1 routes and the developer router never
// registers these routes (ADR-0021).
func NewHandler(deps Deps) http.Handler {
	h := &handler{
		service: deps.Service, logger: deps.Logger, telemetry: deps.Telemetry,
		drainCheck: deps.Draining, provisionerKinds: deps.ProvisionerKinds,
	}
	mux := http.NewServeMux()
	for _, rt := range apiRoutes(h) {
		mux.HandleFunc(rt.Method+" "+rt.Pattern, rt.Handle)
	}
	var chain http.Handler = mux
	chain = withAuthentication(deps.Auth, chain)
	chain = withRecovery(deps.Logger, chain)
	chain = withRequestID(chain)
	return chain
}
