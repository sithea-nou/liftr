// SPDX-License-Identifier: Apache-2.0

package resourcetypes_test

import (
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
	"github.com/sithea-nou/liftr/internal/resourcetypes"
)

func widgetType() domain.ResourceType {
	typeValue, err := domain.NewResourceType(
		domain.ResourceTypeRef{Name: "Widget", Version: "v1"},
		"Test widget contract.",
		[]domain.Capability{domain.CapabilityDelete, domain.CapabilityCreate},
	)
	if err != nil {
		panic(err)
	}
	return typeValue
}

func mustContract(t *testing.T, input resourcetypes.ContractInput) resourcetypes.Contract {
	t.Helper()
	contract, err := resourcetypes.NewContract(input)
	if err != nil {
		t.Fatalf("NewContract() error = %v", err)
	}
	return contract
}

func TestNewContract(t *testing.T) {
	contract := mustContract(t, resourcetypes.ContractInput{
		Type:        widgetType(),
		DisplayName: "Test Widget",
		SpecSchema:  []byte(minimalSchema),
	})
	if contract.Ref() != (domain.ResourceTypeRef{Name: "Widget", Version: "v1"}) {
		t.Fatalf("Ref() = %#v", contract.Ref())
	}
	if contract.DisplayName() != "Test Widget" {
		t.Fatalf("DisplayName() = %q", contract.DisplayName())
	}
	if contract.Description() != "Test widget contract." {
		t.Fatalf("Description() = %q", contract.Description())
	}

	// Capabilities are deduplicated by the domain and sorted for discovery.
	capabilities := contract.Capabilities()
	if len(capabilities) != 2 || capabilities[0] != domain.CapabilityCreate || capabilities[1] != domain.CapabilityDelete {
		t.Fatalf("Capabilities() = %v, want sorted [create delete]", capabilities)
	}
	capabilities[0] = "mutated"
	if contract.Capabilities()[0] != domain.CapabilityCreate {
		t.Fatal("Capabilities() exposed internal state")
	}
}

func TestNewContractDefaultsDisplayNameToName(t *testing.T) {
	contract := mustContract(t, resourcetypes.ContractInput{
		Type:       widgetType(),
		SpecSchema: []byte(minimalSchema),
	})
	if contract.DisplayName() != "Widget" {
		t.Fatalf("DisplayName() = %q", contract.DisplayName())
	}
}

func TestNewContractRejectsMismatchedSchemaIdentity(t *testing.T) {
	document := strings.ReplaceAll(minimalSchema, "urn:liftr:resource-type:Widget:v1:spec", "urn:liftr:resource-type:Other:v9:spec")
	_, err := resourcetypes.NewContract(resourcetypes.ContractInput{
		Type:       widgetType(),
		SpecSchema: []byte(document),
	})
	if err == nil {
		t.Fatal("contract accepted a schema whose $id does not match its identity")
	}
}

func TestNewContractRejectsInvalidSchema(t *testing.T) {
	broken := strings.Replace(minimalSchema, `"required": ["name"],`, `"required": "name",`, 1)
	if _, err := resourcetypes.NewContract(resourcetypes.ContractInput{Type: widgetType(), SpecSchema: []byte(broken)}); err == nil {
		t.Fatal("registration accepted an uncompilable schema")
	}
}

func TestValidateSpecIsPurePredicate(t *testing.T) {
	contract := mustContract(t, resourcetypes.ContractInput{
		Type:       widgetType(),
		SpecSchema: []byte(minimalSchema),
	})
	spec, err := domain.NewResourceSpec(map[string]any{"name": "gear"})
	if err != nil {
		t.Fatal(err)
	}
	before := spec.Values()
	if err := contract.ValidateSpec(spec); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	if err := contract.ValidateSpec(mustSpecValues(map[string]any{})); err == nil {
		t.Fatal("missing name should fail")
	} else {
		var invalid *resourcecontract.ValidationError
		if !asInvalidSpec(err, &invalid) {
			t.Fatalf("error is not *resourcecontract.ValidationError: %T", err)
		}
		if invalid.TypeRef.Name != "Widget" || invalid.TypeRef.Version != "v1" {
			t.Fatalf("violation TypeRef = %#v", invalid.TypeRef)
		}
	}
	if got := spec.Values()["name"]; got != "gear" {
		t.Fatalf("validation mutated the spec: name = %v", got)
	}
	_ = before
}

func TestSemanticValidatorRunsAfterStructuralValidation(t *testing.T) {
	semanticCalls := 0
	contract := mustContract(t, resourcetypes.ContractInput{
		Type:       widgetType(),
		SpecSchema: []byte(minimalSchema),
		Semantic: func(values map[string]any) []resourcecontract.Violation {
			semanticCalls++
			return []resourcecontract.Violation{{Path: "", Keyword: "semantic", Message: "cross-field rule failed"}}
		},
	})
	spec := mustSpecValues(map[string]any{"name": "gear"})
	err := contract.ValidateSpec(spec)
	if semanticCalls != 1 {
		t.Fatalf("semantic validator ran %d times, want once", semanticCalls)
	}
	var invalid *resourcecontract.ValidationError
	if !asInvalidSpec(err, &invalid) {
		t.Fatalf("expected InvalidSpecError, got %v", err)
	}
	if len(invalid.Violations) != 1 || invalid.Violations[0].Keyword != "semantic" {
		t.Fatalf("violations = %+v", invalid.Violations)
	}
}

func TestSchemaDigestStablePerRegistration(t *testing.T) {
	first := mustContract(t, resourcetypes.ContractInput{Type: widgetType(), SpecSchema: []byte(minimalSchema)})
	second := mustContract(t, resourcetypes.ContractInput{Type: widgetType(), SpecSchema: []byte(minimalSchema)})
	if first.SchemaDigest() != second.SchemaDigest() {
		t.Fatal("digest differs between registrations of identical documents")
	}
	mutated := strings.Replace(minimalSchema, `"minLength": 1`, `"minLength": 2`, 1)
	third := mustContract(t, resourcetypes.ContractInput{Type: widgetType(), SpecSchema: []byte(mutated)})
	if third.SchemaDigest() == first.SchemaDigest() {
		t.Fatal("digest did not detect a schema mutation under the same ref")
	}
}

func asInvalidSpec(err error, target **resourcecontract.ValidationError) bool {
	if invalid, ok := err.(*resourcecontract.ValidationError); ok {
		*target = invalid
		return true
	}
	return false
}

func mustSpecValues(values map[string]any) domain.ResourceSpec {
	spec, err := domain.NewResourceSpec(values)
	if err != nil {
		panic(err)
	}
	return spec
}
