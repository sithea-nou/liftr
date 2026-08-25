// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
)

// m17InternalTelemetry records panic phases for middleware tests.
type m17InternalTelemetry struct {
	beforeCommit bool
	afterCommit  bool
}

func (c *m17InternalTelemetry) HTTPRequestStarted() {}
func (c *m17InternalTelemetry) HTTPRequestFinished(string, string, int, time.Duration) {
}
func (c *m17InternalTelemetry) HTTPPanicRecovered(beforeCommit bool) {
	if beforeCommit {
		c.beforeCommit = true
	} else {
		c.afterCommit = true
	}
}
func (c *m17InternalTelemetry) CorrelationIDDropped() {}
func (c *m17InternalTelemetry) AuthenticationObserved(bool, identity.AuthFailureReason) {
}
func (c *m17InternalTelemetry) OperationAdmitted(domain.Capability, bool) {}

// Before commit: a panic becomes one sanitized INTERNAL problem carrying the
// authoritative request ID; no panic detail reaches the response.
func TestPanicBeforeCommitRendersSanitizedProblem(t *testing.T) {
	telemetry := &m17InternalTelemetry{}
	handler := withRecovery(nil, telemetry, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := newRequestID()
		w.Header().Set("X-Request-ID", requestID)
		r = r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, requestID))
		panic("secret internal state leaked in panic value")
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/resources/x", nil)
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", recorder.Code)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "https://liftr.dev/problems/internal") || !strings.Contains(body, "requestId") {
		t.Fatalf("body is not the sanitized INTERNAL problem: %s", body)
	}
	if strings.Contains(body, "secret") || strings.Contains(body, "panic") {
		t.Fatalf("panic value leaked into response: %s", body)
	}
	if !telemetry.beforeCommit || telemetry.afterCommit {
		t.Fatalf("panic classification wrong: before=%t after=%t", telemetry.beforeCommit, telemetry.afterCommit)
	}
}

// After commit: the partial response is left alone — appending a Problem
// document would produce partial-success JSON followed by an error document
// in one response (ADR-0018).
func TestPanicAfterCommitDoesNotAppendProblem(t *testing.T) {
	telemetry := &m17InternalTelemetry{}
	handler := withRecovery(nil, telemetry, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"partial":`))
		panic("mid-stream failure")
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/resources/x", nil)
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("committed status changed to %d", recorder.Code)
	}
	body := recorder.Body.String()
	if body != `{"partial":` {
		t.Fatalf("after-commit body was altered: %q", body)
	}
	if strings.Contains(body, "liftr.dev/problems") {
		t.Fatalf("problem document appended after committed response: %q", body)
	}
	if !telemetry.afterCommit || telemetry.beforeCommit {
		t.Fatalf("panic classification wrong: before=%t after=%t", telemetry.beforeCommit, telemetry.afterCommit)
	}
}

// Panic values are bounded and control-character-safe in logs.
func TestSanitizedLogValuesAreBounded(t *testing.T) {
	hostile := strings.Repeat("x\r\ny\x00", 500)
	sanitized := sanitizeLogValue(hostile)
	if len(sanitized) > maxSanitizedValueRunes {
		t.Fatalf("sanitized value length %d exceeds bound", len(sanitized))
	}
	if strings.ContainsAny(sanitized, "\r\n\x00") {
		t.Fatal("control characters survived sanitization")
	}
}
