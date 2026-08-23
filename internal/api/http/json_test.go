// SPDX-License-Identifier: Apache-2.0

package httpapi_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rawRequest sends a handcrafted body so malformed documents can be exercised.
// A default credential is injected when absent; tests needing explicit
// credential handling use requestWithoutAuth instead.
func (f *fixture) rawRequest(t *testing.T, method, path string, headers map[string]string, payload []byte) *http.Response {
	t.Helper()
	if _, present := headers["Authorization"]; !present {
		headers["Authorization"] = "Bearer tester"
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, req)
	response := recorder.Result()
	buffer := append([]byte(nil), recorder.Body.Bytes()...)
	response.Body = io.NopCloser(bytes.NewReader(buffer))
	return response
}

func envelopeWithSpec(idempotencyIndependentID, spec string) []byte {
	return []byte(`{"id":"` + idempotencyIndependentID + `","type":{"name":"` + testResourceType + `","version":"` + testResourceVersion + `"},` +
		`"owner":{"kind":"team","id":"platform"},"spec":` + spec + `}`)
}

func postEnvelope(t *testing.T, f *fixture, idempotencyKey string, payload []byte) *http.Response {
	t.Helper()
	return f.rawRequest(t, http.MethodPost, "/v1/resources", map[string]string{"Idempotency-Key": idempotencyKey}, payload)
}

func TestEnvelopeRejectsUnknownFields(t *testing.T) {
	f := newFixture(t)
	response := f.rawRequest(t, http.MethodPost, "/v1/resources", map[string]string{"Idempotency-Key": "unknown-field"},
		[]byte(`{"id":"r1","type":{"name":"`+testResourceType+`","version":"`+testResourceVersion+`"},"owner":{"kind":"team","id":"platform"},"spec":{},"fingerprint":"nope"}`))
	expectProblem(t, response, http.StatusBadRequest, "INVALID_ARGUMENT")

	nested := f.rawRequest(t, http.MethodPost, "/v1/resources", map[string]string{"Idempotency-Key": "unknown-nested"},
		[]byte(`{"id":"r1","type":{"name":"`+testResourceType+`","version":"`+testResourceVersion+`","stack":"pulumi"},"owner":{"kind":"team","id":"platform"},"spec":{}}`))
	expectProblem(t, nested, http.StatusBadRequest, "INVALID_ARGUMENT")
}

func TestSpecAllowsArbitraryKeys(t *testing.T) {
	f := newFixture(t)
	spec := map[string]any{
		"weird key name!": true,
		"nested":          map[string]any{"deep": []any{"a", nil}},
	}
	response := f.createResource(t, "resource-spec-keys", spec)
	body := decodeBody(t, response)
	stored := body["spec"].(map[string]any)
	if stored["weird key name!"] != true {
		t.Fatalf("arbitrary spec keys must be preserved: %v", stored)
	}
	if _, ok := stored["nested"].(map[string]any); !ok {
		t.Fatalf("nested spec objects must round-trip: %v", stored)
	}
}

// TestJSONNumberSemantics pins the approved normalization rule: integer
// literals stay int64, decimal and exponent literals stay float64, and
// overflowing integers are rejected instead of coerced. The canonical
// fingerprints of 1 and 1.0 differ, so replaying one Idempotency-Key across
// them conflicts instead of silently matching.
func TestJSONNumberSemantics(t *testing.T) {
	f := newFixture(t)

	integerThenDecimal := func(value string) *http.Response {
		return postEnvelope(t, f, "numbers-distinct", envelopeWithSpec("resource-number-distinct", `{"n":`+value+`}`))
	}
	first := integerThenDecimal("1")
	expectStatus(t, first, http.StatusCreated)
	replayedAsDecimal := integerThenDecimal("1.0")
	expectProblem(t, replayedAsDecimal, http.StatusConflict, "IDEMPOTENCY_CONFLICT")

	exponentCreate := postEnvelope(t, f, "numbers-exponent", envelopeWithSpec("resource-exponent", `{"n":1e2}`))
	expectStatus(t, exponentCreate, http.StatusCreated)
	exponentBody := decodeBody(t, exponentCreate)
	if got := exponentBody["spec"].(map[string]any)["n"]; got != float64(100) {
		t.Fatalf("exponent literal normalized to %v, want float 100", got)
	}

	overflow := postEnvelope(t, f, "overflow", envelopeWithSpec("resource-overflow", `{"n":9223372036854775808}`))
	expectProblem(t, overflow, http.StatusBadRequest, "INVALID_ARGUMENT")

	outOfRange := postEnvelope(t, f, "out-of-range", envelopeWithSpec("resource-out-of-range", `{"n":1e999}`))
	expectProblem(t, outOfRange, http.StatusBadRequest, "INVALID_ARGUMENT")

	malformed := postEnvelope(t, f, "malformed", []byte(`{"id":`))
	expectProblem(t, malformed, http.StatusBadRequest, "INVALID_ARGUMENT")

	trailing := postEnvelope(t, f, "trailing", []byte(`{"id":"r","type":{"name":"x","version":"y"},"owner":{"kind":"k","id":"i"},"spec":{}} {}`))
	expectProblem(t, trailing, http.StatusBadRequest, "INVALID_ARGUMENT")

	arraySpec := postEnvelope(t, f, "array-spec", envelopeWithSpec("resource-array-spec", `[1]`))
	expectProblem(t, arraySpec, http.StatusBadRequest, "INVALID_ARGUMENT")

	missingType := postEnvelope(t, f, "missing-type", []byte(`{"id":"r","owner":{"kind":"k","id":"i"},"spec":{}}`))
	expectProblem(t, missingType, http.StatusBadRequest, "INVALID_ARGUMENT")
}

// forbiddenMetadataKeys are internal concepts that must never appear as JSON
// fields in public v1 representations.
var forbiddenMetadataKeys = []string{
	"phase", "phaseChangedAt", "provisionerRef", "handle", "executionHandle",
	"attempt", "attemptNumber", "currentAttempt", "fingerprint",
	"recordVersion", "record_version", "stack", "workspace", "project",
	"lease", "leaseToken", "outbox", "worker", "etag", "ifMatch",
}

func assertNoInternalKeys(t *testing.T, document map[string]any, path string) {
	t.Helper()
	for key, value := range document {
		lower := strings.ToLower(key)
		for _, forbidden := range forbiddenMetadataKeys {
			if lower == strings.ToLower(forbidden) {
				t.Fatalf("%s exposes forbidden internal field %q", path, key)
			}
		}
		switch nested := value.(type) {
		case map[string]any:
			assertNoInternalKeys(t, nested, path+"."+key)
		case []any:
			for _, item := range nested {
				if nestedItem, ok := item.(map[string]any); ok {
					assertNoInternalKeys(t, nestedItem, path+"."+key)
				}
			}
		}
	}
}

// TestNoProviderOrPersistenceMetadataLeaks pins adversarial review items:
// provider/persistence metadata leak and phase leak across every endpoint.
func TestNoProviderOrPersistenceMetadataLeaks(t *testing.T) {
	f := newFixture(t)
	createResponse := f.createResource(t, "resource-leak-check", map[string]any{"size": int64(1)})
	createOperationID := extractMonitorOperationID(t, header(createResponse, "Link"))
	f.drainWorker(t)

	updateResponse := f.request(t, http.MethodPut, "/v1/resources/resource-leak-check", map[string]string{
		"Idempotency-Key":     "leak-update",
		"If-Liftr-Generation": "1",
	}, map[string]any{"spec": map[string]any{"size": int64(2)}})
	expectStatus(t, updateResponse, http.StatusAccepted)

	targets := []struct {
		name     string
		document map[string]any
	}{
		{"create", decodeBody(t, createResponse)},
		{"update", decodeBody(t, updateResponse)},
		{"get resource", decodeBody(t, f.request(t, http.MethodGet, "/v1/resources/resource-leak-check", nil, nil))},
		{"get operation", decodeBody(t, f.request(t, http.MethodGet, "/v1/operations/"+createOperationID, nil, nil))},
	}
	for _, target := range targets {
		assertNoInternalKeys(t, target.document, target.name)
	}
}

// TestDeleteRejectsAnyBody pins that DELETE accepts no request body,
// including malformed payloads that would fail JSON parsing.
func TestDeleteRejectsAnyBody(t *testing.T) {
	f := newFixture(t)
	f.createResource(t, "resource-delete-body", map[string]any{"size": int64(1)})
	f.drainWorker(t)

	for name, payload := range map[string][]byte{
		"empty object": []byte(`{}`),
		"null":         []byte(`null`),
		"malformed":    []byte(`not-json`),
		"spec object":  []byte(`{"spec":{}}`),
	} {
		response := f.rawRequest(t, http.MethodDelete, "/v1/resources/resource-delete-body", map[string]string{
			"Idempotency-Key":     "delete-body-" + name,
			"If-Liftr-Generation": "1",
		}, payload)
		expectProblem(t, response, http.StatusBadRequest, "INVALID_ARGUMENT")
	}
}

// TestIfMatchIsNeverHonored pins that no ETag validator exists in v1: an
// If-Match precondition cannot substitute for If-Liftr-Generation.
func TestIfMatchIsNeverHonored(t *testing.T) {
	f := newFixture(t)
	f.createResource(t, "resource-if-match", map[string]any{"size": int64(1)})
	response := f.request(t, http.MethodPut, "/v1/resources/resource-if-match", map[string]string{
		"Idempotency-Key": "if-match-attempt",
		"If-Match":        `W/"1"`,
	}, map[string]any{"spec": map[string]any{"size": int64(3)}})
	expectProblem(t, response, http.StatusPreconditionRequired, "PRECONDITION_REQUIRED")
}
