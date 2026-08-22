// SPDX-License-Identifier: Apache-2.0

package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	apihttp "github.com/sithea-nou/liftr/internal/api/http"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/resourcetypes"
	"github.com/sithea-nou/liftr/internal/resourcetypes/postgresqldatabase"
)

// newCatalogFixture wires a real resourcetypes.Registry into the standard
// transport fixture so admission and discovery exercise the actual contract.
func newCatalogFixture(t *testing.T, contracts ...resourcetypes.Contract) *fixture {
	t.Helper()
	return newFixtureWithCatalog(t, mustRegistry(t, contracts...))
}

func mustRegistry(t *testing.T, contracts ...resourcetypes.Contract) *resourcetypes.Registry {
	t.Helper()
	registry, err := resourcetypes.NewRegistry(contracts...)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func mustPostgresContract(t *testing.T) resourcetypes.Contract {
	t.Helper()
	contract, err := postgresqldatabase.Contract()
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

// syntheticContract builds one minimal extra contract for ordering tests.
func syntheticContract(t *testing.T, name, version string) resourcetypes.Contract {
	t.Helper()
	typeValue, err := domain.NewResourceType(
		domain.ResourceTypeRef{Name: name, Version: version},
		name+" contract.",
		[]domain.Capability{domain.CapabilityCreate},
	)
	if err != nil {
		t.Fatal(err)
	}
	document := `{
	  "$schema": "https://json-schema.org/draft/2020-12/schema",
	  "$id": "urn:liftr:resource-type:` + name + `:` + version + `:spec",
	  "type": "object"
	}`
	contract, err := resourcetypes.NewContract(resourcetypes.ContractInput{
		Type:        typeValue,
		DisplayName: name + " Display",
		SpecSchema:  []byte(document),
	})
	if err != nil {
		t.Fatal(err)
	}
	return contract
}

func requestHandler(t *testing.T, handler http.Handler, method, path string, headers map[string]string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Result()
}

func TestListResourceTypesEndpoint(t *testing.T) {
	fixture := newCatalogFixture(t, mustPostgresContract(t))
	response := fixture.request(t, http.MethodGet, "/v1/resource-types", nil, nil)
	expectStatus(t, response, http.StatusOK)

	if got := header(response, "Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	document := decodeBody(t, response)
	items, ok := document["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %#v, want exactly one summary", document["items"])
	}
	summary, ok := items[0].(map[string]any)
	if !ok {
		t.Fatal("item is not an object")
	}
	want := map[string]any{
		"name":         "PostgreSQLDatabase",
		"version":      "v1",
		"displayName":  "PostgreSQL Database",
		"description":  "A managed PostgreSQL database requested through a provisioner-neutral contract.",
		"capabilities": []any{"create", "delete", "observe", "update"},
		"href":         "/v1/resource-types/PostgreSQLDatabase/v1",
	}
	for field, expected := range want {
		if got := summary[field]; !reflect.DeepEqual(got, expected) {
			t.Fatalf("summary.%s = %#v, want %#v", field, got, expected)
		}
	}
	for _, forbidden := range []string{"specSchema", "provisioner", "availability"} {
		if _, exists := summary[forbidden]; exists {
			t.Fatalf("list summary leaks %q", forbidden)
		}
	}
}

func TestListResourceTypesDeterministicOrder(t *testing.T) {
	fixture := newCatalogFixture(t,
		syntheticContract(t, "Widget", "v10"),
		mustPostgresContract(t),
		syntheticContract(t, "Widget", "v2"),
		syntheticContract(t, "Widget", "v1"),
	)
	want := []string{"PostgreSQLDatabase/v1", "Widget/v1", "Widget/v10", "Widget/v2"}
	var previous []string
	for range 3 {
		response := fixture.request(t, http.MethodGet, "/v1/resource-types", nil, nil)
		expectStatus(t, response, http.StatusOK)
		items := decodeBody(t, response)["items"].([]any)
		order := make([]string, 0, len(items))
		for _, raw := range items {
			item := raw.(map[string]any)
			order = append(order, item["name"].(string)+"/"+item["version"].(string))
		}
		if previous != nil && strings.Join(order, ",") != strings.Join(previous, ",") {
			t.Fatalf("ordering differs between calls: %v vs %v", order, previous)
		}
		previous = order
	}
	if strings.Join(previous, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v, want %v", previous, want)
	}
}

func TestListResourceTypesEmptyCatalog(t *testing.T) {
	fixture := newCatalogFixture(t)
	response := fixture.request(t, http.MethodGet, "/v1/resource-types", nil, nil)
	body := rawBody(t, response)
	if string(body) != "{\"items\":[]}\n" {
		t.Fatalf("empty catalog body = %q, want an empty items array", body)
	}
}

func TestGetResourceTypeEndpoint(t *testing.T) {
	fixture := newCatalogFixture(t, mustPostgresContract(t))
	response := fixture.request(t, http.MethodGet, "/v1/resource-types/PostgreSQLDatabase/v1", nil, nil)
	expectStatus(t, response, http.StatusOK)

	var document struct {
		Name         string          `json:"name"`
		Version      string          `json:"version"`
		DisplayName  string          `json:"displayName"`
		Capabilities []string        `json:"capabilities"`
		SpecSchema   json.RawMessage `json:"specSchema"`
	}
	if err := json.Unmarshal(rawBody(t, response), &document); err != nil {
		t.Fatal(err)
	}
	if document.Name != "PostgreSQLDatabase" || document.Version != "v1" || document.DisplayName != "PostgreSQL Database" {
		t.Fatalf("detail metadata = %#v", document)
	}
	if len(document.Capabilities) != 4 {
		t.Fatalf("capabilities = %v", document.Capabilities)
	}

	// The schema round-trips verbatim and keeps its stable URN $id.
	var schemaObject map[string]any
	if err := json.Unmarshal(document.SpecSchema, &schemaObject); err != nil {
		t.Fatal(err)
	}
	wantID := "urn:liftr:resource-type:PostgreSQLDatabase:v1:spec"
	if got := schemaObject["$id"]; got != wantID {
		t.Fatalf("$id = %v, want %s", got, wantID)
	}
	if schemaObject["additionalProperties"] != false {
		t.Fatal("schema lost strict unknown-property rejection")
	}
	required := schemaObject["required"].([]any)
	if len(required) != 3 {
		t.Fatalf("required = %v, want three fields", required)
	}

	published, err := resourcetypes.CompileSpecSchema(document.SpecSchema)
	if err != nil {
		t.Fatalf("published schema is not valid JSON Schema 2020-12: %v", err)
	}
	if published.ID() != wantID {
		t.Fatal("discovery schema lost its $id")
	}

	// Serialization compacts whitespace, so byte equality is impossible across
	// the wire; semantic equality with the registered document is the
	// invariant. Deep-compare parsed documents and pin that the published
	// bytes recompile to an identical digest of their own canonical form.
	var registeredObject, publishedObject any
	if err := json.Unmarshal([]byte(postgresqldatabase.SpecSchemaDocument()), &registeredObject); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(document.SpecSchema, &publishedObject); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(registeredObject, publishedObject) {
		t.Fatal("published schema differs semantically from the registered schema")
	}
	recompublished, err := resourcetypes.CompileSpecSchema(published.Document())
	if err != nil {
		t.Fatal(err)
	}
	if recompublished.Digest() != published.Digest() {
		t.Fatal("published schema is not byte-stable through discovery")
	}
}

func TestGetResourceTypeUnknown(t *testing.T) {
	fixture := newCatalogFixture(t, mustPostgresContract(t))
	response := fixture.request(t, http.MethodGet, "/v1/resource-types/MissingDatabase/v9", nil, nil)
	document := expectProblem(t, response, http.StatusNotFound, "RESOURCE_TYPE_NOT_FOUND")
	if document["type"] != "https://liftr.dev/problems/resource-type-not-found" {
		t.Fatalf("problem type = %v", document["type"])
	}
}

// TestDiscoveryNeverExposesImplementation pins invariant K across both
// discovery endpoints.
func TestDiscoveryNeverExposesImplementation(t *testing.T) {
	fixture := newCatalogFixture(t, mustPostgresContract(t))
	for _, path := range []string{"/v1/resource-types", "/v1/resource-types/PostgreSQLDatabase/v1"} {
		body := strings.ToLower(string(rawBody(t, fixture.request(t, http.MethodGet, path, nil, nil))))
		for _, forbidden := range []string{
			"provisionerref", "pulumi", "terraform", "crossplane",
			"cloudaccount", "subscription", "executionhandle", "leasetoken",
			"outbox", "stackname", "gitrepo", "\"namespace\"",
		} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s leaks implementation concept %q", path, forbidden)
			}
		}
	}
}

func TestDiscoveryWithoutServiceIsUnavailable(t *testing.T) {
	handler := apihttp.NewHandler(apihttp.Deps{})
	for _, path := range []string{"/v1/resource-types", "/v1/resource-types/x/y"} {
		response := requestHandler(t, handler, http.MethodGet, path, nil, nil)
		expectProblem(t, response, http.StatusServiceUnavailable, "PERSISTENCE_UNAVAILABLE")
	}
}

func invalidSpecCreateRequest(typeName string, spec map[string]any) map[string]any {
	return map[string]any{
		"id":    "db-1",
		"type":  map[string]string{"name": typeName, "version": "v1"},
		"owner": map[string]string{"kind": "team", "id": "platform"},
		"spec":  spec,
	}
}

// TestInvalidSpecProblemDetails pins the structured RESOURCE_SPEC_INVALID
// problem shape over the M7 create API using the real PostgreSQL contract.
func TestInvalidSpecProblemDetails(t *testing.T) {
	fixture := newCatalogFixture(t, mustPostgresContract(t))
	response := fixture.request(t, http.MethodPost, "/v1/resources",
		map[string]string{"Idempotency-Key": "invalid-spec-key"},
		invalidSpecCreateRequest("PostgreSQLDatabase", map[string]any{"storagGB": int64(5)}))
	document := expectProblem(t, response, http.StatusUnprocessableEntity, "RESOURCE_SPEC_INVALID")
	if document["title"] != "Invalid resource spec" {
		t.Fatalf("title = %v", document["title"])
	}

	rawViolations, ok := document["violations"].([]any)
	if !ok || len(rawViolations) != 4 {
		t.Fatalf("violations = %#v, want four entries", document["violations"])
	}
	keywords := make([]string, 0, len(rawViolations))
	for _, raw := range rawViolations {
		violation := raw.(map[string]any)
		path, _ := violation["path"].(string)
		keyword := violation["keyword"].(string)
		message := violation["message"].(string)
		keywords = append(keywords, keyword)
		switch keyword {
		case "additionalProperties":
			if path != "/storagGB" || message != `property "storagGB" is not permitted by this resource type` {
				t.Fatalf("unexpected unknown-property violation: %+v", violation)
			}
		case "required":
			if path != "" || !strings.HasPrefix(message, `property "`) {
				t.Fatalf("unexpected required violation: %+v", violation)
			}
		default:
			t.Fatalf("unexpected keyword %q", keyword)
		}
		if strings.Contains(strings.ToLower(message), "got ") || strings.Contains(message, "jsonschema") {
			t.Fatalf("violation leaks validator internals or submitted values: %+v", violation)
		}
	}
	// Deterministic order: root-pathed required violations sort before the
	// property-pathed unknown-field violation.
	if strings.Join(keywords, ",") != "required,required,required,additionalProperties" {
		t.Fatalf("violations are not deterministically ordered: %v", keywords)
	}
	if _, exists := document["truncated"]; exists {
		t.Fatal("small violation set must not report truncation")
	}
}

// TestInvalidSpecTruncatesViolations caps structured violations at ten with a
// truncated indicator.
func TestInvalidSpecTruncatesViolations(t *testing.T) {
	wideSchema := `{
	  "$schema": "https://json-schema.org/draft/2020-12/schema",
	  "$id": "urn:liftr:resource-type:Wide:v1:spec",
	  "type": "object",
	  "required": ["a","b","c","d","e","f","g","h","i","j","k"],
	  "properties": {"a": {"type": "object"}}
	}`
	typeValue, err := domain.NewResourceType(
		domain.ResourceTypeRef{Name: "Wide", Version: "v1"},
		"Wide contract.",
		[]domain.Capability{domain.CapabilityCreate},
	)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := resourcetypes.NewContract(resourcetypes.ContractInput{
		Type:        typeValue,
		DisplayName: "Wide",
		SpecSchema:  []byte(wideSchema),
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := newCatalogFixture(t, contract)
	response := fixture.request(t, http.MethodPost, "/v1/resources",
		map[string]string{"Idempotency-Key": "wide-invalid"},
		invalidSpecCreateRequest("Wide", map[string]any{}))
	document := expectProblem(t, response, http.StatusUnprocessableEntity, "RESOURCE_SPEC_INVALID")
	violations, ok := document["violations"].([]any)
	if !ok || len(violations) != 10 {
		t.Fatalf("violations = %#v, want exactly ten", document["violations"])
	}
	truncated, ok := document["truncated"].(bool)
	if !ok || !truncated {
		t.Fatal("truncated indicator missing or false")
	}
}

// TestInvalidSpecLeavesNoRecords pins invariant L at the transport boundary:
// after a rejected create nothing is retained, the idempotency key stays
// unused, and a corrected retry under the same key succeeds as a fresh
// admission.
func TestInvalidSpecLeavesNoRecords(t *testing.T) {
	fixture := newCatalogFixture(t, mustPostgresContract(t))

	headers := map[string]string{"Idempotency-Key": "retry-key"}
	first := fixture.request(t, http.MethodPost, "/v1/resources", headers,
		invalidSpecCreateRequest("PostgreSQLDatabase", map[string]any{}))
	expectProblem(t, first, http.StatusUnprocessableEntity, "RESOURCE_SPEC_INVALID")

	counts := fixture.store.RecordCounts()
	if counts.Resources != 0 || counts.Operations != 0 || counts.Events != 0 ||
		counts.Executions != 0 || counts.Idempotency != 0 || counts.Outbox != 0 {
		t.Fatalf("rejected create left durable state: %+v", counts)
	}

	missing := fixture.request(t, http.MethodGet, "/v1/resources/db-1", nil, nil)
	expectProblem(t, missing, http.StatusNotFound, "RESOURCE_NOT_FOUND")

	retry := fixture.request(t, http.MethodPost, "/v1/resources", headers,
		invalidSpecCreateRequest("PostgreSQLDatabase", map[string]any{
			"version": "16", "storageGB": int64(20), "highAvailability": false,
		}))
	expectStatus(t, retry, http.StatusCreated)
	if header(retry, "Idempotency-Replayed") == "true" {
		t.Fatal("corrected retry replayed the rejected attempt instead of admitting fresh")
	}
}

// TestNumericRepresentationPreserved pins refinement F end to end over HTTP:
// storageGB 20 and 20.0 are both schema-valid integers per JSON Schema 2020-12,
// and they remain fingerprint-distinct ResourceSpecs. Reusing one idempotency
// key with only the numeric spelling changed therefore conflicts instead of
// silently matching, while a distinct admission of the float spelling succeeds.
func TestNumericRepresentationPreserved(t *testing.T) {
	fixture := newCatalogFixture(t, mustPostgresContract(t))

	intHeaders := map[string]string{"Idempotency-Key": "int-key"}
	intResponse := fixture.request(t, http.MethodPost, "/v1/resources", intHeaders,
		invalidSpecCreateRequest("PostgreSQLDatabase", map[string]any{
			"version": "16", "storageGB": int64(20), "highAvailability": true,
		}))
	expectStatus(t, intResponse, http.StatusCreated)

	// Same ID, same key, semantically equal value, different numeric
	// spelling on the wire ("20.0"). Fingerprinting preserves the literal
	// representation distinction established by M7.
	floatBody := `{"id":"db-1","type":{"name":"PostgreSQLDatabase","version":"v1"},` +
		`"owner":{"kind":"team","id":"platform"},` +
		`"spec":{"version":"16","storageGB":20.0,"highAvailability":true}}`
	replayRequest := httptest.NewRequest(http.MethodPost, "/v1/resources", strings.NewReader(floatBody))
	replayRequest.Header.Set("Idempotency-Key", "int-key")
	recorder := httptest.NewRecorder()
	fixture.handler.ServeHTTP(recorder, replayRequest)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("replay of 20.0 spelling = %d (%s), want %d IDEMPOTENCY_CONFLICT",
			recorder.Code, recorder.Body.String(), http.StatusConflict)
	}
	problem := decodeBody(t, recorder.Result())
	if problem["code"] != "IDEMPOTENCY_CONFLICT" {
		t.Fatalf("code = %v", problem["code"])
	}

	// The float spelling is independently admissible under its own identity.
	floatHeaders := map[string]string{"Idempotency-Key": "float-key"}
	floatResponse := fixture.request(t, http.MethodPost, "/v1/resources", floatHeaders,
		map[string]any{
			"id":    "db-2",
			"type":  map[string]string{"name": "PostgreSQLDatabase", "version": "v1"},
			"owner": map[string]string{"kind": "team", "id": "platform"},
			"spec":  map[string]any{"version": "16", "storageGB": 20, "highAvailability": true},
		})
	expectStatus(t, floatResponse, http.StatusCreated)

	stored := fixture.request(t, http.MethodGet, "/v1/resources/db-2", nil, nil)
	spec := decodeBody(t, stored)["spec"].(map[string]any)
	if value, ok := spec["storageGB"].(float64); !ok || value != 20 {
		t.Fatalf("stored storageGB = %#v, want the number 20", spec["storageGB"])
	}
}
