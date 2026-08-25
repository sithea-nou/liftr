// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sithea-nou/liftr/internal/identity"
)

type requestIDKey struct{}
type principalKey struct{}

const maxBearerCredentialBytes = 8 * 1024

var healthPaths = map[string]struct{}{`/healthz`: {}, `/readyz`: {}}

func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw [16]byte
		requestID := "req-unknown"
		if _, err := rand.Read(raw[:]); err == nil {
			requestID = hex.EncodeToString(raw[:])
		}
		w.Header().Set("X-Request-ID", requestID)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, requestID)))
	})
}

func withAuthentication(auth Authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, health := healthPaths[r.URL.Path]; health {
			next.ServeHTTP(w, r)
			return
		}
		credential, present := bearerCredential(r)
		if !present || auth == nil {
			if anonymous, ok := auth.(AnonymousAuthenticator); ok && anonymous.AllowsAnonymous() {
				principal, err := auth.Authenticate(r.Context(), "")
				if err == nil && principal.ID != "" {
					next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal)))
					return
				}
			}
			writeUnauthenticated(w, r, false)
			return
		}
		if len(credential) > maxBearerCredentialBytes {
			writeUnauthenticated(w, r, true)
			return
		}
		principal, err := auth.Authenticate(r.Context(), credential)
		if err != nil || principal.ID == "" {
			writeUnauthenticated(w, r, true)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal)))
	})
}

func bearerCredential(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	scheme, credential, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	credential = strings.TrimSpace(credential)
	if credential == "" || strings.ContainsAny(credential, " \t\r\n\x00") {
		return "", false
	}
	return credential, true
}

func withRecovery(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				if logger != nil {
					logger.Error("admin request panicked", "request_id", requestID(r.Context()), "error_class", "panic")
				}
				writeProblem(w, r, codeInternal, "an unexpected internal error occurred", nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func principal(ctx context.Context) (identity.Principal, bool) {
	value, ok := ctx.Value(principalKey{}).(identity.Principal)
	return value, ok && value.ID != ""
}

func requestID(ctx context.Context) string {
	value, _ := ctx.Value(requestIDKey{}).(string)
	return value
}

func writeUnauthenticated(w http.ResponseWriter, r *http.Request, presented bool) {
	challenge := `Bearer realm="liftr-operator"`
	if presented {
		challenge = `Bearer realm="liftr-operator", error="invalid_token"`
	}
	w.Header().Set("WWW-Authenticate", challenge)
	writeProblem(w, r, codeUnauthenticated, "valid operator bearer credentials are required", nil)
}
