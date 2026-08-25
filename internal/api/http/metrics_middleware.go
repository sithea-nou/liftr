// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sithea-nou/liftr/internal/identity"
)

// unmatchedRoute is the single bounded fallback label for requests that match
// no registered route template (404s, 405s, probes). Raw request paths are
// never used as telemetry labels.
const unmatchedRoute = "unmatched"

// healthPathsSkipTelemetry mirrors healthPaths: probe traffic is excluded
// from metrics and access logs so infrastructure polling cannot drown real
// signals.
var healthPathsSkipTelemetry = healthPaths

// routeMatcher resolves one incoming request to its registered route
// template. Patterns use the same Go 1.22 method+wildcard syntax as the mux;
// the compiled matcher is built once from apiRoutes, which is the single
// source of truth for registration and OpenAPI drift — and now for bounded
// http.route labels.
type routeMatcher struct {
	method   string
	template string
	segments []string
}

func newRouteMatchers(routes []route) []routeMatcher {
	matchers := make([]routeMatcher, 0, len(routes))
	for _, rt := range routes {
		method := rt.Method
		pattern := rt.Pattern
		if space := strings.IndexByte(pattern, ' '); space >= 0 {
			method = pattern[:space]
			pattern = pattern[space+1:]
		}
		matchers = append(matchers, routeMatcher{
			method:   method,
			template: pattern,
			segments: strings.Split(strings.Trim(pattern, "/"), "/"),
		})
	}
	return matchers
}

// match returns the registered template for this request. Method mismatches
// still resolve to their path template so a 405 cannot inflate "unmatched".
func (m routeMatcher) matches(method, path string) bool {
	if m.method != method && !(method == http.MethodHead && m.method == http.MethodGet) {
		return false
	}
	return segmentsMatch(m.segments, path)
}

func segmentsMatch(template []string, path string) bool {
	requested := strings.Split(strings.Trim(path, "/"), "/")
	if len(requested) != len(template) {
		return false
	}
	for i, want := range template {
		if strings.HasPrefix(want, "{") && strings.HasSuffix(want, "}") {
			if requested[i] == "" {
				return false
			}
			continue
		}
		if want != requested[i] {
			return false
		}
	}
	return true
}

func resolveRoute(matchers []routeMatcher, method, path string) string {
	for _, matcher := range matchers {
		if matcher.matches(method, path) {
			return matcher.template
		}
	}
	// Method mismatch on a known path keeps its template label.
	for _, matcher := range matchers {
		if matcher.method != method && segmentsMatch(matcher.segments, path) {
			return matcher.template
		}
	}
	return unmatchedRoute
}

// withMetrics instruments every non-health request with standard OTel HTTP
// semantics (bounded route templates, status codes, duration) and emits one
// structured access-log record. PrincipalID is deliberately omitted from
// generic read access logs; it appears only on mutation routes where it
// materially aids mutation diagnostics (ADR-0018).
// requestTelemetryState is shared mutable context placed by the metrics
// middleware so inner layers (authentication) can attach the authenticated
// principal for mutation access records without re-wrapping contexts.
type requestTelemetryState struct {
	mu        sync.Mutex
	principal identity.Principal
}

type telemetryStateKey struct{}

func (s *requestTelemetryState) setPrincipal(principal identity.Principal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.principal = principal
}

func (s *requestTelemetryState) currentPrincipal() (identity.Principal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.principal, s.principal.ID != ""
}

func telemetryStateFrom(ctx context.Context) *requestTelemetryState {
	state, _ := ctx.Value(telemetryStateKey{}).(*requestTelemetryState)
	return state
}

func withMetrics(logger *slog.Logger, telemetry Telemetry, matchers []routeMatcher, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, health := healthPathsSkipTelemetry[r.URL.Path]; health {
			next.ServeHTTP(w, r)
			return
		}
		route := resolveRoute(matchers, r.Method, r.URL.Path)
		started := time.Now()
		tracked := &statusResponseWriter{ResponseWriter: w}
		state := &requestTelemetryState{}
		r = r.WithContext(context.WithValue(r.Context(), telemetryStateKey{}, state))
		if telemetry != nil {
			telemetry.HTTPRequestStarted()
		}
		defer func() {
			status := tracked.status()
			duration := time.Since(started)
			if telemetry != nil {
				telemetry.HTTPRequestFinished(route, requestMethodLabel(r), status, duration)
			}
			logAccessRecord(logger, r, route, status, duration, state)
		}()
		next.ServeHTTP(tracked, r)
	})
}

// statusResponseWriter captures the final status without altering writes.
type statusResponseWriter struct {
	http.ResponseWriter
	writtenStatus int
}

func (w *statusResponseWriter) WriteHeader(status int) {
	if w.writtenStatus == 0 {
		w.writtenStatus = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusResponseWriter) Write(payload []byte) (int, error) {
	if w.writtenStatus == 0 {
		w.writtenStatus = http.StatusOK
	}
	return w.ResponseWriter.Write(payload)
}

func (w *statusResponseWriter) status() int {
	if w.writtenStatus == 0 {
		return http.StatusOK
	}
	return w.writtenStatus
}

func requestMethodLabel(r *http.Request) string {
	// HEAD is recorded under GET per semantic-convention guidance so method
	// dimensions stay bounded and comparable.
	if r.Method == http.MethodHead {
		return http.MethodGet
	}
	return r.Method
}

// logAccessRecord renders one structured access record.
func logAccessRecord(logger *slog.Logger, r *http.Request, route string, status int, duration time.Duration, state *requestTelemetryState) {
	if logger == nil {
		return
	}
	args := []any{
		"request_id", RequestIDFromContext(r.Context()),
		"correlation_id", CorrelationIDFromContext(r.Context()),
		"method", requestMethodLabel(r),
		"route", route,
		"status", status,
		"duration_ms", float64(duration.Nanoseconds()) / 1e6,
	}
	if isMutationMethod(r.Method) && state != nil {
		if principal, ok := state.currentPrincipal(); ok {
			args = append(args, "principal_id", string(principal.ID))
		}
	}
	logger.Info("http_request", args...)
}

func isMutationMethod(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch:
		return true
	default:
		return false
	}
}
