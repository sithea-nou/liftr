// SPDX-License-Identifier: Apache-2.0

package postgresqldatabase

import (
	"errors"
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
)

// rawSpec builds developer intent with explicit numeric representations so
// transition tests can exercise int64 and integral float64 variants. Values
// are structurally valid under the v1 schema.
func rawSpec(t *testing.T, version string, storageGB any, highAvailability bool) domain.ResourceSpec {
	t.Helper()
	spec, err := domain.NewResourceSpec(map[string]any{"version": version, "storageGB": storageGB, "highAvailability": highAvailability})
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func TestValidateTransitionAcceptsLegalTransitions(t *testing.T) {
	tests := []struct {
		name    string
		oldSpec domain.ResourceSpec
		newSpec domain.ResourceSpec
	}{
		{name: "identical specs", oldSpec: rawSpec(t, "16", int64(20), false), newSpec: rawSpec(t, "16", int64(20), false)},
		{name: "storage increase", oldSpec: rawSpec(t, "16", int64(20), true), newSpec: rawSpec(t, "16", int64(50), true)},
		{name: "high availability enabled", oldSpec: rawSpec(t, "17", int64(20), false), newSpec: rawSpec(t, "17", int64(20), true)},
		{name: "high availability disabled", oldSpec: rawSpec(t, "17", int64(20), true), newSpec: rawSpec(t, "17", int64(20), false)},
		{name: "decimal representation of equal storage", oldSpec: rawSpec(t, "15", float64(20.0), false), newSpec: rawSpec(t, "15", int64(20), false)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if violations := ValidateTransition(test.oldSpec.Values(), test.newSpec.Values()); len(violations) != 0 {
				t.Fatalf("legal transition rejected: %+v", violations)
			}
			contract, err := Contract()
			if err != nil {
				t.Fatal(err)
			}
			if err := contract.ValidateUpdate(test.oldSpec, test.newSpec); err != nil {
				t.Fatalf("legal transition rejected by contract: %v", err)
			}
		})
	}
}

func TestValidateTransitionRejectsIllegalTransitions(t *testing.T) {
	tests := []struct {
		name     string
		oldSpec  domain.ResourceSpec
		newSpec  domain.ResourceSpec
		wantPath string
	}{
		{name: "engine version change", oldSpec: rawSpec(t, "16", int64(20), false), newSpec: rawSpec(t, "17", int64(20), false), wantPath: "/version"},
		{name: "storage decrease", oldSpec: rawSpec(t, "16", int64(50), false), newSpec: rawSpec(t, "16", int64(20), false), wantPath: "/storageGB"},
		{name: "storage decrease across representations", oldSpec: rawSpec(t, "16", float64(50.0), true), newSpec: rawSpec(t, "16", int64(49), true), wantPath: "/storageGB"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			violations := ValidateTransition(test.oldSpec.Values(), test.newSpec.Values())
			if len(violations) != 1 {
				t.Fatalf("want exactly one violation, got %+v", violations)
			}
			violation := violations[0]
			if violation.Path != test.wantPath || violation.Keyword != "transition" || strings.TrimSpace(violation.Message) == "" {
				t.Fatalf("violation = %+v", violation)
			}
		})
	}
}

// TestContractValidateUpdateSurfacesStructuredViolations pins that admission
// receives transition rejections as *resourcecontract.ValidationError so they map
// onto the structured RESOURCE_SPEC_INVALID problem channel.
func TestContractValidateUpdateSurfacesStructuredViolations(t *testing.T) {
	contract, err := Contract()
	if err != nil {
		t.Fatal(err)
	}
	err = contract.ValidateUpdate(rawSpec(t, "16", int64(20), false), rawSpec(t, "17", int64(20), false))
	var invalid *resourcecontract.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("ValidateUpdate error = %v, want *resourcecontract.ValidationError", err)
	}
	if invalid.TypeRef != TypeRef() || len(invalid.Violations) != 1 || invalid.Violations[0].Path != "/version" ||
		invalid.Violations[0].Keyword != "transition" {
		t.Fatalf("invalid spec error = %+v", invalid)
	}
}

func TestValidateTransitionDoesNotMutateInputs(t *testing.T) {
	oldValues := map[string]any{"version": "16", "storageGB": int64(20), "highAvailability": false}
	newValues := map[string]any{"version": "17", "storageGB": float64(10.0), "highAvailability": true}
	_ = ValidateTransition(oldValues, newValues)
	if oldValues["version"] != "16" || oldValues["storageGB"] != int64(20) || oldValues["highAvailability"] != false {
		t.Fatalf("old values mutated: %+v", oldValues)
	}
	if newValues["version"] != "17" || newValues["storageGB"] != float64(10.0) || newValues["highAvailability"] != true {
		t.Fatalf("new values mutated: %+v", newValues)
	}
}
