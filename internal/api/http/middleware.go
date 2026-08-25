// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
)

type requestIDContextKey struct{}

type principalContextKey struct{}

type correlationIDContextKey struct{}

// MaxCorrelationIDBytes bounds a client-supplied X-Correlation-ID. It is a
// diagnostic identifier, never an authority; oversized or non-printable
// values are dropped rather than echoed.
const MaxCorrelationIDBytes = 128

// Telemetry is the transport-owned observability port. Composition injects
// one implementation; this package never imports a telemetry library.
// Every method must be safe on a nil receiver path via the guarded helpers
// below.
type Telemetry interface {
	HTTPRequestStarted()
	HTTPRequestFinished(route string, method string, status int, duration time.Duration)
	HTTPPanicRecovered(beforeCommit bool)
	CorrelationIDDropped()
	AuthenticationObserved(success bool, reason identity.AuthFailureReason)
	OperationAdmitted(capability domain.Capability, retry bool)
}

// Authenticator turns one presented bearer credential into an authenticated
// principal. It is a transport-owned consumer port: concrete implementations
// (the M11 RFC 9068 OIDC verifier) satisfy it structurally without this
// package importing them. Implementations must never return raw protocol
// errors; transports render only curated problems (ADR-0012).
type Authenticator interface {
	Authenticate(ctx context.Context, credential string) (identity.Principal, error)
}

// AnonymousAuthenticator is optionally implemented by authenticators that
// accept requests with no credential at all. Only the explicit development
// insecure mode implements it; secured OIDC composition never does, so a
// missing credential always answers 401 there (ADR-0012).
type AnonymousAuthenticator interface {
	Authenticator
	AllowsAnonymous() bool
}

// healthPaths are unauthenticated by contract: process liveness and readiness
// must stay reachable for infrastructure probes regardless of credentials.
var healthPaths = map[string]struct{}{
	"/healthz": {},
	"/readyz":  {},
}

// withRequestIdentity assigns an authoritative server-generated X-Request-ID
// to every response. A client-supplied X-Correlation-ID is optional and stays
// strictly separate from the request identifier: it is echoed only after
// sanitization (bounded, printable ASCII), so hostile header content can
// never reach response headers or operator logs (ADR-0018).
func withRequestIdentity(telemetry Telemetry, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := newRequestID()
		w.Header().Set("X-Request-ID", requestID)
		if correlation := r.Header.Get("X-Correlation-ID"); correlation != "" {
			sanitized, ok := sanitizeCorrelationID(correlation)
			if ok {
				w.Header().Set("X-Correlation-ID", sanitized)
				r = r.WithContext(context.WithValue(r.Context(), correlationIDContextKey{}, sanitized))
			} else if telemetry != nil {
				telemetry.CorrelationIDDropped()
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, requestID)))
	})
}

// sanitizeCorrelationID accepts trimmed printable ASCII identifiers up to
// MaxCorrelationIDBytes and rejects everything else.
func sanitizeCorrelationID(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > MaxCorrelationIDBytes {
		return "", false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < 0x21 || value[i] > 0x7e {
			return "", false
		}
	}
	return value, true
}

// CorrelationIDFromContext returns the sanitized caller-supplied correlation
// ID for this request, if one was accepted.
func CorrelationIDFromContext(ctx context.Context) string {
	value, _ := ctx.Value(correlationIDContextKey{}).(string)
	return value
}

func newRequestID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "req-unknown"
	}
	return hex.EncodeToString(raw[:])
}

// RequestIDFromContext returns the authoritative request ID assigned by the
// transport for inclusion in Problem responses.
func RequestIDFromContext(ctx context.Context) string {
	value, _ := ctx.Value(requestIDContextKey{}).(string)
	return value
}

// MaxBearerCredentialBytes is Liftr's explicit limit on one bearer
// credential. RFC 9068 access tokens are typically well under 4 KiB; 8 KiB
// accepts every realistic token while keeping a conservative fixed ceiling.
// Oversized credentials are refused before any JWT parsing or JWKS work, and
// the exact length never appears in errors or logs (ADR-0012).
const MaxBearerCredentialBytes = 8 * 1024

// withAuthentication authenticates every versioned request before routing and
// places the resulting principal on the request context. Health endpoints are
// always reachable without credentials.
//
// The check fails closed: without a configured authenticator, no request can
// authenticate. Credential failures produce one identical curated problem
// body regardless of failure reason, plus WWW-Authenticate per RFC 6750 —
// error="invalid_token" only when a credential was presented and rejected
// (ADR-0012).
func withAuthentication(auth Authenticator, telemetry Telemetry, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, health := healthPaths[r.URL.Path]; health {
			next.ServeHTTP(w, r)
			return
		}
		credential, present := bearerCredential(r)
		if !present || auth == nil {
			if anonymous, ok := auth.(AnonymousAuthenticator); ok && anonymous.AllowsAnonymous() {
				principal, err := auth.Authenticate(r.Context(), "")
				if err != nil || principal.ID == "" {
					writeUnauthenticated(w, r, false)
					return
				}
				if state := telemetryStateFrom(r.Context()); state != nil {
					state.setPrincipal(principal)
				}
				ctx := context.WithValue(r.Context(), principalContextKey{}, principal)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			if telemetry != nil {
				telemetry.AuthenticationObserved(false, identity.AuthFailureMissingCredential)
			}
			writeUnauthenticated(w, r, false)
			return
		}
		if len(credential) > MaxBearerCredentialBytes {
			// Refuse before any token processing: no parsing, no key lookup,
			// no refetch. The challenge says only that a presented credential
			// was rejected.
			if telemetry != nil {
				telemetry.AuthenticationObserved(false, identity.AuthFailureMalformed)
			}
			writeUnauthenticated(w, r, true)
			return
		}
		principal, err := auth.Authenticate(r.Context(), credential)
		if err != nil || principal.ID == "" {
			// The concrete verifier reports its own typed outcome through the
			// observer hook; this fallback covers nil-auth and empty-principal
			// paths only.
			if telemetry != nil && err == nil {
				telemetry.AuthenticationObserved(false, identity.AuthFailureOther)
			}
			writeUnauthenticated(w, r, true)
			return
		}
		if state := telemetryStateFrom(r.Context()); state != nil {
			state.setPrincipal(principal)
		}
		ctx := context.WithValue(r.Context(), principalContextKey{}, principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bearerCredential extracts the token of exactly one Bearer challenge. Scheme
// matching is case-insensitive per RFC 7235; anything else is treated as no
// usable credential.
func bearerCredential(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	scheme, credential, found := strings.Cut(header, " ")
	if !found {
		return "", false
	}
	if !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	credential = strings.TrimSpace(credential)
	if credential == "" || strings.ContainsAny(credential, " \t\r\n\x00") {
		return "", false
	}
	return credential, true
}

// PrincipalFromContext returns the authenticated principal assigned by the
// authentication middleware.
func PrincipalFromContext(ctx context.Context) (identity.Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(identity.Principal)
	return principal, ok && principal.ID != ""
}

// writeUnauthenticated renders the single curated credential-failure problem.
// presented distinguishes "no usable Authorization header" from "presented
// credential rejected" for the WWW-Authenticate challenge only; the body is
// identical so failure reasons are never revealed to clients.
func writeUnauthenticated(w http.ResponseWriter, r *http.Request, presented bool) {
	challenge := `Bearer realm="liftr"`
	if presented {
		challenge = `Bearer realm="liftr", error="invalid_token"`
	}
	w.Header().Set("WWW-Authenticate", challenge)
	writeProblem(w, r, CodeUnauthenticated, "valid bearer credentials are required", nil)
}
