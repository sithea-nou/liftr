// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

type requestIDContextKey struct{}

// withRequestIdentity assigns an authoritative server-generated X-Request-ID
// to every response. A client-supplied X-Correlation-ID is optional and stays
// strictly separate: it is echoed verbatim and never used as or merged into
// the request identifier.
func withRequestIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := newRequestID()
		w.Header().Set("X-Request-ID", requestID)
		if correlation := r.Header.Get("X-Correlation-ID"); correlation != "" {
			w.Header().Set("X-Correlation-ID", correlation)
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, requestID)))
	})
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
