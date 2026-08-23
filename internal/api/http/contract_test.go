// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const openAPIPath = "../../../docs/openapi/v1/openapi.yaml"

// TestOpenAPIMatchesRuntimeRoutes pins that the handwritten contract and the
// running router describe exactly the same surface.
func TestOpenAPIMatchesRuntimeRoutes(t *testing.T) {
	document := loadContract(t)
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		t.Fatal("contract has no paths object")
	}

	documented := map[string]bool{}
	for path, rawItem := range paths {
		item, ok := rawItem.(map[string]any)
		if !ok {
			t.Fatalf("path %q is not an object", path)
		}
		for method := range item {
			switch method {
			case "get", "post", "put", "delete", "patch":
				documented[methodKey(method, path)] = true
			case "parameters", "summary", "description", "operationId":
				// shared path-level metadata
			default:
				t.Fatalf("unexpected key %q under path %q", method, path)
			}
		}
	}
	if len(documented) == 0 {
		t.Fatal("no documented operations found")
	}

	runtime := map[string]bool{}
	for _, rt := range apiRoutes(&handler{}) {
		runtime[methodKey(strings.ToLower(rt.Method), rt.Pattern)] = true
	}
	for key := range documented {
		if !runtime[key] {
			t.Errorf("documented operation %q is not served by the router", key)
		}
	}
	for key := range runtime {
		if !documented[key] {
			t.Errorf("runtime route %q is missing from the contract", key)
		}
	}
}

// TestOpenAPIModelsPublicSchemas pins the approved public shapes and the
// absence of internal concepts.
func TestOpenAPIModelsPublicSchemas(t *testing.T) {
	document := loadContract(t)
	schemas := componentSchemas(t, document)

	resourceProperties := propertyNames(t, schemas["Resource"])
	wantResource := []string{"id", "type", "owner", "generation", "spec", "status", "latestOperation", "outputs", "createdAt", "updatedAt"}
	if strings.Join(resourceProperties, ",") != strings.Join(sorted(wantResource), ",") {
		t.Fatalf("Resource properties = %v, want %v", resourceProperties, wantResource)
	}

	// The outputs envelope and the discovery output contract must be modeled
	// explicitly; the values payload stays open because exact names and types
	// are declared per ResourceType in discovery.
	for _, name := range []string{"ResourceOutputs", "ResourceTypeOutputContract", "ResourceTypeOutputField"} {
		if _, ok := schemas[name]; !ok {
			t.Errorf("component schema %q is not modeled explicitly", name)
		}
	}
	outputEnvelope := propertyNames(t, schemas["ResourceOutputs"])
	if strings.Join(outputEnvelope, ",") != strings.Join(sorted([]string{"observedGeneration", "values"}), ",") {
		t.Fatalf("ResourceOutputs properties = %v", outputEnvelope)
	}
	outputValuesProperties := propertyNames(t, schemas["ResourceOutputs"].(map[string]any)["properties"].(map[string]any)["values"])
	if len(outputValuesProperties) != 0 {
		t.Fatalf("OutputValues must stay open-ended without fixed properties, got %v", outputValuesProperties)
	}

	operationProperties := propertyNames(t, schemas["Operation"])
	wantOperation := []string{"id", "resourceId", "capability", "state", "targetGeneration", "requestedAt", "startedAt", "completedAt", "failure"}
	if strings.Join(operationProperties, ",") != strings.Join(sorted(wantOperation), ",") {
		t.Fatalf("Operation properties = %v, want %v", operationProperties, wantOperation)
	}

	problemProperties := propertyNames(t, schemas["Problem"])
	for _, field := range []string{"type", "title", "status", "detail", "instance", "code", "requestId", "currentGeneration"} {
		found := false
		for _, name := range problemProperties {
			if name == field {
				found = true
			}
		}
		if !found {
			t.Errorf("Problem schema is missing %q", field)
		}
	}
	for _, name := range []string{"ResourceStatus", "Condition", "LatestOperationRef", "ResourceSpec",
		"Capability", "ResourceTypeSummary", "ResourceType", "ResourceSpecSchema", "SpecViolation"} {
		if _, ok := schemas[name]; !ok {
			t.Errorf("component schema %q is not modeled explicitly", name)
		}
	}

	specProperties := propertyNames(t, schemas["ResourceSpec"])
	if len(specProperties) != 0 {
		t.Fatalf("ResourceSpec must stay open-ended without fixed properties, got %v", specProperties)
	}
	additionalProperties, allowsArbitrary := schemas["ResourceSpec"].(map[string]any)["additionalProperties"]
	if !allowsArbitrary || additionalProperties != true {
		t.Fatal("ResourceSpec must allow arbitrary keys")
	}
	// ResourceSpecSchema models only the envelope of a JSON Schema document;
	// it must stay an arbitrary object without pretending to own keywords.
	schemaEnvelopeProperties := propertyNames(t, schemas["ResourceSpecSchema"])
	if len(schemaEnvelopeProperties) != 0 {
		t.Fatalf("ResourceSpecSchema must model only the envelope, got fixed properties %v", schemaEnvelopeProperties)
	}
	envelopeAdditional, envelopeAllowsArbitrary := schemas["ResourceSpecSchema"].(map[string]any)["additionalProperties"]
	if !envelopeAllowsArbitrary || envelopeAdditional != true {
		t.Fatal("ResourceSpecSchema must allow arbitrary JSON Schema keywords")
	}
	assertNoInternalConcepts(t, schemas)
}

func assertNoInternalConcepts(t *testing.T, schemas map[string]any) {
	t.Helper()
	forbidden := []string{
		"phase", "phaseChangedAt", "provisionerRef", "handle", "attemptNumber", "fingerprint",
		"recordVersion", "leaseToken", "pulumi", "terraform", "crossplane", "stack",
		"workspace", "cloudAccount", "subscription", "gitRepository",
	}
	for name, rawSchema := range schemas {
		for _, property := range propertyNames(t, rawSchema) {
			for _, bad := range forbidden {
				if strings.EqualFold(property, bad) {
					t.Errorf("schema %q exposes internal property %q", name, property)
				}
			}
		}
	}
}

// TestOpenAPIDocumentsHeadersAndCodes pins the required header documentation
// and error-code registry.
func TestOpenAPIDocumentsHeadersAndCodes(t *testing.T) {
	raw := string(loadRawContract(t))
	headers := []string{
		"Idempotency-Key", "Liftr-Generation", "If-Liftr-Generation",
		"Location", "Link", "Idempotency-Replayed",
		"X-Request-ID", "X-Correlation-ID", "Cache-Control",
	}
	for _, header := range headers {
		if !strings.Contains(raw, header+":") && !strings.Contains(raw, header+"\n") {
			t.Errorf("contract does not document header %q", header)
		}
	}
	codes := []string{
		"UNAUTHENTICATED", "FORBIDDEN",
		"INVALID_ARGUMENT", "UNSUPPORTED_RESOURCE_TYPE", "RESOURCE_TYPE_NOT_FOUND",
		"RESOURCE_SPEC_INVALID", "RESOURCE_NOT_FOUND", "OPERATION_NOT_FOUND",
		"RESOURCE_ALREADY_EXISTS", "IDEMPOTENCY_CONFLICT", "GENERATION_CONFLICT", "OPERATION_ACTIVE",
		"RESOURCE_STATE_CONFLICT", "UNSUPPORTED_CAPABILITY", "PRECONDITION_REQUIRED",
		"PROVISIONER_UNAVAILABLE", "PERSISTENCE_UNAVAILABLE", "INTERNAL",
	}
	for _, code := range codes {
		if !strings.Contains(raw, "- "+code) {
			t.Errorf("contract does not register error code %q", code)
		}
	}
}

// TestNoValidatorHeadersInContract extends transport pin A to the written
// contract: representation validators do not exist in v1.
func TestNoValidatorHeadersInContract(t *testing.T) {
	raw := string(loadRawContract(t))
	for _, banned := range []string{"ETag", "Etag", "etag", "If-Match", "If-None-Match"} {
		if strings.Contains(raw, banned) {
			t.Errorf("contract mentions %q; v1 defines no representation validators", banned)
		}
	}
}

// TestOpenAPIDocumentsSecurity pins the M11 security contract: a bearer
// scheme applies to every /v1 operation, health endpoints opt out, 401 is
// documented everywhere versioned, and 403 is reserved for create/discovery
// denials under the hidden-404 policy (ADR-0012).
func TestOpenAPIDocumentsSecurity(t *testing.T) {
	document := loadContract(t)

	components, ok := document["components"].(map[string]any)
	if !ok {
		t.Fatal("document has no components")
	}
	schemes, ok := components["securitySchemes"].(map[string]any)
	if !ok {
		t.Fatal("document declares no securitySchemes")
	}
	bearer, ok := schemes["bearerAuth"].(map[string]any)
	if !ok {
		t.Fatal("securitySchemes does not declare bearerAuth")
	}
	if bearer["type"] != "http" || bearer["scheme"] != "bearer" || bearer["bearerFormat"] != "JWT" {
		t.Fatalf("bearerAuth = %v, want http/bearer/JWT", bearer)
	}

	requirements := document["security"].([]any)
	rootRequirement, ok := requirements[0].(map[string]any)
	if !ok || len(rootRequirement) != 1 {
		t.Fatalf("root security = %v, want exactly one bearerAuth requirement", document["security"])
	}

	for path, rawOperations := range document["paths"].(map[string]any) {
		operations, ok := rawOperations.(map[string]any)
		if !ok {
			continue
		}
		versioned := strings.HasPrefix(path, "/v1/")
		for method, rawOperation := range operations {
			operation, ok := rawOperation.(map[string]any)
			if !ok || method == "parameters" {
				continue
			}
			responses, _ := operation["responses"].(map[string]any)
			if versioned {
				if _, has401 := responses["401"]; !has401 {
					t.Errorf("%s %s documents no 401 response", method, path)
				}
			} else {
				if security, present := operation["security"]; !present || security != nil && len(security.([]any)) != 0 {
					t.Errorf("%s %s must override the root security requirement with an empty list", method, path)
				}
			}
			if _, has403 := responses["403"]; has403 && path != "/v1/resources" {
				t.Errorf("%s %s documents 403 but only create admissions may answer FORBIDDEN", method, path)
			}
		}
	}
}

func loadContract(t *testing.T) map[string]any {
	t.Helper()
	var document map[string]any
	if err := yaml.Unmarshal(loadRawContract(t), &document); err != nil {
		t.Fatalf("parse OpenAPI contract: %v", err)
	}
	return document
}

func loadRawContract(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(openAPIPath)
	if err != nil {
		t.Fatalf("read OpenAPI contract: %v", err)
	}
	return raw
}

func componentSchemas(t *testing.T, document map[string]any) map[string]any {
	t.Helper()
	components, ok := document["components"].(map[string]any)
	if !ok {
		t.Fatal("contract has no components object")
	}
	schemas, ok := components["schemas"].(map[string]any)
	if !ok {
		t.Fatal("contract has no components.schemas object")
	}
	return schemas
}

func propertyNames(t *testing.T, schema any) []string {
	t.Helper()
	properties, ok := schema.(map[string]any)["properties"].(map[string]any)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func sorted(values []string) []string {
	cloned := append([]string(nil), values...)
	sortStrings(cloned)
	return cloned
}

func methodKey(method, path string) string {
	return fmt.Sprintf("%s %s", method, path)
}
