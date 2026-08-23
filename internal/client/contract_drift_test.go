// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/client"

	"gopkg.in/yaml.v3"
)

// The client deliberately duplicates the public representations instead of
// importing server DTOs, so this test pins every duplicated shape against the
// authoritative OpenAPI document: property names must match JSON tags
// exactly, and required fields must be present in the struct.

type schemaShape struct {
	Properties map[string]yaml.Node `yaml:"properties"`
	Required   []string             `yaml:"required"`
}

func loadOpenAPISchemas(t *testing.T) map[string]schemaShape {
	t.Helper()
	raw, err := os.ReadFile("../../docs/openapi/v1/openapi.yaml")
	if err != nil {
		t.Fatalf("reading OpenAPI document: %v", err)
	}
	var document struct {
		Components struct {
			Schemas map[string]schemaShape `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parsing OpenAPI document: %v", err)
	}
	return document.Components.Schemas
}

func jsonTags(t reflect.Type) map[string]bool {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("jsonTags on non-struct %s", t))
	}
	tags := make(map[string]bool)
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		tag := field.Tag.Get("json")
		if tag == "-" || tag == "" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name != "" {
			tags[name] = true
		}
	}
	return tags
}

func assertShapeMatches(t *testing.T, schemas map[string]schemaShape, schemaName string, instance any) {
	t.Helper()
	shape, ok := schemas[schemaName]
	if !ok {
		t.Fatalf("OpenAPI document has no schema %q", schemaName)
	}
	tags := jsonTags(reflect.TypeOf(instance))
	documented := make(map[string]bool, len(shape.Properties))
	for name := range shape.Properties {
		documented[name] = true
	}
	for name := range shape.Properties {
		if !tags[name] {
			t.Errorf("%s: documented property %q has no matching JSON tag in the client type", schemaName, name)
		}
	}
	for tag := range tags {
		if !documented[tag] {
			t.Errorf("%s: client JSON tag %q is not part of the documented representation", schemaName, tag)
		}
	}
	for _, required := range shape.Required {
		if !tags[required] {
			t.Errorf("%s: required property %q missing from client type", schemaName, required)
		}
	}
}

func TestClientRepresentationsMatchOpenAPI(t *testing.T) {
	schemas := loadOpenAPISchemas(t)

	assertShapeMatches(t, schemas, "Resource", client.Resource{})
	assertShapeMatches(t, schemas, "ResourceTypeRef", client.ResourceTypeRef{})
	assertShapeMatches(t, schemas, "OwnerRef", client.OwnerRef{})
	assertShapeMatches(t, schemas, "ResourceStatus", client.ResourceStatus{})
	assertShapeMatches(t, schemas, "Condition", client.Condition{})
	assertShapeMatches(t, schemas, "LatestOperationRef", client.LatestOperationRef{})
	assertShapeMatches(t, schemas, "ResourceOutputs", client.ResourceOutputs{})
	assertShapeMatches(t, schemas, "Operation", client.Operation{})
	assertShapeMatches(t, schemas, "OperationFailure", client.OperationFailure{})
	assertShapeMatches(t, schemas, "ResourceTypeSummary", client.ResourceTypeSummary{})
	assertShapeMatches(t, schemas, "ResourceType", client.ResourceTypeDetail{})
	assertShapeMatches(t, schemas, "Problem", client.Problem{})
	assertShapeMatches(t, schemas, "SpecViolation", client.SpecViolation{})
}

func TestCreateEnvelopeKeysMatchOpenAPI(t *testing.T) {
	schemas := loadOpenAPISchemas(t)
	body := client.BuildCreateEnvelope("orders-db", "PostgreSQLDatabase", "v2", "team", "payments",
		[]byte(`{"storageGB":20}`))
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	documented := make(map[string]bool)
	for name := range schemas["CreateResourceRequest"].Properties {
		documented[name] = true
	}
	for key := range decoded {
		if !documented[key] {
			t.Errorf("create envelope key %q is not part of CreateResourceRequest", key)
		}
	}
	for key := range documented {
		if _, ok := decoded[key]; !ok {
			t.Errorf("create envelope misses required key %q", key)
		}
	}
}
