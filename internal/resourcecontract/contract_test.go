// SPDX-License-Identifier: Apache-2.0

package resourcecontract_test

import (
	"errors"
	"sort"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
)

func TestNewValidationErrorDeduplicatesAndSorts(t *testing.T) {
	ref := domain.ResourceTypeRef{Name: "Widget", Version: "v1"}
	rejection := resourcecontract.NewValidationError(ref, []resourcecontract.Violation{
		{Path: "/b", Keyword: "type", Message: "second"},
		{Path: "/a", Keyword: "required", Message: "first"},
		{Path: "/b", Keyword: "type", Message: "second"},
	})
	if len(rejection.Violations) != 2 {
		t.Fatalf("duplicates were not removed: %+v", rejection.Violations)
	}
	if !sort.SliceIsSorted(rejection.Violations, func(i, j int) bool {
		return rejection.Violations[i].Path < rejection.Violations[j].Path
	}) {
		t.Fatalf("violations are not sorted: %+v", rejection.Violations)
	}
	if rejection.TypeRef != ref {
		t.Fatalf("TypeRef = %#v", rejection.TypeRef)
	}
}

func TestNewValidationErrorDoesNotCapResponseSize(t *testing.T) {
	ref := domain.ResourceTypeRef{Name: "Widget", Version: "v1"}
	raw := make([]resourcecontract.Violation, 0, 25)
	for i := range 25 {
		raw = append(raw, resourcecontract.Violation{Path: string(rune('a' + i)), Keyword: "k", Message: "m"})
	}
	rejection := resourcecontract.NewValidationError(ref, raw)
	if len(rejection.Violations) != 25 {
		t.Fatalf("producer-side normalization capped violations at %d", len(rejection.Violations))
	}
}

func TestValidationErrorMatchesNeutralSentinel(t *testing.T) {
	rejection := resourcecontract.NewValidationError(domain.ResourceTypeRef{Name: "W", Version: "v1"}, nil)
	if !errors.Is(rejection, resourcecontract.ErrInvalidSpec) {
		t.Fatal("ValidationError must match ErrInvalidSpec")
	}
	if errors.Is(errors.New("other"), resourcecontract.ErrInvalidSpec) {
		t.Fatal("unrelated error matched the sentinel")
	}
}

func sampleOutputFields() []resourcecontract.OutputField {
	return []resourcecontract.OutputField{
		{Name: "port", JSONType: resourcecontract.OutputTypeInteger, RequiredWhenReady: true},
		{Name: "hostname", JSONType: resourcecontract.OutputTypeString, RequiredWhenReady: true},
	}
}

func TestNewOutputContractNormalizesOrderAndRejectsBadDeclarations(t *testing.T) {
	contract, err := resourcecontract.NewOutputContract(sampleOutputFields())
	if err != nil {
		t.Fatal(err)
	}
	fields := contract.Fields()
	if fields[0].Name != "hostname" || fields[1].Name != "port" {
		t.Fatalf("fields are not deterministically ordered: %+v", fields)
	}
	if !contract.RequiresOutputs() {
		t.Fatal("required fields not detected")
	}

	cases := []struct {
		name   string
		fields []resourcecontract.OutputField
	}{
		{"empty", nil},
		{"blank name", []resourcecontract.OutputField{{Name: "  ", JSONType: resourcecontract.OutputTypeString}}},
		{"non-canonical name", []resourcecontract.OutputField{{Name: " Host ", JSONType: resourcecontract.OutputTypeString}}},
		{"duplicate", []resourcecontract.OutputField{
			{Name: "host", JSONType: resourcecontract.OutputTypeString},
			{Name: "host", JSONType: resourcecontract.OutputTypeInteger},
		}},
		{"reserved observedGeneration", []resourcecontract.OutputField{{Name: "observedGeneration", JSONType: resourcecontract.OutputTypeInteger}}},
		{"reserved values", []resourcecontract.OutputField{{Name: "values", JSONType: resourcecontract.OutputTypeString}}},
		{"unknown type", []resourcecontract.OutputField{{Name: "x", JSONType: "array"}}},
	}
	for _, tc := range cases {
		if _, err := resourcecontract.NewOutputContract(tc.fields); err == nil {
			t.Errorf("%s: declaration accepted", tc.name)
		}
	}
}

func TestOptionalOnlyContractDoesNotRequireOutputs(t *testing.T) {
	contract, err := resourcecontract.NewOutputContract([]resourcecontract.OutputField{
		{Name: "note", JSONType: resourcecontract.OutputTypeString, RequiredWhenReady: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if contract.RequiresOutputs() {
		t.Fatal("optional-only contract claims required outputs")
	}
	values := map[string]any{}
	if err := contract.Validate(values); err != nil {
		t.Fatalf("absence of optional outputs rejected: %v", err)
	}
}

func TestOutputContractValidationMatrix(t *testing.T) {
	contract, err := resourcecontract.NewOutputContract(sampleOutputFields())
	if err != nil {
		t.Fatal(err)
	}
	valid := []map[string]any{
		{"hostname": "db.example", "port": int64(5432)},
		{"hostname": "db.example", "port": float64(5432)},
		{"hostname": "db.example", "port": int(5432)},
	}
	for index, values := range valid {
		if err := contract.Validate(values); err != nil {
			t.Errorf("valid case %d rejected: %v", index, err)
		}
	}
	invalid := []struct {
		name   string
		values map[string]any
	}{
		{"undeclared field", map[string]any{"hostname": "h", "port": int64(1), "password": "hunter2"}},
		{"missing required hostname", map[string]any{"port": int64(1)}},
		{"missing required port", map[string]any{"hostname": "h"}},
		{"wrong type port", map[string]any{"hostname": "h", "port": "5432"}},
		{"wrong type hostname", map[string]any{"hostname": int64(5), "port": int64(1)}},
		{"fractional port", map[string]any{"hostname": "h", "port": float64(5432.5)}},
		{"boolean port", map[string]any{"hostname": "h", "port": true}},
		{"nested value", map[string]any{"hostname": map[string]any{}, "port": int64(1)}},
	}
	for _, tc := range invalid {
		if err := contract.Validate(tc.values); err == nil {
			t.Errorf("%s: invalid values accepted", tc.name)
		}
	}
}

func TestOutputContractValidateDoesNotMutateInput(t *testing.T) {
	contract, err := resourcecontract.NewOutputContract(sampleOutputFields())
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]any{"hostname": "db.example", "port": int64(5432)}
	if err := contract.Validate(values); err != nil {
		t.Fatal(err)
	}
	if len(values) != 2 {
		t.Fatalf("validation mutated the input map: %v", values)
	}
}
