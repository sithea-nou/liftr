// SPDX-License-Identifier: Apache-2.0

package resourcetypes_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/resourcetypes"
)

func widgetVersion(t *testing.T, version string) resourcetypes.Contract {
	t.Helper()
	typeValue, err := domain.NewResourceType(
		domain.ResourceTypeRef{Name: "Widget", Version: version},
		"Test widget contract.",
		[]domain.Capability{domain.CapabilityCreate},
	)
	if err != nil {
		t.Fatal(err)
	}
	document := strings.ReplaceAll(minimalSchema, ":Widget:v1:", ":Widget:"+version+":")
	return mustContract(t, resourcetypes.ContractInput{Type: typeValue, SpecSchema: []byte(document)})
}

func gadgetType(t *testing.T) resourcetypes.Contract {
	t.Helper()
	typeValue, err := domain.NewResourceType(
		domain.ResourceTypeRef{Name: "Gadget", Version: "v1"},
		"Test gadget contract.",
		[]domain.Capability{domain.CapabilityDelete},
	)
	if err != nil {
		t.Fatal(err)
	}
	document := strings.ReplaceAll(minimalSchema, ":Widget:v1:", ":Gadget:v1:")
	return mustContract(t, resourcetypes.ContractInput{Type: typeValue, SpecSchema: []byte(document)})
}

// TestRegistryDeterministicOrdering pins the public ordering contract:
// name ascending, then version ascending, byte-wise, stable across calls.
func TestRegistryDeterministicOrdering(t *testing.T) {
	registry, err := resourcetypes.NewRegistry(
		widgetVersion(t, "v10"),
		gadgetType(t),
		widgetVersion(t, "v2"),
		widgetVersion(t, "v1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Gadget/v1", "Widget/v1", "Widget/v10", "Widget/v2"}
	assertOrder(t, registry, want)
	assertOrder(t, registry, want) // repeated calls agree
}

func assertOrder(t *testing.T, registry *resourcetypes.Registry, want []string) {
	t.Helper()
	contracts, err := registry.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(contracts) != len(want) {
		t.Fatalf("List() returned %d contracts, want %d", len(contracts), len(want))
	}
	for index, contract := range contracts {
		ref := contract.Ref()
		if got := ref.Name + "/" + ref.Version; got != want[index] {
			t.Fatalf("List()[%d] = %s, want %s", index, got, want[index])
		}
	}
}

func TestRegistryRejectsDuplicateRef(t *testing.T) {
	first := widgetVersion(t, "v1")
	second := widgetVersion(t, "v1")
	if first.SchemaDigest() != second.SchemaDigest() || first.Ref() != second.Ref() {
		t.Fatal("test setup produced divergent identical contracts")
	}
	if _, err := resourcetypes.NewRegistry(first, second); err == nil {
		t.Fatal("same ResourceTypeRef was registered twice")
	}
}

// TestRegistryRejectsConflictingSchemas pins that a ref cannot silently gain
// a different schema within one process: re-registration is rejected even
// before any digest-based persistence could detect it across restarts.
func TestRegistryRejectsConflictingSchemas(t *testing.T) {
	first := widgetVersion(t, "v1")
	mutatedDocument := strings.Replace(minimalSchema, `"minLength": 1`, `"minLength": 2`, 1)
	conflicting := mustContract(t, resourcetypes.ContractInput{
		Type:       first.Domain(),
		SpecSchema: []byte(mutatedDocument),
	})
	if conflicting.SchemaDigest() == first.SchemaDigest() {
		t.Fatal("conflicting contract has an identical digest")
	}
	registry, err := resourcetypes.NewRegistry(first)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(conflicting); err == nil {
		t.Fatal("conflicting schema registered under an existing ref")
	}

	// The original contract is untouched after the rejected registration.
	contract, err := registry.Get(context.Background(), first.Ref())
	if err != nil {
		t.Fatal(err)
	}
	if string(contract.SpecSchema()) != string(first.SpecSchema()) {
		t.Fatal("registered schema changed after a rejected conflicting registration")
	}
}

func TestRegistryGetUnknown(t *testing.T) {
	registry, err := resourcetypes.NewRegistry(widgetVersion(t, "v1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Get(context.Background(), domain.ResourceTypeRef{Name: "Missing", Version: "v1"}); !errors.Is(err, resourcetypes.ErrUnknownResourceType) {
		t.Fatalf("Get() error = %v, want ErrUnknownResourceType", err)
	}
}

func TestRegistryEmptyList(t *testing.T) {
	registry, err := resourcetypes.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	contracts, err := registry.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if contracts == nil || len(contracts) != 0 {
		t.Fatalf("empty List() = %#v, want empty non-nil slice", contracts)
	}
}

// TestRegistrySatisfiesApplicationPort pins structural satisfaction of the
// consumer-owned application port.
func TestRegistrySatisfiesApplicationPort(t *testing.T) {
	var catalog application.ResourceTypeCatalog = (*resourcetypes.Registry)(nil)
	_ = catalog
	var contract application.ResourceContract = resourcetypes.Contract{}
	_ = contract
}
