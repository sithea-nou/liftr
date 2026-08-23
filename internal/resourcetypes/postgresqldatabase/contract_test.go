// SPDX-License-Identifier: Apache-2.0

package postgresqldatabase_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
	"github.com/sithea-nou/liftr/internal/resourcetypes"
	"github.com/sithea-nou/liftr/internal/resourcetypes/postgresqldatabase"
)

func mustContract(t *testing.T) resourcetypes.Contract {
	t.Helper()
	contract, err := postgresqldatabase.Contract()
	if err != nil {
		t.Fatalf("Contract() error = %v", err)
	}
	return contract
}

func mustSpec(t *testing.T, values map[string]any) domain.ResourceSpec {
	t.Helper()
	spec, err := domain.NewResourceSpec(values)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// TestContractIdentity pins the discovery identity of the example contract:
// stable URN $id namespace, verbatim schema document, and contract metadata.
func TestContractIdentity(t *testing.T) {
	contract := mustContract(t)

	if contract.Ref() != postgresqldatabase.TypeRef() {
		t.Fatalf("Ref() = %#v", contract.Ref())
	}
	if contract.DisplayName() != "PostgreSQL Database" {
		t.Fatalf("DisplayName() = %q", contract.DisplayName())
	}
	if !contract.Domain().Supports(domain.CapabilityObserve) {
		t.Fatal("contract lost the observe capability")
	}

	wantID := "urn:liftr:resource-type:PostgreSQLDatabase:v1:spec"
	schema := string(contract.SpecSchema())
	if !strings.Contains(schema, `"$id": "`+wantID+`"`) {
		t.Fatalf("schema does not declare the stable URN $id %s:\n%s", wantID, schema)
	}
	if compiled, err := resourcetypes.CompileSpecSchema(postgresqldatabase.SpecSchemaDocument()); err != nil {
		t.Fatalf("registered document no longer compiles: %v", err)
	} else if compiled.ID() != wantID {
		t.Fatalf("$id = %q, want %q", compiled.ID(), wantID)
	}
}

// TestSchemaContainsOnlyDeveloperIntent pins that the example schema proves
// provisioner neutrality: no provider, account, network, or credential
// vocabulary exists anywhere in the published contract.
func TestSchemaContainsOnlyDeveloperIntent(t *testing.T) {
	document := strings.ToLower(string(postgresqldatabase.SpecSchemaDocument()))
	for _, forbidden := range []string{
		"pulumi", "terraform", "crossplane", "stack", "workspace",
		"provider", "account", "subscription", "region", "sku",
		"credential", "password", "secret", "namespace", "git",
	} {
		if strings.Contains(document, forbidden) {
			t.Errorf("schema exposes implementation concept %q", forbidden)
		}
	}
	for _, required := range []string{"version", "storagegb", "highavailability"} {
		if !strings.Contains(document, `"`+required+`"`) {
			t.Errorf("schema is missing developer field %q", required)
		}
	}
}

func TestValidateSpecMatrix(t *testing.T) {
	contract := mustContract(t)

	valid := []domain.ResourceSpec{
		mustSpec(t, map[string]any{"version": "16", "storageGB": int64(20), "highAvailability": true}),
		mustSpec(t, map[string]any{"version": "16", "storageGB": int64(20), "highAvailability": false}),
		// JSON Schema integer semantics: an integral float is a valid integer.
		mustSpec(t, map[string]any{"version": "16", "storageGB": float64(20), "highAvailability": false}),
	}
	for index, spec := range valid {
		if err := contract.ValidateSpec(spec); err != nil {
			t.Fatalf("valid spec %d rejected: %v", index, err)
		}
	}

	tests := []struct {
		name        string
		values      map[string]any
		wantKeyword string
		wantPath    string
		wantMessage string
	}{
		{
			name:        "unknown property is rejected with typo path",
			values:      map[string]any{"version": "16", "storageGB": int64(20), "highAvailability": true, "storagGB": int64(5)},
			wantKeyword: "additionalProperties",
			wantPath:    "/storagGB",
			wantMessage: `property "storagGB" is not permitted by this resource type`,
		},
		{
			name:        "missing highAvailability has no server-side default",
			values:      map[string]any{"version": "16", "storageGB": int64(20)},
			wantKeyword: "required",
			wantPath:    "",
			wantMessage: `property "highAvailability" is required`,
		},
		{
			name:        "missing version",
			values:      map[string]any{"storageGB": int64(20), "highAvailability": true},
			wantKeyword: "required",
			wantPath:    "",
			wantMessage: `property "version" is required`,
		},
		{
			name:        "zero storage below minimum",
			values:      map[string]any{"version": "16", "storageGB": int64(0), "highAvailability": true},
			wantKeyword: "minimum",
			wantPath:    "/storageGB",
			wantMessage: "value must be greater than or equal to 1",
		},
		{
			name:        "negative storage",
			values:      map[string]any{"version": "16", "storageGB": int64(-1), "highAvailability": true},
			wantKeyword: "minimum",
			wantPath:    "/storageGB",
			wantMessage: "value must be greater than or equal to 1",
		},
		{
			name:        "string storage",
			values:      map[string]any{"version": "16", "storageGB": "20", "highAvailability": true},
			wantKeyword: "type",
			wantPath:    "/storageGB",
			wantMessage: "value must be of type integer",
		},
		{
			name:        "fractional storage is not an integer",
			values:      map[string]any{"version": "16", "storageGB": 20.5, "highAvailability": true},
			wantKeyword: "type",
			wantPath:    "/storageGB",
			wantMessage: "value must be of type integer",
		},
		{
			name:        "empty version",
			values:      map[string]any{"version": "", "storageGB": int64(20), "highAvailability": true},
			wantKeyword: "minLength",
			wantPath:    "/version",
			wantMessage: "string length must be at least 1",
		},
		{
			name:        "string highAvailability",
			values:      map[string]any{"version": "16", "storageGB": int64(20), "highAvailability": "true"},
			wantKeyword: "type",
			wantPath:    "/highAvailability",
			wantMessage: "value must be of type boolean",
		},
		{
			name:        "non-object root",
			values:      map[string]any{},
			wantKeyword: "required",
			wantPath:    "",
			wantMessage: `property "highAvailability" is required`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := contract.ValidateSpec(mustSpec(t, tt.values))
			var invalid *resourcecontract.ValidationError
			if !errors.As(err, &invalid) {
				t.Fatalf("ValidateSpec() error = %v (%T), want *resourcecontract.ValidationError", err, err)
			}
			found := false
			for _, violation := range invalid.Violations {
				if violation.Path == tt.wantPath && violation.Keyword == tt.wantKeyword && violation.Message == tt.wantMessage {
					found = true
				}
				if strings.Contains(violation.Message, "got ") || strings.Contains(violation.Message, "jsonschema") {
					t.Fatalf("violation leaks validator internals or submitted values: %+v", violation)
				}
			}
			if !found {
				t.Fatalf("violations missing {%s %s %q}, got %+v", tt.wantPath, tt.wantKeyword, tt.wantMessage, invalid.Violations)
			}
		})
	}
}

// TestViolationsAreDeterministic pins stable ordering of multi-violation
// responses regardless of library evaluation order.
func TestViolationsAreDeterministic(t *testing.T) {
	contract := mustContract(t)
	spec := mustSpec(t, map[string]any{"storagGB": int64(5)})
	first := validateViolations(t, contract, spec)
	for range 10 {
		next := validateViolations(t, contract, spec)
		if len(next) != len(first) {
			t.Fatal("violation count is not deterministic")
		}
		for index := range next {
			if next[index] != first[index] {
				t.Fatalf("violations are not deterministically ordered:\n%dth: %+v\nwant: %+v", index, next[index], first[index])
			}
		}
	}
}

func validateViolations(t *testing.T, contract resourcetypes.Contract, spec domain.ResourceSpec) []resourcecontract.Violation {
	t.Helper()
	err := contract.ValidateSpec(spec)
	var invalid *resourcecontract.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("expected InvalidSpecError, got %v", err)
	}
	return invalid.Violations
}

// TestNewSpecDelegatesToContract pins that the convenience builder and the
// published schema cannot silently diverge.
func TestNewSpecDelegatesToContract(t *testing.T) {
	spec, err := postgresqldatabase.NewSpec("16", 20, true)
	if err != nil {
		t.Fatalf("NewSpec() error = %v", err)
	}
	if got := spec.Values()["storageGB"]; got != int64(20) {
		t.Fatalf("storageGB = %v (%T), want int64(20)", got, got)
	}

	if _, err := postgresqldatabase.NewSpec("", 20, true); err == nil {
		t.Fatal("empty version accepted")
	}
	if _, err := postgresqldatabase.NewSpec("16", 0, true); err == nil {
		t.Fatal("zero storage accepted")
	}
}
