// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"fmt"
	"log/slog"
	"net/http"
)

// committedResponseWriter tracks whether any bytes/status were written, so
// panic recovery can respect response commitment: a Problem document is
// appended only when the response has not started. A panic after commit can
// never be turned into a clean RFC 9457 answer — appending one would produce
// a partial successful body followed by an error document (ADR-0018).
type committedResponseWriter struct {
	http.ResponseWriter
	committed bool
	status    int
}

func (w *committedResponseWriter) WriteHeader(status int) {
	if w.committed {
		return
	}
	w.committed = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *committedResponseWriter) Write(payload []byte) (int, error) {
	if !w.committed {
		w.committed = true
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(payload)
}

// withRecovery converts handler panics into safe outcomes:
//
//   - before commit: one generic sanitized INTERNAL problem plus requestId;
//   - after commit: nothing further is written; the partial response ends as
//     net/http terminates it, and the panic is logged and counted.
//
// No stack trace or panic value ever reaches a public response. The process
// stays alive: net/http isolates connections, and every durable invariant in
// Liftr is protected by transactional boundaries rather than handler success.
func withRecovery(logger *slog.Logger, telemetry Telemetry, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tracked := &committedResponseWriter{ResponseWriter: w}
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			beforeCommit := !tracked.committed
			if telemetry != nil {
				telemetry.HTTPPanicRecovered(beforeCommit)
			}
			logPanic(logger, r, recovered)
			if beforeCommit {
				writeProblem(tracked, r, CodeInternal, "an unexpected internal error occurred", nil)
			}
		}()
		next.ServeHTTP(tracked, r)
	})
}

// logPanic records the sanitized panic value with request correlation. Panic
// values pass the same redaction discipline as any log field: bounded length,
// control characters flattened.
func logPanic(logger *slog.Logger, r *http.Request, recovered any) {
	if logger == nil {
		return
	}
	logger.Error("request handler panicked",
		"error_class", "panic",
		"panic_value", sanitizeLogValue(recovered),
		"method", r.Method,
		"path", r.URL.Path,
		"request_id", RequestIDFromContext(r.Context()),
		"correlation_id", CorrelationIDFromContext(r.Context()),
	)
}

const maxSanitizedValueRunes = 512

func sanitizeLogValue(value any) string {
	raw := []rune(toString(value))
	if len(raw) > maxSanitizedValueRunes {
		raw = raw[:maxSanitizedValueRunes]
	}
	out := make([]rune, 0, len(raw))
	for _, char := range raw {
		if char < 0x20 || char == 0x7f {
			char = '?'
		}
		out = append(out, char)
	}
	return string(out)
}

func toString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case error:
		return typed.Error()
	default:
		return fmt.Sprintf("%v", typed)
	}
}
