// SPDX-License-Identifier: Apache-2.0

package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/identity"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
)

type denyActionAuthorizer struct {
	action identity.Action
}

func (a denyActionAuthorizer) Authorize(_ context.Context, _ identity.Principal, action identity.Action, _ identity.ResourceTarget) error {
	if action == a.action {
		return errors.New("denied for test")
	}
	return nil
}

func updateResourceForHistory(t *testing.T, f *fixture, id, key string, generation uint64) string {
	t.Helper()
	response := f.request(t, http.MethodPut, "/v1/resources/"+id, map[string]string{
		"Idempotency-Key":     key,
		"If-Liftr-Generation": strconv.FormatUint(generation, 10),
	}, map[string]any{"spec": map[string]any{"size": int64(generation + 1)}})
	expectStatus(t, response, http.StatusAccepted)
	return extractMonitorOperationID(t, header(response, "Link"))
}

func seedFailedUpdate(t *testing.T, f *fixture, id string) string {
	t.Helper()
	f.createResource(t, id, map[string]any{"size": int64(1)})
	f.drainWorker(t)
	f.resolver.Providers[f.ref] = provisioningfake.New(provisioningfake.ModeFailure)
	failedID := updateResourceForHistory(t, f, id, "fail-"+id, 1)
	f.drainWorker(t)
	f.resolver.Providers[f.ref] = provisioningfake.New(provisioningfake.ModeSynchronous)
	failed := decodeBody(t, f.request(t, http.MethodGet, "/v1/operations/"+failedID, nil, nil))
	if failed["state"] != "Failed" {
		t.Fatalf("source operation state = %v, want Failed", failed["state"])
	}
	return failedID
}

func TestResourceOperationHistoryIsStableAndPublic(t *testing.T) {
	f := newFixture(t)
	create := f.createResource(t, "history-stable", map[string]any{"size": int64(1)})
	createID := decodeBody(t, create)["latestOperation"].(map[string]any)["id"].(string)
	f.drainWorker(t)
	updateID := updateResourceForHistory(t, f, "history-stable", "history-update", 1)
	f.drainWorker(t)

	first := f.request(t, http.MethodGet, "/v1/resources/history-stable/operations?limit=1", nil, nil)
	expectStatus(t, first, http.StatusOK)
	assertNoStore(t, first, "operation history")
	if header(first, "Liftr-Generation") != "" {
		t.Fatal("operation history must not emit Liftr-Generation")
	}
	firstBody := decodeBody(t, first)
	items := firstBody["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != updateID {
		t.Fatalf("first history page = %v, want latest update %q", items, updateID)
	}
	cursor, ok := firstBody["nextCursor"].(string)
	if !ok || cursor == "" || len(cursor) > 64 {
		t.Fatalf("nextCursor = %v", firstBody["nextCursor"])
	}
	for _, forbidden := range []string{"sequence", "phase", "phaseChangedAt", "recordVersion"} {
		if _, present := items[0].(map[string]any)[forbidden]; present {
			t.Fatalf("history item exposes %q: %v", forbidden, items[0])
		}
	}

	// A newer insertion after the cursor was issued cannot shift the next page.
	deleteResponse := f.request(t, http.MethodDelete, "/v1/resources/history-stable", map[string]string{
		"Idempotency-Key":     "history-delete",
		"If-Liftr-Generation": "2",
	}, nil)
	expectStatus(t, deleteResponse, http.StatusAccepted)

	second := f.request(t, http.MethodGet, "/v1/resources/history-stable/operations?limit=1&cursor="+url.QueryEscape(cursor), nil, nil)
	expectStatus(t, second, http.StatusOK)
	secondBody := decodeBody(t, second)
	secondItems := secondBody["items"].([]any)
	if len(secondItems) != 1 || secondItems[0].(map[string]any)["id"] != createID {
		t.Fatalf("second history page = %v, want original create %q", secondItems, createID)
	}
	if _, present := secondBody["nextCursor"]; present {
		t.Fatalf("final page unexpectedly has nextCursor: %v", secondBody)
	}

	empty := decodeBody(t, f.request(t, http.MethodGet, "/v1/resources/history-stable/operations?cursor="+url.QueryEscape(cursor), nil, nil))
	if _, ok := empty["items"].([]any); !ok {
		t.Fatalf("items is not an array: %v", empty)
	}
}

func TestResourceOperationHistoryRejectsStrictQueryAndBoundCursors(t *testing.T) {
	f := newFixture(t)
	f.createResource(t, "cursor-a", map[string]any{"size": int64(1)})
	f.drainWorker(t)
	updateResourceForHistory(t, f, "cursor-a", "cursor-a-update", 1)
	f.createResource(t, "cursor-b", map[string]any{"size": int64(1)})

	page := decodeBody(t, f.request(t, http.MethodGet, "/v1/resources/cursor-a/operations?limit=1", nil, nil))
	valid := page["nextCursor"].(string)
	alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, valid[len(valid)-1])
	if last < 0 || last%4 != 0 {
		t.Fatalf("unexpected canonical cursor suffix %q", valid)
	}
	noncanonical := valid[:len(valid)-1] + string(alphabet[last+1])
	cases := []string{
		"limit=0",
		"limit=101",
		"limit=1&limit=2",
		"unknown=1",
		"cursor=garbage",
		"cursor=c2_" + strings.TrimPrefix(valid, "c1_"),
		"cursor=c1_AA",
		"cursor=" + valid + "A",
		"cursor=" + noncanonical,
		"cursor=" + strings.Repeat("a", 65),
	}
	for _, query := range cases {
		response := f.request(t, http.MethodGet, "/v1/resources/cursor-a/operations?"+query, nil, nil)
		expectProblem(t, response, http.StatusBadRequest, "INVALID_ARGUMENT")
	}
	wrongResource := f.request(t, http.MethodGet, "/v1/resources/cursor-b/operations?cursor="+url.QueryEscape(valid), nil, nil)
	expectProblem(t, wrongResource, http.StatusBadRequest, "INVALID_ARGUMENT")
}

func TestResourceOperationHistoryHidesBeforeCursorValidation(t *testing.T) {
	f, _ := newFixtureWithPolicy(t, denyActionAuthorizer{action: identity.ActionResourceRead})
	f.createResource(t, "history-hidden", map[string]any{"size": int64(1)})

	denied := f.request(t, http.MethodGet, "/v1/resources/history-hidden/operations?cursor=malformed", nil, nil)
	missing := f.request(t, http.MethodGet, "/v1/resources/history-missing/operations?cursor=malformed", nil, nil)
	expectProblem(t, denied, http.StatusNotFound, "RESOURCE_NOT_FOUND")
	expectProblem(t, missing, http.StatusNotFound, "RESOURCE_NOT_FOUND")
}

func TestRetryPreconditionsResponseReplayAndConflict(t *testing.T) {
	f := newFixture(t)
	failedID := seedFailedUpdate(t, f, "retry-http")
	path := "/v1/operations/" + failedID + "/retry"

	expectProblem(t, f.request(t, http.MethodPost, path, map[string]string{"If-Liftr-Generation": "2"}, nil), http.StatusBadRequest, "INVALID_ARGUMENT")
	expectProblem(t, f.request(t, http.MethodPost, path, map[string]string{"Idempotency-Key": "retry-missing-generation"}, nil), http.StatusPreconditionRequired, "PRECONDITION_REQUIRED")
	expectProblem(t, f.request(t, http.MethodPost, path, map[string]string{"Idempotency-Key": "retry-zero", "If-Liftr-Generation": "0"}, nil), http.StatusBadRequest, "INVALID_ARGUMENT")
	expectProblem(t, f.request(t, http.MethodPost, path, map[string]string{"Idempotency-Key": "retry-body", "If-Liftr-Generation": "2"}, map[string]any{}), http.StatusBadRequest, "INVALID_ARGUMENT")
	oversized := sendRaw(t, f, http.MethodPost, path, map[string]string{"Idempotency-Key": "retry-oversized", "If-Liftr-Generation": "2"}, strings.Repeat(" ", (1<<20)+1))
	expectProblem(t, oversized, http.StatusBadRequest, "INVALID_ARGUMENT")
	stale := f.request(t, http.MethodPost, path, map[string]string{"Idempotency-Key": "retry-stale", "If-Liftr-Generation": "1"}, nil)
	staleProblem := expectProblem(t, stale, http.StatusConflict, "GENERATION_CONFLICT")
	if staleProblem["currentGeneration"] != float64(2) {
		t.Fatalf("currentGeneration = %v, want 2", staleProblem["currentGeneration"])
	}

	headers := map[string]string{"Idempotency-Key": "retry-success", "If-Liftr-Generation": "2"}
	first := sendRaw(t, f, http.MethodPost, path, headers, " \n\t")
	expectStatus(t, first, http.StatusAccepted)
	assertNoStore(t, first, "retry")
	if header(first, "Liftr-Generation") != "" {
		t.Fatal("retry response must not emit Liftr-Generation")
	}
	body := decodeBody(t, first)
	childID := body["id"].(string)
	if childID == failedID || body["retryOf"] != failedID || body["resourceId"] != "retry-http" || body["state"] != "Pending" || body["targetGeneration"] != float64(2) {
		t.Fatalf("unexpected retry operation: %v", body)
	}
	wantFields := []string{"id", "resourceId", "retryOf", "capability", "state", "targetGeneration", "requestedAt"}
	gotFields := make([]string, 0, len(body))
	for field := range body {
		gotFields = append(gotFields, field)
	}
	sort.Strings(gotFields)
	sort.Strings(wantFields)
	if strings.Join(gotFields, ",") != strings.Join(wantFields, ",") {
		t.Fatalf("retry fields = %v, want exactly %v", gotFields, wantFields)
	}
	if header(first, "Location") != "/v1/operations/"+childID || extractMonitorOperationID(t, header(first, "Link")) != childID {
		t.Fatalf("retry headers Location=%q Link=%q", header(first, "Location"), header(first, "Link"))
	}

	replay := f.request(t, http.MethodPost, path, headers, nil)
	expectStatus(t, replay, http.StatusAccepted)
	if header(replay, "Idempotency-Replayed") != "true" || decodeBody(t, replay)["id"] != childID {
		t.Fatal("retry replay did not return the original child Operation")
	}
	conflict := f.request(t, http.MethodPost, path, map[string]string{"Idempotency-Key": "retry-success", "If-Liftr-Generation": "1"}, nil)
	expectProblem(t, conflict, http.StatusConflict, "IDEMPOTENCY_CONFLICT")
	active := f.request(t, http.MethodPost, path, map[string]string{"Idempotency-Key": "retry-active", "If-Liftr-Generation": "2"}, nil)
	expectProblem(t, active, http.StatusConflict, "OPERATION_ACTIVE")

	source := decodeBody(t, f.request(t, http.MethodGet, "/v1/operations/"+failedID, nil, nil))
	if source["state"] != "Failed" || source["retryOf"] != nil {
		t.Fatalf("source Operation was mutated: %v", source)
	}
	resource := decodeBody(t, f.request(t, http.MethodGet, "/v1/resources/retry-http", nil, nil))
	if resource["generation"] != float64(2) {
		t.Fatalf("retry changed Resource generation: %v", resource["generation"])
	}

	// Replay is resolved before current generation, latest-source, and active
	// operation checks, so later lifecycle progress still returns the child
	// originally admitted under this key.
	f.drainWorker(t)
	updateResourceForHistory(t, f, "retry-http", "after-retry-update", 2)
	lateReplay := f.request(t, http.MethodPost, path, headers, nil)
	expectStatus(t, lateReplay, http.StatusAccepted)
	if header(lateReplay, "Idempotency-Replayed") != "true" || decodeBody(t, lateReplay)["id"] != childID {
		t.Fatal("late retry replay did not return the original child Operation")
	}
	current := decodeBody(t, f.request(t, http.MethodGet, "/v1/resources/retry-http", nil, nil))
	if current["generation"] != float64(3) {
		t.Fatalf("test did not advance to a later generation: %v", current["generation"])
	}
}

func TestRetryAuthorizationAndNotRetryableAreHiddenOrMapped(t *testing.T) {
	f, _ := newFixtureWithPolicy(t, denyActionAuthorizer{action: identity.ActionResourceRetry})
	failedID := seedFailedUpdate(t, f, "retry-denied")
	denied := f.request(t, http.MethodPost, "/v1/operations/"+failedID+"/retry", nil, map[string]any{"leak": true})
	expectProblem(t, denied, http.StatusNotFound, "OPERATION_NOT_FOUND")

	missing := f.request(t, http.MethodPost, "/v1/operations/no-such-operation/retry", nil, map[string]any{"leak": true})
	expectProblem(t, missing, http.StatusNotFound, "OPERATION_NOT_FOUND")

	allowed := newFixture(t)
	created := allowed.createResource(t, "retry-nonfailed", map[string]any{"size": int64(1)})
	operationID := decodeBody(t, created)["latestOperation"].(map[string]any)["id"].(string)
	notRetryable := allowed.request(t, http.MethodPost, "/v1/operations/"+operationID+"/retry", map[string]string{
		"Idempotency-Key":     "retry-nonfailed",
		"If-Liftr-Generation": "1",
	}, nil)
	expectProblem(t, notRetryable, http.StatusConflict, "OPERATION_NOT_RETRYABLE")
}

func sendRaw(t *testing.T, f *fixture, method, path string, headers map[string]string, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer tester")
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, req)
	response := recorder.Result()
	payload := append([]byte(nil), recorder.Body.Bytes()...)
	response.Body = io.NopCloser(bytes.NewReader(payload))
	return response
}
