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

func testRequest(id domain.OperationID, capability domain.Capability) provisioning.ExecutionRequest {
	spec, _ := domain.NewResourceSpec(map[string]any{"intent": "test"})
	return provisioning.ExecutionRequest{
		OperationID:      id,
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
		ResourceID:       "resource-1",
		ResourceType:     domain.ResourceTypeRef{Name: "FakeResource", Version: "v1"},
		Spec:             spec,
		TargetGeneration: 1,
	}
}
