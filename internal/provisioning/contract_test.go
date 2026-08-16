// SPDX-License-Identifier: Apache-2.0

package provisioning_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

func TestExecutionObservationDistinguishesNoExecutionFromUnknown(t *testing.T) {
	ready := provisioning.ExecutionObservation{
		Resource: provisioning.ResourceObservation{
			Presence:  provisioning.ResourcePresencePresent,
			Readiness: provisioning.ResourceReadinessReady,
			Drift:     provisioning.ResourceDriftInSync,
		},
	}
	if ready.Execution != nil {
		t.Fatal("ready resource unexpectedly has an active execution")
	}

	unknown := provisioning.ExecutionObservation{
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateUnknown},
	}
	if unknown.Execution == nil || unknown.Execution.State != provisioning.ExecutionStateUnknown {
		t.Fatal("unknown execution was not represented distinctly")
	}
}

func TestRequestCorrelationIsIndependentFromCurrentExecution(t *testing.T) {
	observation := provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationNotFound}
	if observation.Execution != nil {
		t.Fatal("request lookup unexpectedly reported a current execution")
	}
	if observation.Correlation != provisioning.RequestCorrelationNotFound {
		t.Fatal("request correlation evidence was not retained")
	}
}

func TestExecutionHandleHasNoProviderSpecificSurface(t *testing.T) {
	typ := reflect.TypeOf(provisioning.ExecutionHandle{})
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if field.PkgPath == "" {
			t.Fatalf("ExecutionHandle exposes field %q", field.Name)
		}
	}

	for _, name := range []string{"Pulumi", "Terraform", "Crossplane", "Git", "Kubernetes", "Cloud"} {
		if strings.Contains(typ.String(), name) {
			t.Fatalf("ExecutionHandle contains provider-specific name %q", name)
		}
	}
}

func TestExecutionRequestContainsNoLifecyclePhase(t *testing.T) {
	typ := reflect.TypeOf(provisioning.ExecutionRequest{})
	if _, ok := typ.FieldByName("Phase"); ok {
		t.Fatal("ExecutionRequest exposes Liftr OperationPhase")
	}
	if _, ok := typ.FieldByName("State"); ok {
		t.Fatal("ExecutionRequest exposes Liftr OperationState")
	}
	if _, ok := typ.FieldByName("Event"); ok {
		t.Fatal("ExecutionRequest exposes Liftr Event")
	}
}

func TestProvisioningContractsContainNoPulumiSurface(t *testing.T) {
	types := []reflect.Type{
		reflect.TypeOf(provisioning.ExecutionRequest{}),
		reflect.TypeOf(provisioning.ObservationRequest{}),
		reflect.TypeOf(provisioning.ExecutionObservation{}),
	}
	for _, typ := range types {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			value := strings.ToLower(field.Name + " " + field.Type.String() + " " + field.Type.PkgPath())
			for _, forbidden := range []string{"pulumi", "stack", "workspace", "backend", "projectpath", "clioutput"} {
				if strings.Contains(value, forbidden) {
					t.Fatalf("%s.%s exposes %q", typ.Name(), field.Name, forbidden)
				}
			}
		}
	}
}

func TestNormalizedFailureError(t *testing.T) {
	failure := provisioning.ExecutionFailure{
		Kind:    provisioning.FailureUnavailable,
		Reason:  "BackendUnavailable",
		Message: "temporarily unavailable",
	}
	if got := failure.Error(); !strings.Contains(got, "BackendUnavailable") {
		t.Fatalf("failure error = %q, want reason", got)
	}
}

func TestObservationCorrelationFieldsAreJointlyPresentOrAbsent(t *testing.T) {
	operation := testObservationRequest("operation-1")
	if err := operation.Validate(); err != nil {
		t.Fatalf("operation observation rejected: %v", err)
	}
	passive := operation
	passive.OperationID = ""
	passive.AttemptNumber = 0
	passive.Capability = ""
	if err := passive.Validate(); err != nil {
		t.Fatalf("passive observation rejected: %v", err)
	}
	partial := operation
	partial.AttemptNumber = 0
	if err := partial.Validate(); err == nil {
		t.Fatal("partial operation correlation was accepted")
	}
}

func testRequest(id domain.OperationID, capability domain.Capability) provisioning.ExecutionRequest {
	spec, _ := domain.NewResourceSpec(map[string]any{"intent": "test"})
	return provisioning.ExecutionRequest{
		OperationID:      id,
		AttemptNumber:    1,
		ResourceID:       "resource-1",
		ResourceType:     domain.ResourceTypeRef{Name: "FakeResource", Version: "v1"},
		Spec:             spec,
		Capability:       capability,
		TargetGeneration: 1,
	}
}

func testObservationRequest(id domain.OperationID) provisioning.ObservationRequest {
	spec, _ := domain.NewResourceSpec(map[string]any{"intent": "test"})
	return provisioning.ObservationRequest{
		OperationID:      id,
		AttemptNumber:    1,
		ResourceID:       "resource-1",
		ResourceType:     domain.ResourceTypeRef{Name: "FakeResource", Version: "v1"},
		Spec:             spec,
		Capability:       domain.CapabilityCreate,
		TargetGeneration: 1,
	}
}
