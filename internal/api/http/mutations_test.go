// SPDX-License-Identifier: Apache-2.0

package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
)

func TestUpdateRequiresConcreteGenerationHeader(t *testing.T) {
	f := newFixture(t)
	f.createResource(t, "resource-precondition", map[string]any{"size": int64(1)})

	missing := f.request(t, http.MethodPut, "/v1/resources/resource-precondition",
		map[string]string{"Idempotency-Key": "k"}, map[string]any{"spec": map[string]any{"size": int64(2)}})
	expectProblem(t, missing, http.StatusPreconditionRequired, "PRECONDITION_REQUIRED")

	wildcard := f.request(t, http.MethodPut, "/v1/resources/resource-precondition", map[string]string{
		"Idempotency-Key":     "k",
		"If-Liftr-Generation": "*",
	}, map[string]any{"spec": map[string]any{"size": int64(2)}})
	expectProblem(t, wildcard, http.StatusBadRequest, "INVALID_ARGUMENT")

	malformed := f.request(t, http.MethodPut, "/v1/resources/resource-precondition", map[string]string{
		"Idempotency-Key":     "k",
		"If-Liftr-Generation": "1.0",
	}, map[string]any{"spec": map[string]any{"size": int64(2)}})
	expectProblem(t, malformed, http.StatusBadRequest, "INVALID_ARGUMENT")

	zero := f.request(t, http.MethodPut, "/v1/resources/resource-precondition", map[string]string{
		"Idempotency-Key":     "k",
		"If-Liftr-Generation": "0",
	}, map[string]any{"spec": map[string]any{"size": int64(2)}})
	expectProblem(t, zero, http.StatusBadRequest, "INVALID_ARGUMENT")
}

func TestDeleteRequiresConcreteGenerationHeader(t *testing.T) {
	f := newFixture(t)
	f.createResource(t, "resource-delete-precondition", map[string]any{"size": int64(1)})

	missing := f.request(t, http.MethodDelete, "/v1/resources/resource-delete-precondition",
		map[string]string{"Idempotency-Key": "k"}, nil)
	expectProblem(t, missing, http.StatusPreconditionRequired, "PRECONDITION_REQUIRED")

	wildcard := f.request(t, http.MethodDelete, "/v1/resources/resource-delete-precondition", map[string]string{
		"Idempotency-Key":     "k",
		"If-Liftr-Generation": "*",
	}, nil)
	expectProblem(t, wildcard, http.StatusBadRequest, "INVALID_ARGUMENT")
}

func TestGenerationMismatchExposesCurrentGenerationOnly(t *testing.T) {
	f := newFixture(t)
	f.createResource(t, "resource-stale", map[string]any{"size": int64(1)})

	response := f.request(t, http.MethodPut, "/v1/resources/resource-stale", map[string]string{
		"Idempotency-Key":     "stale-update",
		"If-Liftr-Generation": "99",
	}, map[string]any{"spec": map[string]any{"size": int64(5)}})
	problemDocument := expectProblem(t, response, http.StatusConflict, "GENERATION_CONFLICT")
	current, ok := problemDocument["currentGeneration"].(float64)
	if !ok || current != 1 {
		t.Fatalf("currentGeneration = %v, want 1", problemDocument["currentGeneration"])
	}
	if detail, _ := problemDocument["detail"].(string); strings.Contains(detail, "expected") || strings.Contains(detail, "99") {
		t.Fatalf("detail leaks internal error text: %q", detail)
	}
}

func TestMutationWithoutIdempotencyKeyIsInvalid(t *testing.T) {
	f := newFixture(t)
	createBody := map[string]any{
		"id":   "resource-no-key",
		"type": map[string]string{"name": testResourceType, "version": testResourceVersion},
		"owner": map[string]string{
			"kind": "team",
			"id":   "platform",
		},
		"spec": map[string]any{},
	}
	create := f.request(t, http.MethodPost, "/v1/resources", nil, createBody)
	expectProblem(t, create, http.StatusBadRequest, "INVALID_ARGUMENT")

	// PUT and DELETE evaluate the Idempotency-Key requirement only after the
	// stored Resource is authorized, so they target an existing record here.
	f.createResource(t, "resource-existing", map[string]any{"size": int64(3)})
	update := f.request(t, http.MethodPut, "/v1/resources/resource-existing",
		map[string]string{"If-Liftr-Generation": "1"}, map[string]any{"spec": map[string]any{"size": int64(4)}})
	expectProblem(t, update, http.StatusBadRequest, "INVALID_ARGUMENT")

	deleteCall := f.request(t, http.MethodDelete, "/v1/resources/resource-existing",
		map[string]string{"If-Liftr-Generation": "1"}, nil)
	expectProblem(t, deleteCall, http.StatusBadRequest, "INVALID_ARGUMENT")
}

func TestCreateReplayReturnsOriginalOperationAndCurrentSnapshot(t *testing.T) {
	f := newFixture(t)
	first := f.createResource(t, "resource-replay", map[string]any{"size": int64(7)})
	firstBody := decodeBody(t, first)

	replay := f.request(t, http.MethodPost, "/v1/resources", map[string]string{"Idempotency-Key": "create-key-resource-replay"}, map[string]any{
		"id":   "resource-replay",
		"type": map[string]string{"name": testResourceType, "version": testResourceVersion},
		"owner": map[string]string{
			"kind": "team",
			"id":   "platform",
		},
		"spec": map[string]any{"size": int64(7)},
	})
	expectStatus(t, replay, http.StatusCreated)
	if header(replay, "Idempotency-Replayed") != "true" {
		t.Fatal("create replay must set Idempotency-Replayed: true")
	}
	if header(replay, "Location") != header(first, "Location") {
		t.Fatalf("replay Location = %q, want %q", header(replay, "Location"), header(first, "Location"))
	}
	if got, want := extractMonitorOperationID(t, header(replay, "Link")), firstBody["latestOperation"].(map[string]any)["id"]; got != want {
		t.Fatalf("replay monitor operation = %v, want original %v", got, want)
	}

	conflict := f.request(t, http.MethodPost, "/v1/resources", map[string]string{"Idempotency-Key": "create-key-resource-replay"}, map[string]any{
		"id":   "resource-replay-different",
		"type": map[string]string{"name": testResourceType, "version": testResourceVersion},
		"owner": map[string]string{
			"kind": "team",
			"id":   "platform",
		},
		"spec": map[string]any{"size": int64(7)},
	})
	expectProblem(t, conflict, http.StatusConflict, "IDEMPOTENCY_CONFLICT")
}

// TestReplayAfterLaterMutation pins the deliberate replay semantics:
// Location and Link keep identifying the original lifecycle Operation while
// the body reports the current Resource snapshot whose latestOperation may be
// newer than that Operation.
func TestReplayAfterLaterMutation(t *testing.T) {
	f := newFixture(t)
	createResponse := f.createResource(t, "resource-later-mutation", map[string]any{"size": int64(1)})
	createOperationID := decodeBody(t, createResponse)["latestOperation"].(map[string]any)["id"].(string)
	f.drainWorker(t)

	updateResponse := f.request(t, http.MethodPut, "/v1/resources/resource-later-mutation", map[string]string{
		"Idempotency-Key":     "later-update",
		"If-Liftr-Generation": "1",
	}, map[string]any{"spec": map[string]any{"size": int64(2)}})
	expectStatus(t, updateResponse, http.StatusAccepted)
	updateOperationID := extractMonitorOperationID(t, header(updateResponse, "Link"))
	if updateOperationID == createOperationID {
		t.Fatal("test setup produced one operation for two mutations")
	}

	replay := f.request(t, http.MethodPost, "/v1/resources", map[string]string{"Idempotency-Key": "create-key-resource-later-mutation"}, map[string]any{
		"id":   "resource-later-mutation",
		"type": map[string]string{"name": testResourceType, "version": testResourceVersion},
		"owner": map[string]string{
			"kind": "team",
			"id":   "platform",
		},
		"spec": map[string]any{"size": int64(1)},
	})
	expectStatus(t, replay, http.StatusCreated)
	if header(replay, "Idempotency-Replayed") != "true" {
		t.Fatal("late replay must be marked as replayed")
	}
	if location := header(replay, "Location"); location != "/v1/resources/resource-later-mutation" {
		t.Fatalf("create replay Location = %q, want the Resource URL", location)
	}
	monitorID := extractMonitorOperationID(t, header(replay, "Link"))
	if monitorID != createOperationID {
		t.Fatalf("replay Link identifies operation %q, want original %q", monitorID, createOperationID)
	}
	body := decodeBody(t, replay)
	latestID := body["latestOperation"].(map[string]any)["id"].(string)
	if latestID != updateOperationID {
		t.Fatalf("replay latestOperation = %q, want newer update operation %q", latestID, updateOperationID)
	}
	if body["generation"] != float64(2) || body["spec"].(map[string]any)["size"] != float64(2) {
		t.Fatalf("replay body is not the current snapshot: %v", body)
	}
}

func TestUpdateAndDeleteReplaysIdentifyOriginalOperation(t *testing.T) {
	f := newFixture(t)
	f.createResource(t, "resource-mutation-replay", map[string]any{"size": int64(1)})
	f.drainWorker(t)

	headers := map[string]string{
		"Idempotency-Key":     "mutation-replay-update",
		"If-Liftr-Generation": "1",
	}
	first := f.request(t, http.MethodPut, "/v1/resources/resource-mutation-replay", headers, map[string]any{"spec": map[string]any{"size": int64(9)}})
	expectStatus(t, first, http.StatusAccepted)

	replay := f.request(t, http.MethodPut, "/v1/resources/resource-mutation-replay", headers, map[string]any{"spec": map[string]any{"size": int64(9)}})
	expectStatus(t, replay, http.StatusAccepted)
	if header(replay, "Idempotency-Replayed") != "true" {
		t.Fatal("update replay must be marked as replayed")
	}
	if header(replay, "Location") != header(first, "Location") {
		t.Fatalf("update replay Location = %q, want the original operation URL %q", header(replay, "Location"), header(first, "Location"))
	}
	f.drainWorker(t)

	deleteHeaders := map[string]string{
		"Idempotency-Key":     "mutation-replay-delete",
		"If-Liftr-Generation": "2",
	}
	deleteFirst := f.request(t, http.MethodDelete, "/v1/resources/resource-mutation-replay", deleteHeaders, nil)
	expectStatus(t, deleteFirst, http.StatusAccepted)
	deleteReplay := f.request(t, http.MethodDelete, "/v1/resources/resource-mutation-replay", deleteHeaders, nil)
	expectStatus(t, deleteReplay, http.StatusAccepted)
	if header(deleteReplay, "Location") != header(deleteFirst, "Location") {
		t.Fatalf("delete replay Location = %q, want the original operation URL", header(deleteReplay, "Location"))
	}
	if header(deleteReplay, "Idempotency-Replayed") != "true" {
		t.Fatal("delete replay must be marked as replayed")
	}
}

func TestTombstoneLifecycle(t *testing.T) {
	f := newFixture(t)
	f.seedDeletedRecord(t, "resource-tombstone")

	get := f.request(t, http.MethodGet, "/v1/resources/resource-tombstone", nil, nil)
	expectStatus(t, get, http.StatusOK)
	body := decodeBody(t, get)
	state := body["status"].(map[string]any)["state"]
	if state != "Deleted" {
		t.Fatalf("retained tombstone state = %v, want Deleted", state)
	}
	if got := header(get, "Liftr-Generation"); got != "2" {
		t.Fatalf("tombstone Liftr-Generation = %q, want 2", got)
	}

	recreate := f.request(t, http.MethodPost, "/v1/resources", map[string]string{"Idempotency-Key": "tombstone-key"}, map[string]any{
		"id":   "resource-tombstone",
		"type": map[string]string{"name": testResourceType, "version": testResourceVersion},
		"owner": map[string]string{
			"kind": "team",
			"id":   "platform",
		},
		"spec": map[string]any{"size": int64(4)},
	})
	problemDocument := expectProblem(t, recreate, http.StatusConflict, "RESOURCE_ALREADY_EXISTS")
	if _, hasGeneration := problemDocument["currentGeneration"]; !hasGeneration {
		t.Fatal("tombstone conflict should expose currentGeneration when readable")
	}

	update := f.request(t, http.MethodPut, "/v1/resources/resource-tombstone", map[string]string{
		"Idempotency-Key":     "tombstone-update",
		"If-Liftr-Generation": "2",
	}, map[string]any{"spec": map[string]any{"size": int64(4)}})
	expectProblem(t, update, http.StatusConflict, "RESOURCE_STATE_CONFLICT")

	del := f.request(t, http.MethodDelete, "/v1/resources/resource-tombstone", map[string]string{
		"Idempotency-Key":     "tombstone-delete",
		"If-Liftr-Generation": "2",
	}, nil)
	expectProblem(t, del, http.StatusConflict, "RESOURCE_STATE_CONFLICT")
}

func TestUnknownResourceTypeIsUnprocessable(t *testing.T) {
	f := newFixture(t)
	response := f.request(t, http.MethodPost, "/v1/resources", map[string]string{"Idempotency-Key": "unknown-type"}, map[string]any{
		"id":   "resource-unknown-type",
		"type": map[string]string{"name": "NoSuchType", "version": "v1"},
		"owner": map[string]string{
			"kind": "team",
			"id":   "platform",
		},
		"spec": map[string]any{},
	})
	expectProblem(t, response, http.StatusUnprocessableEntity, "UNSUPPORTED_RESOURCE_TYPE")
}

func TestUpdateMissingResourceIsNotFound(t *testing.T) {
	f := newFixture(t)
	response := f.request(t, http.MethodPut, "/v1/resources/never-created", map[string]string{
		"Idempotency-Key":     "ghost-update",
		"If-Liftr-Generation": "1",
	}, map[string]any{"spec": map[string]any{}})
	expectProblem(t, response, http.StatusNotFound, "RESOURCE_NOT_FOUND")
}
