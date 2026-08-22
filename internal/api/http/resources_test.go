// SPDX-License-Identifier: Apache-2.0

package httpapi_test

import (
	"net/http"
	"sort"
	"strings"
	"testing"
)

func TestCreateResourceAdmitsAsynchronously(t *testing.T) {
	f := newFixture(t)
	response := f.createResource(t, "resource-create", map[string]any{"size": int64(10)})
	body := decodeBody(t, response)

	if location := header(response, "Location"); location != "/v1/resources/resource-create" {
		t.Fatalf("Location = %q, want the resource URL", location)
	}
	link := header(response, "Link")
	if !strings.Contains(link, `rel="monitor"`) || !strings.HasPrefix(link, "</v1/operations/") {
		t.Fatalf("Link = %q, want a monitor link to the operation", link)
	}
	if got := header(response, "Liftr-Generation"); got != "1" {
		t.Fatalf("Liftr-Generation = %q, want 1", got)
	}
	assertNoStore(t, response, "create")
	assertNoETagHeaders(t, response, "create")

	for _, field := range []string{"id", "type", "owner", "generation", "spec", "status", "latestOperation", "createdAt", "updatedAt"} {
		if _, ok := body[field]; !ok {
			t.Fatalf("resource response missing %q: %v", field, body)
		}
	}
	if body["id"] != "resource-create" || body["generation"] != float64(1) {
		t.Fatalf("unexpected identity fields: %v", body)
	}
	status := body["status"].(map[string]any)
	if status["state"] != string("Pending") {
		t.Fatalf("status.state = %v, want Pending while admission is asynchronous", status["state"])
	}
	latest := body["latestOperation"].(map[string]any)
	if latest["capability"] != "create" || latest["state"] != "Pending" {
		t.Fatalf("latestOperation = %v, want pending create", latest)
	}
	operationID := latest["id"].(string)
	if href := latest["href"].(string); href != "/v1/operations/"+operationID {
		t.Fatalf("latestOperation.href = %q", href)
	}

	// The monitor Link must name the same operation as latestOperation.
	if !strings.Contains(link, "/v1/operations/"+operationID+">") {
		t.Fatalf("Link %q does not reference operation %q", link, operationID)
	}
	if header(response, "Idempotency-Replayed") != "" {
		t.Fatal("first create must not be marked as replayed")
	}
}

func TestGetResourceReturnsRetainedRepresentation(t *testing.T) {
	f := newFixture(t)
	f.createResource(t, "resource-get", map[string]any{"size": int64(3)})

	response := f.request(t, http.MethodGet, "/v1/resources/resource-get", nil, nil)
	expectStatus(t, response, http.StatusOK)
	if got := header(response, "Liftr-Generation"); got != "1" {
		t.Fatalf("Liftr-Generation = %q, want 1", got)
	}
	assertNoStore(t, response, "get resource")
	assertNoETagHeaders(t, response, "get resource")
	body := decodeBody(t, response)
	if body["id"] != "resource-get" || body["generation"] != float64(1) {
		t.Fatalf("unexpected resource body: %v", body)
	}
	if body["spec"].(map[string]any)["size"] != float64(3) {
		t.Fatalf("spec did not round-trip: %v", body["spec"])
	}
}

func TestGetUnknownResourceIsProblemNotFound(t *testing.T) {
	f := newFixture(t)
	response := f.request(t, http.MethodGet, "/v1/resources/missing", nil, nil)
	expectProblem(t, response, http.StatusNotFound, "RESOURCE_NOT_FOUND")
}

func TestUpdateAdmitsWithConcreteGeneration(t *testing.T) {
	f := newFixture(t)
	f.createResource(t, "resource-update", map[string]any{"size": int64(1)})
	f.drainWorker(t)

	response := f.request(t, http.MethodPut, "/v1/resources/resource-update", map[string]string{
		"Idempotency-Key":     "update-1",
		"If-Liftr-Generation": "1",
	}, map[string]any{"spec": map[string]any{"size": int64(2)}})
	expectStatus(t, response, http.StatusAccepted)

	location := header(response, "Location")
	if !strings.HasPrefix(location, "/v1/operations/") {
		t.Fatalf("update Location = %q, want an operation URL", location)
	}
	if got := header(response, "Liftr-Generation"); got != "2" {
		t.Fatalf("Liftr-Generation = %q, want 2", got)
	}
	assertNoStore(t, response, "update")
	link := header(response, "Link")
	operationID := strings.TrimSuffix(strings.TrimPrefix(location, "/v1/operations/"), "")
	if !strings.Contains(link, "/v1/operations/"+operationID+">") {
		t.Fatalf("Link %q does not match Location operation %q", link, operationID)
	}
	body := decodeBody(t, response)
	if body["generation"] != float64(2) {
		t.Fatalf("body generation = %v, want 2 after admitted revision", body["generation"])
	}
	if body["spec"].(map[string]any)["size"] != float64(2) {
		t.Fatalf("body spec was not updated: %v", body["spec"])
	}
}

func TestDeleteAdmitsWithConcreteGeneration(t *testing.T) {
	f := newFixture(t)
	f.createResource(t, "resource-delete", map[string]any{"size": int64(1)})
	f.drainWorker(t)

	response := f.request(t, http.MethodDelete, "/v1/resources/resource-delete", map[string]string{
		"Idempotency-Key":     "delete-1",
		"If-Liftr-Generation": "1",
	}, nil)
	expectStatus(t, response, http.StatusAccepted)
	if location := header(response, "Location"); !strings.HasPrefix(location, "/v1/operations/") {
		t.Fatalf("delete Location = %q, want an operation URL", location)
	}
	if got := header(response, "Liftr-Generation"); got != "1" {
		t.Fatalf("Liftr-Generation = %q, want unchanged 1 during delete", got)
	}
	assertNoStore(t, response, "delete")

	getResponse := f.request(t, http.MethodGet, "/v1/resources/resource-delete", nil, nil)
	expectStatus(t, getResponse, http.StatusOK)
	state := decodeBody(t, getResponse)["status"].(map[string]any)["state"]
	if state != "Deleting" {
		t.Fatalf("retained state after delete admission = %v, want Deleting", state)
	}
}

func TestGetOperationRepresentation(t *testing.T) {
	f := newFixture(t)
	createResponse := f.createResource(t, "resource-operation", map[string]any{"size": int64(4)})
	operationID := extractMonitorOperationID(t, header(createResponse, "Link"))

	response := f.request(t, http.MethodGet, "/v1/operations/"+operationID, nil, nil)
	expectStatus(t, response, http.StatusOK)
	if header(response, "Liftr-Generation") != "" {
		t.Fatal("an Operation response must not carry Liftr-Generation; it does not represent a Resource")
	}
	assertNoStore(t, response, "get operation")
	assertNoETagHeaders(t, response, "get operation")

	body := decodeBody(t, response)
	wantFields := []string{"id", "resourceId", "capability", "state", "targetGeneration", "requestedAt"}
	gotFields := make([]string, 0, len(body))
	for field := range body {
		gotFields = append(gotFields, field)
	}
	sort.Strings(gotFields)
	sort.Strings(wantFields)
	if strings.Join(gotFields, ",") != strings.Join(wantFields, ",") {
		t.Fatalf("pending operation fields = %v, want exactly %v", gotFields, wantFields)
	}
	if body["id"] != operationID || body["resourceId"] != "resource-operation" {
		t.Fatalf("operation identity mismatch: %v", body)
	}
	if body["capability"] != "create" || body["state"] != "Pending" || body["targetGeneration"] != float64(1) {
		t.Fatalf("unexpected operation body: %v", body)
	}

	unknown := f.request(t, http.MethodGet, "/v1/operations/op-missing", nil, nil)
	expectProblem(t, unknown, http.StatusNotFound, "OPERATION_NOT_FOUND")
}

func TestHealthEndpoints(t *testing.T) {
	f := newFixture(t)
	live := f.request(t, http.MethodGet, "/healthz", nil, nil)
	expectStatus(t, live, http.StatusOK)
	if header(live, "Liftr-Generation") != "" {
		t.Fatal("health responses never represent a Resource and must not emit Liftr-Generation")
	}
	if got := header(live, "Cache-Control"); got == "no-store" {
		t.Fatal("health endpoints are not versioned API responses; no-store is not required there")
	}

	ready := f.request(t, http.MethodGet, "/readyz", nil, nil)
	expectStatus(t, ready, http.StatusOK)
	if header(ready, "Liftr-Generation") != "" {
		t.Fatal("readyz must not emit Liftr-Generation")
	}
}

func TestUnsupportedMethodIsRejected(t *testing.T) {
	f := newFixture(t)
	response := f.request(t, http.MethodPatch, "/v1/resources/whatever", map[string]string{
		"Idempotency-Key":     "patch-key",
		"If-Liftr-Generation": "1",
	}, map[string]any{"spec": map[string]any{}})
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("PATCH status = %d, want 405", response.StatusCode)
	}
}

// extractMonitorOperationID reads the operation ID from a Link rel="monitor".
func extractMonitorOperationID(t *testing.T, link string) string {
	t.Helper()
	start := strings.Index(link, "</v1/operations/")
	end := strings.Index(link, ">")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("cannot parse monitor Link %q", link)
	}
	return strings.TrimPrefix(link[start+1:end], "/v1/operations/")
}

func assertNoStore(t *testing.T, response *http.Response, context string) {
	t.Helper()
	if got := header(response, "Cache-Control"); got != "no-store" {
		t.Fatalf("%s Cache-Control = %q, want no-store", context, got)
	}
}

func assertNoETagHeaders(t *testing.T, response *http.Response, context string) {
	t.Helper()
	for name := range response.Header {
		if name == "Etag" || name == "If-Match" {
			t.Fatalf("%s response unexpectedly carries %s", context, name)
		}
	}
}
