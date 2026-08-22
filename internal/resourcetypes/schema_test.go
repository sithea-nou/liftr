// SPDX-License-Identifier: Apache-2.0

package resourcetypes_test

import (
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/resourcetypes"
)

const minimalSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "urn:liftr:resource-type:Widget:v1:spec",
  "type": "object",
  "additionalProperties": false,
  "required": ["name"],
  "properties": {"name": {"type": "string", "minLength": 1}}
}`

func TestCompileSpecSchema(t *testing.T) {
	schema, err := resourcetypes.CompileSpecSchema([]byte(minimalSchema))
	if err != nil {
		t.Fatalf("CompileSpecSchema() error = %v", err)
	}
	if schema.ID() != "urn:liftr:resource-type:Widget:v1:spec" {
		t.Fatalf("ID() = %q", schema.ID())
	}

	// The document must round-trip exactly as registered.
	document := string(schema.Document())
	if document != minimalSchema {
		t.Fatalf("Document() did not return the registered bytes verbatim")
	}

	digest := schema.Digest()
	if len(digest) != 64 {
		t.Fatalf("Digest() = %q, want a hex-encoded SHA-256", digest)
	}
	recompiled, err := resourcetypes.CompileSpecSchema([]byte(minimalSchema))
	if err != nil {
		t.Fatal(err)
	}
	if recompiled.Digest() != digest {
		t.Fatal("digest is not stable for identical documents")
	}
}

func TestCompileSpecSchemaRejectsInvalidDocuments(t *testing.T) {
	tests := []struct {
		name     string
		document string
	}{
		{"empty", ""},
		{"not json", "{not json"},
		{"not an object", `[]`},
		{"missing id", `{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object"}`},
		{
			"wrong dialect",
			`{
			  "$schema": "http://json-schema.org/draft-07/schema#",
			  "$id": "urn:liftr:resource-type:Widget:v1:spec",
			  "type": "object"
			}`,
		},
		{
			"root not object",
			`{
			  "$schema": "https://json-schema.org/draft/2020-12/schema",
			  "$id": "urn:liftr:resource-type:Widget:v1:spec",
			  "type": "string"
			}`,
		},
		{
			"invalid keyword values",
			`{
			  "$schema": "https://json-schema.org/draft/2020-12/schema",
			  "$id": "urn:liftr:resource-type:Widget:v1:spec",
			  "type": "object",
			  "required": "name"
			}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := resourcetypes.CompileSpecSchema([]byte(tt.document)); err == nil {
				t.Fatalf("CompileSpecSchema() accepted invalid document:\n%s", tt.document)
			}
		})
	}
}

// TestSpecSchemaPerformsNoNetworkResolution pins that validation never
// performs network I/O: any reference escaping the registered document fails
// compilation through the blocked loader instead of being fetched.
func TestSpecSchemaPerformsNoNetworkResolution(t *testing.T) {
	documents := []string{
		`{
		  "$schema": "https://json-schema.org/draft/2020-12/schema",
		  "$id": "urn:liftr:resource-type:Widget:v1:spec",
		  "type": "object",
		  "properties": {"remote": {"$ref": "https://example.com/schema.json"}}
		}`,
		`{
		  "$schema": "https://json-schema.org/draft/2020-12/schema",
		  "$id": "urn:liftr:resource-type:Widget:v1:spec",
		  "type": "object",
		  "properties": {"file": {"$ref": "file:///etc/passwd.json"}}
		}`,
	}
	for _, document := range documents {
		_, err := resourcetypes.CompileSpecSchema([]byte(document))
		if err == nil {
			t.Fatalf("external $ref was resolved instead of blocked:\n%s", document)
		}
		if !strings.Contains(err.Error(), "resolution outside the registered document is disabled") {
			t.Fatalf("unexpected error for external ref: %v", err)
		}
	}
}

// TestSpecSchemaLocalReferencesWork proves that disabling network resolution
// does not disable valid local composition via $defs and "#/$defs/...".
func TestSpecSchemaLocalReferencesWork(t *testing.T) {
	document := `{
	  "$schema": "https://json-schema.org/draft/2020-12/schema",
	  "$id": "urn:liftr:resource-type:Widget:v1:spec",
	  "type": "object",
	  "additionalProperties": false,
	  "required": ["labels"],
	  "properties": {
	    "labels": {"type": "array", "items": {"$ref": "#/$defs/label"}}
	  },
	  "$defs": {
	    "label": {"type": "string", "minLength": 1}
	  }
	}`
	schema, err := resourcetypes.CompileSpecSchema([]byte(document))
	if err != nil {
		t.Fatalf("local $defs/$ref failed to compile: %v", err)
	}
	valid := map[string]any{"labels": []any{"a", "b"}}
	if violations := schema.ViolationsFor(valid); len(violations) != 0 {
		t.Fatalf("valid local-ref payload rejected: %+v", violations)
	}
	invalid := map[string]any{"labels": []any{""}}
	if violations := schema.ViolationsFor(invalid); len(violations) == 0 {
		t.Fatal("payload violating a locally referenced subschema was accepted")
	}
}

// TestFormatIsAnnotationOnly pins the M8 contract decision: JSON Schema
// format keywords never become admission constraints. A validator-library
// swap cannot silently change this behavior because it is asserted here.
func TestFormatIsAnnotationOnly(t *testing.T) {
	document := `{
	  "$schema": "https://json-schema.org/draft/2020-12/schema",
	  "$id": "urn:liftr:resource-type:Widget:v1:spec",
	  "type": "object",
	  "properties": {
	    "contact": {"type": "string", "format": "email"},
	    "host": {"type": "string", "format": "hostname"},
	    "uid": {"type": "string", "format": "uuid"}
	  }
	}`
	schema, err := resourcetypes.CompileSpecSchema([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{"contact": "not-an-email", "host": "not a hostname", "uid": "not-a-uuid"}
	if violations := schema.ViolationsFor(values); len(violations) != 0 {
		t.Fatalf("format keywords became assertions: %+v", violations)
	}
}

// TestIntegerSemanticsFollowJSONSchema pins refinement 4: JSON Schema
// considers any number with zero fractional part an integer, regardless of
// the Go representation Liftr stores (int64 vs float64).
func TestIntegerSemanticsFollowJSONSchema(t *testing.T) {
	document := `{
	  "$schema": "https://json-schema.org/draft/2020-12/schema",
	  "$id": "urn:liftr:resource-type:Widget:v1:spec",
	  "type": "object",
	  "additionalProperties": false,
	  "required": ["count"],
	  "properties": {"count": {"type": "integer", "minimum": 1}}
	}`
	schema, err := resourcetypes.CompileSpecSchema([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		count  any
		wantOK bool
	}{
		{name: "int64 integer", count: int64(20), wantOK: true},
		{name: "float64 integral value", count: float64(20), wantOK: true},
		{name: "float64 fractional value", count: float64(20.5), wantOK: false},
		{name: "below minimum", count: int64(0), wantOK: false},
		{name: "string", count: "20", wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			violations := schema.ViolationsFor(map[string]any{"count": tt.count})
			if gotOK := len(violations) == 0; gotOK != tt.wantOK {
				t.Fatalf("accepted = %v, want %v (violations=%+v)", gotOK, tt.wantOK, violations)
			}
		})
	}
}

func TestViolationShapeAndSanitization(t *testing.T) {
	document := `{
	  "$schema": "https://json-schema.org/draft/2020-12/schema",
	  "$id": "urn:liftr:resource-type:Widget:v1:spec",
	  "type": "object",
	  "additionalProperties": false,
	  "required": ["name", "size"],
	  "properties": {
	    "name": {"type": "string"},
	    "size": {"type": "integer", "minimum": 1}
	  }
	}`
	schema, err := resourcetypes.CompileSpecSchema([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	violations := schema.ViolationsFor(map[string]any{
		"name":    int64(7),
		"size":    float64(0.5),
		"storagB": true,
	})
	if len(violations) < 3 {
		t.Fatalf("expected at least three violations, got %+v", violations)
	}

	paths := make(map[string]bool)
	for _, violation := range violations {
		switch violation.Keyword {
		case "type":
			if violation.Message == "" || strings.Contains(violation.Message, "got ") {
				t.Fatalf("message leaks library phrasing or is empty: %q", violation.Message)
			}
		case "additionalProperties":
			if !strings.Contains(violation.Message, `"storagB"`) {
				t.Fatalf("unknown-property message should name the property: %q", violation.Message)
			}
		default:
			t.Fatalf("unexpected keyword %q", violation.Keyword)
		}
		paths[violation.Path+":"+violation.Keyword] = true
	}
	if !paths["/storagB:additionalProperties"] {
		t.Fatalf("unknown property violation lacks stable JSON Pointer path: %+v", violations)
	}

	// A missing required property yields a named, root-pathed violation.
	missingViolations := schema.ViolationsFor(map[string]any{"name": "x"})
	if len(missingViolations) != 1 {
		t.Fatalf("expected exactly one required violation, got %+v", missingViolations)
	}
	if missingViolations[0].Path != "" || missingViolations[0].Keyword != "required" ||
		missingViolations[0].Message != `property "size" is required` {
		t.Fatalf("unexpected required violation: %+v", missingViolations[0])
	}

	// Violations are deterministic across repeated evaluations.
	again := schema.ViolationsFor(map[string]any{
		"name":    int64(7),
		"size":    float64(0.5),
		"storagB": true,
	})
	if len(again) != len(violations) {
		t.Fatal("violation count is not deterministic")
	}
	for index := range again {
		if again[index] != violations[index] {
			t.Fatalf("violations are not deterministically ordered:\n%dth: %+v\nwant: %+v", index, again[index], violations[index])
		}
	}

	// Valid payloads produce nothing.
	if got := schema.ViolationsFor(map[string]any{"name": "x", "size": int64(3)}); len(got) != 0 {
		t.Fatalf("valid payload produced violations: %+v", got)
	}
}
