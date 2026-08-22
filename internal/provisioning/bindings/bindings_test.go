// SPDX-License-Identifier: Apache-2.0

// Package bindings_test pins the private program-input envelope contract.
package bindings_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning/bindings"
	"github.com/sithea-nou/liftr/internal/provisioning/pulumi"
)

var postgresRef = domain.ResourceTypeRef{Name: "PostgreSQLDatabase", Version: "v1"}

func platform() bindings.PostgresPlatform {
	return bindings.PostgresPlatform{
		Location: "eastus", SkuName: "Standard_D2ds_v4",
		SkuTier: "GeneralPurpose", HighAvailabilityMode: "SameZone", AdministratorLogin: "liftradmin",
	}
}

func request(storageGB any, capability domain.Capability) bindings.PostgresRequest {
	return bindings.PostgresRequest{
		Capability:       capability,
		ResourceID:       "resource-1",
		ResourceType:     postgresRef,
		TargetGeneration: 1,
		SpecValues: map[string]any{
			"version": "16", "storageGB": storageGB, "highAvailability": false,
		},
		InfraName: "liftr-0123456789abcdef0123",
		Platform:  platform(),
	}
}

func TestEncodePostgresRequestMapsIntegerAndDecimalIdentically(t *testing.T) {
	fromInt, err := bindings.EncodePostgresRequest(request(int64(20), domain.CapabilityCreate))
	if err != nil {
		t.Fatal(err)
	}
	fromFloat, err := bindings.EncodePostgresRequest(request(float64(20.0), domain.CapabilityCreate))
	if err != nil {
		t.Fatal(err)
	}
	var intEnvelope, floatEnvelope map[string]any
	if err := json.Unmarshal(fromInt, &intEnvelope); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fromFloat, &floatEnvelope); err != nil {
		t.Fatal(err)
	}
	intStorage := intEnvelope["spec"].(map[string]any)["storageGB"]
	floatStorage := floatEnvelope["spec"].(map[string]any)["storageGB"]
	if intStorage != float64(20) || floatStorage != float64(20) {
		t.Fatalf("storage mapping diverged: %v vs %v", intStorage, floatStorage)
	}
	// The canonical encoding carries no fractional notation.
	if strings.Contains(string(fromInt), "20.0") || strings.Contains(string(fromFloat), "20.0") {
		t.Fatalf("non-canonical storage encoding: %s", string(fromInt))
	}
}

func TestEncodePostgresRequestRejectsUnsafeValues(t *testing.T) {
	tests := []struct {
		name      string
		specValue any
	}{
		{name: "fractional", specValue: 20.5},
		{name: "string", specValue: "20"},
		{name: "bool", specValue: true},
		{name: "nil", specValue: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := bindings.EncodePostgresRequest(request(test.specValue, domain.CapabilityCreate)); err == nil {
				t.Fatalf("unsafe value %v was accepted", test.specValue)
			}
		})
	}
}

func TestEncodePostgresRequestRejectsMissingFields(t *testing.T) {
	base := request(int64(20), domain.CapabilityCreate)
	for name := range base.SpecValues {
		partial := base
		partial.SpecValues = map[string]any{}
		for key, value := range base.SpecValues {
			if key != name {
				partial.SpecValues[key] = value
			}
		}
		if _, err := bindings.EncodePostgresRequest(partial); err == nil {
			t.Fatalf("missing %q was accepted", name)
		}
	}
}

func TestEncodePostgresRequestRejectsUnsupportedCapability(t *testing.T) {
	if _, err := bindings.EncodePostgresRequest(request(int64(20), domain.CapabilityObserve)); err == nil {
		t.Fatal("observe capability reached the program envelope")
	}
}

func TestEnvelopeCarriesNeutralIdentityOnly(t *testing.T) {
	encoded, err := bindings.EncodePostgresRequest(request(int64(30), domain.CapabilityUpdate))
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"inputVersion", "capability", "resourceId", "resourceTypeName", "resourceTypeVersion", "targetGeneration", "infraName", "platform", "spec"} {
		if _, ok := envelope[required]; !ok {
			t.Fatalf("envelope missing %q", required)
		}
	}
	for _, forbidden := range []string{"operationId", "attemptNumber"} {
		if _, leaked := envelope[forbidden]; leaked {
			t.Fatalf("envelope leaks execution correlation field %q", forbidden)
		}
	}
}

func TestPostgresEncoderRejectsIncompletePlatformBeforeEncoding(t *testing.T) {
	broken := platform()
	broken.SkuTier = ""
	encoder := bindings.PostgresEncoder("identity", "namespace", broken)
	values, err := domain.NewResourceSpec(map[string]any{"version": "16", "storageGB": int64(20), "highAvailability": false})
	if err != nil {
		t.Fatal(err)
	}
	input := pulumi.Input{
		ResourceID:       "resource-1",
		ResourceType:     postgresRef,
		Capability:       domain.CapabilityCreate,
		Spec:             values,
		TargetGeneration: 1,
	}
	if _, err := encoder(input); err == nil {
		t.Fatal("encoder accepted incomplete platform configuration")
	}
}

func TestPostgresEncoderProducesDeterministicEnvelope(t *testing.T) {
	encoder := bindings.PostgresEncoder("identity", "namespace", platform())
	values, err := domain.NewResourceSpec(map[string]any{"version": "16", "storageGB": float64(20.0), "highAvailability": true})
	if err != nil {
		t.Fatal(err)
	}
	input := pulumi.Input{
		OperationID:      domain.OperationID("op-1"),
		AttemptNumber:    3,
		ResourceID:       "resource-1",
		ResourceType:     postgresRef,
		Capability:       domain.CapabilityCreate,
		Spec:             values,
		TargetGeneration: 7,
	}
	first, err := encoder(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := encoder(input)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("encoder output is not deterministic for identical input")
	}
	if strings.Contains(string(first), "op-1") || strings.Contains(string(first), "\"3\"") {
		t.Fatalf("envelope leaked execution correlation data: %s", string(first))
	}
}
