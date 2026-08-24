// SPDX-License-Identifier: Apache-2.0

package crossplane

import (
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube/fakeapi"
)

func envelopeObject(t *testing.T) map[string]any {
	return map[string]any{
		"version":          float64(outputEnvelopeVersion),
		"mapping":          testOutputMappingRef,
		"resourceId":       "resource-1",
		"targetGeneration": float64(1),
		"values":           map[string]any{"endpoint": "db.example.internal", "port": float64(5432)},
	}
}

func statusController(envelope any) fakeapi.Controller {
	return func(poll int, object *fakeapi.Object) {
		object.Raw["status"] = map[string]any{
			"conditions": conditionsAt(object.Generation(), syncedTrue(), readyTrue()),
			"liftr":      map[string]any{"outputs": envelope},
		}
	}
}

func observeWithMapping(t *testing.T, f *fixture, operationID string) provisioning.ExecutionObservation {
	t.Helper()
	return observeExecution(t, f.provisioner, provisioning.ObservationRequest{
		OperationID: domain.OperationID(operationID), AttemptNumber: 1,
		ResourceID: "resource-1", ResourceType: testResourceType,
		Capability: domain.CapabilityCreate, Spec: simpleSpec(t, true),
		TargetGeneration: 1, OutputMappingRef: testOutputMappingRef,
	})
}

func TestOutputsExtractedOnlyThroughRegisteredPath(t *testing.T) {
	f := newFixture(t, nil)
	f.submit(t, executionRequest(domain.CapabilityCreate, "op-out", 1, 1, simpleSpec(t, true)))

	// Before the composition patches the status envelope: success evidence
	// with unavailable outputs defers publication; it never re-executes.
	f.server.SetController(func(poll int, object *fakeapi.Object) {
		object.Raw["status"] = map[string]any{"conditions": conditionsAt(object.Generation(), syncedTrue(), readyTrue())}
	})
	deferred := observeWithMapping(t, f, "op-out")
	requireFound(t, deferred, provisioning.ExecutionStateSucceeded)
	if deferred.Outputs == nil || deferred.Outputs.State != provisioning.OutputsUnavailable {
		t.Fatalf("outputs before composition patch = %+v", deferred.Outputs)
	}
	if deferred.Outputs.Reason == "" || strings.Contains(strings.ToLower(deferred.Outputs.Reason), "missing") {
		t.Fatalf("curated reason required: %+v", deferred.Outputs)
	}

	// After the composition publishes the envelope: typed candidates flow
	// through the existing provider-neutral output evidence mechanism.
	f.server.SetController(statusController(envelopeObject(t)))
	published := observeWithMapping(t, f, "op-out")
	requireFound(t, published, provisioning.ExecutionStateSucceeded)
	if published.Outputs == nil || published.Outputs.State != provisioning.OutputsAvailable {
		t.Fatalf("outputs after patch = %+v", published.Outputs)
	}
	if published.Outputs.Values["endpoint"] != "db.example.internal" || published.Outputs.Values["port"] != float64(5432) {
		t.Fatalf("candidate values = %+v", published.Outputs.Values)
	}
	if published.Outputs.OutputMappingRef != testOutputMappingRef {
		t.Fatalf("evidence mapping ref = %q", published.Outputs.OutputMappingRef)
	}
}

func TestOutputsRejectForeignEnvelopesDeterministically(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(envelope map[string]any)
	}{
		{"wrong mapping identity", func(e map[string]any) { e["mapping"] = "someone-elses-mapping" }},
		{"wrong resource", func(e map[string]any) { e["resourceId"] = "other-resource" }},
		{"wrong generation", func(e map[string]any) { e["targetGeneration"] = float64(99) }},
		{"unknown field", func(e map[string]any) { e["extra"] = true }},
		{"nested value", func(e map[string]any) { e["values"].(map[string]any)["deep"] = map[string]any{"x": 1} }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			f := newFixture(t, nil)
			f.submit(t, executionRequest(domain.CapabilityCreate, "op-inv", 1, 1, simpleSpec(t, true)))
			envelope := envelopeObject(t)
			testCase.mutate(envelope)
			f.server.SetController(statusController(envelope))
			observation := observeWithMapping(t, f, "op-inv")
			requireFound(t, observation, provisioning.ExecutionStateSucceeded)
			if observation.Outputs == nil || observation.Outputs.State != provisioning.OutputsInvalid {
				t.Fatalf("outputs = %+v, want deterministic Invalid", observation.Outputs)
			}
		})
	}
}

// Output recovery is observe-only: the M13 repair path resolves through a
// compatible mapping without issuing a single mutating call (#25).
func TestOutputRecoveryObservesWithoutMutating(t *testing.T) {
	binding := testBinding()
	binding.OutputMappings = append(binding.OutputMappings, OutputMapping{
		Ref:                        "xrb-test-v2-repair",
		StatusPath:                 []string{"liftr", "outputs"},
		CompatibleSourceMappingRef: testOutputMappingRef,
	})
	f := newFixture(t, func(cfg *Config) { cfg.Bindings[0] = binding })

	repairRef, ok := f.provisioner.SelectOutputRecoveryMapping(testResourceType, domain.CapabilityCreate, testOutputMappingRef)
	if !ok || repairRef != "xrb-test-v2-repair" {
		t.Fatalf("repair selection = %q %v", repairRef, ok)
	}
	if _, selected := f.provisioner.SelectOutputRecoveryMapping(testResourceType, domain.CapabilityCreate, "unknown-source"); selected {
		t.Fatal("recovery must only select explicitly compatible sources")
	}

	f.submit(t, executionRequest(domain.CapabilityCreate, "op-src", 1, 1, simpleSpec(t, true)))
	writesBeforeRepair := f.server.WriteCount()
	f.server.SetController(statusController(envelopeObject(t)))
	observation := observeExecution(t, f.provisioner, provisioning.ObservationRequest{
		OperationID: "op-src", AttemptNumber: 1,
		ResourceID: "resource-1", ResourceType: testResourceType,
		Capability: domain.CapabilityCreate, Spec: simpleSpec(t, true),
		TargetGeneration:       1,
		OutputMappingRef:       repairRef,
		OutputSourceMappingRef: testOutputMappingRef,
	})
	requireFound(t, observation, provisioning.ExecutionStateSucceeded)
	if observation.Outputs == nil || observation.Outputs.State != provisioning.OutputsAvailable {
		t.Fatalf("recovery observation = %+v", observation.Outputs)
	}
	if got := f.server.WriteCount(); got != writesBeforeRepair {
		t.Fatalf("output recovery issued %d extra writes; it must be observe-only", got-writesBeforeRepair)
	}
}

func TestDeleteExecutionsNeverCarryOutputs(t *testing.T) {
	f := newFixture(t, nil)
	if ref := f.provisioner.OutputMappingRef(testResourceType, domain.CapabilityDelete); ref != "" {
		t.Fatalf("delete mapping ref = %q, want empty", ref)
	}
	if ref := f.provisioner.OutputMappingRef(testResourceType, domain.CapabilityCreate); ref != testOutputMappingRef {
		t.Fatalf("create mapping ref = %q", ref)
	}
}
