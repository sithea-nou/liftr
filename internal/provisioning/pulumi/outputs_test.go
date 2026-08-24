// SPDX-License-Identifier: Apache-2.0

package pulumi

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
)

func newTestOutputContract() (resourcecontract.OutputContract, error) {
	return resourcecontract.NewOutputContract([]resourcecontract.OutputField{
		{Name: "hostname", JSONType: resourcecontract.OutputTypeString, RequiredWhenReady: true},
		{Name: "port", JSONType: resourcecontract.OutputTypeInteger, RequiredWhenReady: true},
	})
}

const (
	testMappingRef = "liftr-test-outputs-v1"
	outputsExport  = "liftrOutputs"
)

func envelopePayload(mapping string, resourceID string, generation uint64, valuesJSON string) string {
	return fmt.Sprintf(`{"version":1,"mapping":%q,"resourceId":%q,"targetGeneration":%d,"values":%s}`,
		mapping, resourceID, generation, valuesJSON)
}

func TestDecodeSelectedOutputEnvelopeAcceptsWellFormed(t *testing.T) {
	raw := envelopePayload(testMappingRef, "orders-db", 4, `{"hostname":"db.example","port":5432,"weight":0.5,"enabled":true}`)
	values, err := decodeSelectedOutputEnvelope([]byte(raw), testMappingRef, "orders-db", 4)
	if err != nil {
		t.Fatalf("well-formed envelope rejected: %v", err)
	}
	if values["hostname"] != "db.example" {
		t.Fatalf("hostname = %v", values["hostname"])
	}
	if port, ok := values["port"].(int64); !ok || port != 5432 {
		t.Fatalf("port = %#v (%T)", values["port"], values["port"])
	}
	if weight, ok := values["weight"].(float64); !ok || weight != 0.5 {
		t.Fatalf("weight = %#v (%T)", values["weight"], values["weight"])
	}
	if enabled, ok := values["enabled"].(bool); !ok || !enabled {
		t.Fatalf("enabled = %#v", values["enabled"])
	}
}

func TestDecodeSelectedOutputEnvelopeRejectsViolations(t *testing.T) {
	valid := envelopePayload(testMappingRef, "r", 4, `{"hostname":"h","port":5432}`)
	cases := []struct {
		name string
		raw  string
	}{
		{"empty document", ``},
		{"not an object", `[1,2]`},
		{"duplicate top-level keys", `{"version":1,"version":1,"mapping":"m","resourceId":"r","targetGeneration":4,"values":{}}`},
		{"unknown top-level field", `{"version":1,"mapping":"` + testMappingRef + `","resourceId":"r","targetGeneration":4,"values":{},"extra":true}`},
		{"wrong mapping identity", envelopePayload("liftr-other-v9", "r", 4, `{"a":"b"}`)},
		{"missing mapping", `{"version":1,"resourceId":"r","targetGeneration":4,"values":{}}`},
		{"wrong resource identity", envelopePayload(testMappingRef, "other-resource", 4, `{"a":"b"}`)},
		{"wrong target generation", envelopePayload(testMappingRef, "r", 5, `{"a":"b"}`)},
		{"unsupported version", `{"version":99,"mapping":"` + testMappingRef + `","resourceId":"r","targetGeneration":4,"values":{}}`},
		{"values not an object", envelopePayload(testMappingRef, "r", 4, `[1]`)},
		{"nested value", envelopePayload(testMappingRef, "r", 4, `{"conn":{"host":"h"}}`)},
		{"array value", envelopePayload(testMappingRef, "r", 4, `{"list":[1]}`)},
		{"null value", envelopePayload(testMappingRef, "r", 4, `{"x":null}`)},
		{"redacted secret string", envelopePayload(testMappingRef, "r", 4, `{"hostname":"[secret]"}`)},
		{"secret marker nested in array", envelopePayload(testMappingRef, "r", 4, `{"a":[["[secret]"]]}`)},
		{"trailing content", valid + `garbage`},
		{"oversized string", envelopePayload(testMappingRef, "r", 4, fmt.Sprintf(`{"s":%q}`, strings.Repeat("x", maxOutputStringLength+1)))},
	}
	for _, tc := range cases {
		if _, err := decodeSelectedOutputEnvelope([]byte(tc.raw), testMappingRef, "r", 4); err == nil {
			t.Errorf("%s: violation accepted", tc.name)
		}
	}
	// Oversized total document.
	huge := fmt.Sprintf(`{"version":1,"mapping":%q,"resourceId":"r","targetGeneration":4,"values":{"s":%q}}`,
		testMappingRef, strings.Repeat("x", maxOutputBytes))
	if _, err := decodeSelectedOutputEnvelope([]byte(huge), testMappingRef, "r", 4); err == nil {
		t.Error("oversized document accepted")
	}
}

// TestSelectedOutputCommandNeverRequestsSecrets pins the primary secret
// boundary at the command-construction level: the allowlisted name is the only
// retrieval surface and --show-secrets can never appear.
func TestSelectedOutputCommandNeverRequestsSecrets(t *testing.T) {
	args := selectedOutputArgs(outputsExport, "organization/project/stack")
	joined := strings.Join(args, " ")
	for _, banned := range []string{"--show-secrets", "export", "stack export", "--all"} {
		if strings.Contains(joined, banned) {
			t.Fatalf("selected-output command contains %q: %s", banned, joined)
		}
	}
	found := false
	for i, arg := range args {
		if arg == outputsExport && i >= 2 && args[i-1] == "output" {
			found = true
		}
	}
	if !found {
		t.Fatalf("selected output name is not the direct argument of stack output: %v", args)
	}
}

// TestAttachOutputsMatrix drives extraction classification through a fake
// stack without invoking any CLI.
func TestAttachOutputsMatrix(t *testing.T) {
	mapping := OutputMapping{Ref: testMappingRef, ExportName: outputsExport}
	program := Program{
		ResourceType:            domain.ResourceTypeRef{Name: "Widget", Version: "v2"},
		Capabilities:            []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete},
		OutputMappings:          []OutputMapping{mapping},
		CurrentOutputMappingRef: testMappingRef,
	}
	provider := &Provisioner{programs: map[domain.ResourceTypeRef]Program{program.ResourceType: program}}
	request := ObservationRequest{
		ResourceID: "r", ResourceType: program.ResourceType,
		Capability: domain.CapabilityCreate, TargetGeneration: 7,
		OutputMappingRef: testMappingRef,
	}
	success := func() provisioning.ExecutionObservation {
		handle, _ := provisioning.NewExecutionHandle("h")
		return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
			Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle}}
	}

	t.Run("available evidence", func(t *testing.T) {
		stack := &fakeStack{}
		payload := envelopePayload(testMappingRef, "r", 7, `{"hostname":"db.example","port":5432}`)
		stack.selectedOutput = func(string) []byte { return []byte(payload) }
		observation, err := provider.attachOutputs(context.Background(), stack, success(), request)
		if err != nil {
			t.Fatal(err)
		}
		if observation.Outputs == nil || observation.Outputs.State != provisioning.OutputsAvailable {
			t.Fatalf("outputs = %+v", observation.Outputs)
		}
	})

	t.Run("unresolved persisted mapping fails loudly", func(t *testing.T) {
		stack := &fakeStack{}
		broken := request
		broken.OutputMappingRef = "liftr-gone-v0"
		if _, err := provider.attachOutputs(context.Background(), stack, success(), broken); err == nil {
			t.Fatal("missing persisted mapping did not fail loudly")
		}
	})

	t.Run("unbound execution fails loudly when mapping registered", func(t *testing.T) {
		stack := &fakeStack{}
		unbound := request
		unbound.OutputMappingRef = ""
		if _, err := provider.attachOutputs(context.Background(), stack, success(), unbound); err == nil {
			t.Fatal("unbound execution with registered mapping did not fail loudly")
		}
	})

	t.Run("cli failure is transient unavailable evidence", func(t *testing.T) {
		stack := &fakeStack{} // no payload configured: read fails
		observation, err := provider.attachOutputs(context.Background(), stack, success(), request)
		if err != nil {
			t.Fatalf("transient read surfaced as error: %v", err)
		}
		if observation.Outputs == nil || observation.Outputs.State != provisioning.OutputsUnavailable {
			t.Fatalf("outputs = %+v", observation.Outputs)
		}
	})

	t.Run("contract-violating envelope is invalid evidence", func(t *testing.T) {
		stack := &fakeStack{}
		stack.selectedOutput = func(string) []byte {
			return []byte(envelopePayload("liftr-wrong-ref", "r", 7, `{}`))
		}
		observation, err := provider.attachOutputs(context.Background(), stack, success(), request)
		if err != nil {
			t.Fatal(err)
		}
		if observation.Outputs == nil || observation.Outputs.State != provisioning.OutputsInvalid {
			t.Fatalf("outputs = %+v", observation.Outputs)
		}
	})

	t.Run("nonterminal observations carry no claim", func(t *testing.T) {
		stack := &fakeStack{}
		running := success()
		running.Execution.State = provisioning.ExecutionStateRunning
		observation, err := provider.attachOutputs(context.Background(), stack, running, request)
		if err != nil {
			t.Fatal(err)
		}
		if observation.Outputs != nil {
			t.Fatalf("outputs = %+v", observation.Outputs)
		}
	})

	t.Run("delete never extracts", func(t *testing.T) {
		stack := &fakeStack{}
		deleteRequest := request
		deleteRequest.Capability = domain.CapabilityDelete
		observation, err := provider.attachOutputs(context.Background(), stack, success(), deleteRequest)
		if err != nil {
			t.Fatal(err)
		}
		if observation.Outputs != nil {
			t.Fatalf("outputs = %+v", observation.Outputs)
		}
	})
}

func TestProvisionerDeclaresMappingIdentityPerCapability(t *testing.T) {
	mapping := OutputMapping{Ref: testMappingRef, ExportName: outputsExport}
	ref := domain.ResourceTypeRef{Name: "Widget", Version: "v2"}
	program := Program{ResourceType: ref, Capabilities: []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete},
		OutputMappings: []OutputMapping{mapping}, CurrentOutputMappingRef: testMappingRef}
	provider := &Provisioner{programs: map[domain.ResourceTypeRef]Program{ref: program}}
	if got := provider.OutputMappingRef(ref, domain.CapabilityCreate); got != testMappingRef {
		t.Fatalf("create mapping ref = %q", got)
	}
	if got := provider.OutputMappingRef(ref, domain.CapabilityUpdate); got != testMappingRef {
		t.Fatalf("update mapping ref = %q", got)
	}
	if got := provider.OutputMappingRef(ref, domain.CapabilityDelete); got != "" {
		t.Fatalf("delete mapping ref = %q", got)
	}
}

func TestSelectOutputRecoveryMappingUsesExplicitCurrentAndExactCompatibility(t *testing.T) {
	ref := domain.ResourceTypeRef{Name: "Widget", Version: "v2"}
	program := Program{
		ResourceType: ref,
		Capabilities: []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete},
		OutputMappings: []OutputMapping{
			{Ref: "mapping-v9", ExportName: "outputsV9"},
			{Ref: "mapping-v2-repair", ExportName: "outputsRepair", CompatibleSourceMappingRef: "mapping-v1"},
			{Ref: "mapping-v2", ExportName: "outputsV2"},
		},
		CurrentOutputMappingRef: "mapping-v2",
	}
	provider := &Provisioner{programs: map[domain.ResourceTypeRef]Program{ref: program}}

	if got := provider.OutputMappingRef(ref, domain.CapabilityCreate); got != "mapping-v2" {
		t.Fatalf("fresh mapping = %q, want explicit current mapping-v2", got)
	}
	if got, ok := provider.SelectOutputRecoveryMapping(ref, domain.CapabilityCreate, "mapping-v1"); !ok || got != "mapping-v2-repair" {
		t.Fatalf("repair selection = %q, %t", got, ok)
	}
	if got, ok := provider.SelectOutputRecoveryMapping(ref, domain.CapabilityUpdate, "mapping-v9"); ok || got != "" {
		t.Fatalf("original mapping selected as recovery = %q, %t", got, ok)
	}
	if got, ok := provider.SelectOutputRecoveryMapping(ref, domain.CapabilityCreate, "mapping-v8"); ok || got != "" {
		t.Fatalf("unknown source selected as %q", got)
	}
	if got, ok := provider.SelectOutputRecoveryMapping(ref, domain.CapabilityDelete, "mapping-v1"); ok || got != "" {
		t.Fatalf("delete selected mapping %q", got)
	}
}

func TestObserveWithRepairMappingAcceptsSourceEnvelopeAndReportsSelectedProvenance(t *testing.T) {
	config := testConfig(t)
	config.Programs[0].OutputMappings = []OutputMapping{
		{Ref: "mapping-v1", ExportName: "outputsV1"},
		{Ref: "mapping-v2-repair", ExportName: "outputsRepair", CompatibleSourceMappingRef: "mapping-v1"},
	}
	config.Programs[0].CurrentOutputMappingRef = "mapping-v2-repair"
	request := observationRequest(t)
	request.OutputMappingRef = "mapping-v2-repair"
	request.OutputSourceMappingRef = "mapping-v1"
	message := correlationMessage(request.OperationID, request.AttemptNumber)
	selectedName := ""
	stack := &fakeStack{
		pages: map[int][]updateSummary{1: {{kind: "update", result: "succeeded", message: message}}},
		selectedOutput: func(name string) []byte {
			selectedName = name
			return []byte(envelopePayload("mapping-v1", string(request.ResourceID), request.TargetGeneration, `{"hostname":"db.example","port":5432}`))
		},
	}
	provider, err := newProvisioner(config, &fakeFactory{workspace: &fakeWorkspace{stack: stack}})
	if err != nil {
		t.Fatal(err)
	}

	observation, err := provider.Observe(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if selectedName != "outputsRepair" {
		t.Fatalf("selected export = %q, want repaired mapping export", selectedName)
	}
	if observation.Outputs == nil || observation.Outputs.State != provisioning.OutputsAvailable {
		t.Fatalf("outputs = %+v", observation.Outputs)
	}
	if observation.Outputs.OutputMappingRef != "mapping-v2-repair" {
		t.Fatalf("output provenance = %q", observation.Outputs.OutputMappingRef)
	}
	request.OutputSourceMappingRef = "mapping-v0"
	if _, err := provider.Observe(context.Background(), request); err == nil {
		t.Fatal("repair mapping accepted a different source mapping identity")
	}
}

func TestOrdinaryObservationUsesSelectedMappingIdentityEvenWhenItDeclaresRepairCompatibility(t *testing.T) {
	ref := domain.ResourceTypeRef{Name: "Widget", Version: "v2"}
	program := Program{ResourceType: ref, Capabilities: []domain.Capability{domain.CapabilityCreate},
		OutputMappings:          []OutputMapping{{Ref: "mapping-v2", ExportName: "outputsV2", CompatibleSourceMappingRef: "mapping-v1"}},
		CurrentOutputMappingRef: "mapping-v2"}
	provider := &Provisioner{programs: map[domain.ResourceTypeRef]Program{ref: program}}
	request := ObservationRequest{ResourceID: "r", ResourceType: ref, Capability: domain.CapabilityCreate,
		TargetGeneration: 2, OutputMappingRef: "mapping-v2"}
	stack := &fakeStack{selectedOutput: func(string) []byte {
		return []byte(envelopePayload("mapping-v2", "r", 2, `{"hostname":"db.example","port":5432}`))
	}}
	handle, _ := provisioning.NewExecutionHandle("h")
	observation, err := provider.attachOutputs(context.Background(), stack, provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle},
	}, request)
	if err != nil || observation.Outputs == nil || observation.Outputs.State != provisioning.OutputsAvailable {
		t.Fatalf("ordinary compatible mapping observation = %+v, %v", observation.Outputs, err)
	}
}

// TestDecoderErrorsNeverCarryValues pins the leak surface: decoder and
// mapping failures name fields, refs, and bounds only — never candidate
// values, never raw CLI output.
func TestDecoderErrorsNeverCarryValues(t *testing.T) {
	secrets := []string{"hunter2", "s3cr3t-password", "supersecretvalue"}
	cases := []struct {
		name string
		raw  string
	}{
		{"nested value carrying material", `{"version":1,"mapping":"` + testMappingRef + `","resourceId":"r","targetGeneration":4,"values":{"conn":{"password":"hunter2"}}}`},
		{"duplicate key near material", `{"version":1,"version":1,"mapping":"m","resourceId":"r","values":{"password":"hunter2"}}`},
		{"redacted marker", `{"version":1,"mapping":"` + testMappingRef + `","resourceId":"r","targetGeneration":4,"values":{"hostname":"[secret]hunter2"}}`},
	}
	for _, tc := range cases {
		_, err := decodeSelectedOutputEnvelope([]byte(tc.raw), testMappingRef, "r", 4)
		if err == nil {
			t.Fatalf("%s: violation accepted", tc.name)
		}
		for _, secret := range secrets {
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("%s: error carries value-like content %q: %s", tc.name, secret, err.Error())
			}
		}
	}

}

// Structurally valid candidates carrying undeclared fields cross to the
// contract boundary as Available evidence; rejection happens at the
// ResourceType contract, whose messages name fields but never values.
func TestContractRejectionMessagesNeverCarryValues(t *testing.T) {
	fields, err := newTestOutputContract()
	if err != nil {
		t.Fatal(err)
	}
	err = fields.Validate(map[string]any{"hostname": "h", "port": int64(5432), "password": "hunter2"})
	if err == nil {
		t.Fatal("undeclared field accepted")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("contract rejection carries a value: %s", err.Error())
	}
}
