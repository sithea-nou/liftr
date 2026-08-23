// SPDX-License-Identifier: Apache-2.0

package postgresqldatabase_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
	"github.com/sithea-nou/liftr/internal/resourcetypes"
	"github.com/sithea-nou/liftr/internal/resourcetypes/postgresqldatabase"
)

// TestV1ContractUnchanged pins Correction F: the released v1 contract stays
// byte-identical, spec-only, and exposes no outputContract.
func TestV1ContractUnchanged(t *testing.T) {
	document := postgresqldatabase.SpecSchemaDocument()
	var schema map[string]any
	if err := json.Unmarshal(document, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["$id"] != "urn:liftr:resource-type:PostgreSQLDatabase:v1:spec" {
		t.Fatalf("v1 $id = %v", schema["$id"])
	}
	contract, err := postgresqldatabase.Contract()
	if err != nil {
		t.Fatal(err)
	}
	if string(contract.SpecSchema()) != string(document) {
		t.Fatal("v1 contract schema drifted from its registered document")
	}
	if contract.OutputContract() != nil {
		t.Fatal("v1 must not declare an output contract")
	}
	if contract.Ref().Version != "v1" {
		t.Fatalf("v1 ref = %#v", contract.Ref())
	}
}

// TestV2ContractDeclaresOutputs pins Correction G: v2 declares exactly
// hostname (string) and port (integer), both required.
func TestV2ContractDeclaresOutputs(t *testing.T) {
	contract, err := postgresqldatabase.ContractV2()
	if err != nil {
		t.Fatal(err)
	}
	if contract.Ref() != postgresqldatabase.V2TypeRef() {
		t.Fatalf("ref = %#v", contract.Ref())
	}
	var schema map[string]any
	if err := json.Unmarshal(contract.SpecSchema(), &schema); err != nil {
		t.Fatal(err)
	}
	if schema["$id"] != "urn:liftr:resource-type:PostgreSQLDatabase:v2:spec" {
		t.Fatalf("v2 $id = %v", schema["$id"])
	}
	properties := schema["properties"].(map[string]any)
	for _, field := range []string{"version", "storageGB", "highAvailability"} {
		if _, ok := properties[field]; !ok {
			t.Errorf("v2 input field %q missing", field)
		}
	}
	if len(properties) != 3 {
		t.Fatalf("v2 input fields = %v", properties)
	}

	outputs := contract.OutputContract()
	if outputs == nil {
		t.Fatal("v2 must declare an output contract")
	}
	fields := outputs.Fields()
	if len(fields) != 2 {
		t.Fatalf("output fields = %+v", fields)
	}
	if fields[0].Name != "hostname" || fields[0].JSONType != resourcecontract.OutputTypeString || !fields[0].RequiredWhenReady {
		t.Fatalf("hostname declaration = %+v", fields[0])
	}
	if fields[1].Name != "port" || fields[1].JSONType != resourcecontract.OutputTypeInteger || !fields[1].RequiredWhenReady {
		t.Fatalf("port declaration = %+v", fields[1])
	}

	// Required postcondition validation.
	valid := map[string]any{"hostname": "orders-db.postgres.database.azure.com", "port": int64(5432)}
	if err := contract.ValidateOutputValues(valid); err != nil {
		t.Fatalf("valid values rejected: %v", err)
	}
	if err := contract.ValidateOutputValues(map[string]any{"hostname": "h"}); err == nil {
		t.Fatal("missing port accepted")
	}
	if err := contract.ValidateOutputValues(map[string]any{"hostname": "h", "port": int64(5432), "azureResourceId": "/subscriptions/x"}); err == nil {
		t.Fatal("provider implementation identifier accepted as an output")
	}
	if err := contract.ValidateOutputValues(map[string]any{}); err == nil {
		t.Fatal("empty values accepted for a type with required outputs")
	}
}

func TestV2TransitionRulesMatchV1Semantics(t *testing.T) {
	oldValues := map[string]any{"version": "16", "storageGB": int64(20), "highAvailability": false}

	growth := map[string]any{"version": "16", "storageGB": int64(40), "highAvailability": true}
	if violations := postgresqldatabase.ValidateTransitionV2(oldValues, growth); len(violations) != 0 {
		t.Fatalf("legal transition rejected: %+v", violations)
	}

	shrink := map[string]any{"version": "16", "storageGB": int64(10), "highAvailability": false}
	violations := postgresqldatabase.ValidateTransitionV2(oldValues, shrink)
	if len(violations) != 1 || violations[0].Path != "/storageGB" {
		t.Fatalf("shrink violations = %+v", violations)
	}

	versionChange := map[string]any{"version": "17", "storageGB": int64(20), "highAvailability": false}
	violations = postgresqldatabase.ValidateTransitionV2(oldValues, versionChange)
	if len(violations) != 1 || violations[0].Path != "/version" {
		t.Fatalf("version-change violations = %+v", violations)
	}
	if !strings.Contains(violations[0].Message, "PostgreSQLDatabase/v2") {
		t.Fatalf("v2 message does not name v2: %q", violations[0].Message)
	}
}

func TestV2AdmissionRejectsIllegalTransitionThroughContractChannel(t *testing.T) {
	contract, err := postgresqldatabase.ContractV2()
	if err != nil {
		t.Fatal(err)
	}
	oldSpec := mustSpecV2(t, map[string]any{"version": "16", "storageGB": int64(20), "highAvailability": false})
	newSpec := mustSpecV2(t, map[string]any{"version": "17", "storageGB": int64(20), "highAvailability": false})
	err = contract.ValidateUpdate(oldSpec, newSpec)
	var invalid *resourcecontract.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v (%T), want *resourcecontract.ValidationError", err, err)
	}
	if invalid.TypeRef.Version != "v2" {
		t.Fatalf("rejection TypeRef = %#v", invalid.TypeRef)
	}
}

func TestBothVersionsRegisterIndependently(t *testing.T) {
	v1, err := postgresqldatabase.Contract()
	if err != nil {
		t.Fatal(err)
	}
	v2, err := postgresqldatabase.ContractV2()
	if err != nil {
		t.Fatal(err)
	}
	registry, err := resourcetypes.NewRegistry(v1, v2)
	if err != nil {
		t.Fatalf("v1 and v2 cannot coexist in one registry: %v", err)
	}
	contracts, err := registry.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(contracts) != 2 {
		t.Fatalf("registry holds %d contracts", len(contracts))
	}
}

func TestNewSpecV2ValidatesAgainstV2Schema(t *testing.T) {
	spec, err := postgresqldatabase.NewSpecV2("16", 20, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := spec.Values()["storageGB"]; !ok {
		t.Fatal("spec values missing")
	}
	if _, err := postgresqldatabase.NewSpecV2("16", 0, false); err == nil {
		t.Fatal("invalid storage accepted")
	}
}

func mustSpecV2(t *testing.T, values map[string]any) domain.ResourceSpec {
	t.Helper()
	spec, err := domain.NewResourceSpec(values)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}
