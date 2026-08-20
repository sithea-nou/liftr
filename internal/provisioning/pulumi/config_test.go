// SPDX-License-Identifier: Apache-2.0

package pulumi

import (
	"context"
	"reflect"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

func TestDuplicateProgramRegistrationForSameResourceTypeIsRejected(t *testing.T) {
	config := testConfig(t)
	create := config.Programs[0]
	create.Capabilities = []domain.Capability{domain.CapabilityCreate}
	update := create
	update.Capabilities = []domain.Capability{domain.CapabilityUpdate}
	config.Programs = []Program{create, update}
	if _, err := newProvisioner(config, &fakeFactory{}); err == nil {
		t.Fatal("duplicate Pulumi program registration for one resource type was accepted")
	}
}

func TestSeparateProgramRegistrationPerResourceTypeIsAccepted(t *testing.T) {
	config := testConfig(t)
	first := config.Programs[0]
	second := first
	second.ResourceType = domain.ResourceTypeRef{Name: "Other", Version: "v1"}
	config.Programs = []Program{first, second}
	if _, err := newProvisioner(config, &fakeFactory{}); err != nil {
		t.Fatal(err)
	}
}

func TestSingleProgramSupportsCreateUpdateAndDelete(t *testing.T) {
	config := testConfig(t)
	provider, err := newProvisioner(config, &fakeFactory{})
	if err != nil {
		t.Fatal(err)
	}
	got := provider.Capabilities()
	want := []provisioning.ProvisionerCapability{
		{ResourceType: testResourceType, Capability: domain.CapabilityCreate},
		{ResourceType: testResourceType, Capability: domain.CapabilityDelete},
		{ResourceType: testResourceType, Capability: domain.CapabilityUpdate},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("capabilities = %+v, want %+v", got, want)
	}
}

func TestUnsupportedSubmitCapabilityIsRejectedBeforePulumiInvocation(t *testing.T) {
	config := testConfig(t)
	config.Programs[0].Capabilities = []domain.Capability{domain.CapabilityCreate}
	factory := &fakeFactory{workspace: &fakeWorkspace{stack: &fakeStack{}}}
	provider, err := newProvisioner(config, factory)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := provider.Submit(context.Background(), executionRequest(t, domain.CapabilityUpdate))
	if err != nil {
		t.Fatal(err)
	}
	observation := submission.Observation
	if observation.Correlation != provisioning.RequestCorrelationNotFound || observation.Execution == nil || observation.Execution.State != provisioning.ExecutionStateFailed {
		t.Fatalf("submission = %+v", observation)
	}
	if factory.openCalls != 0 {
		t.Fatal("unsupported capability reached Automation API")
	}
}

func TestUnsupportedObserveCapabilityIsRejectedBeforePulumiInvocation(t *testing.T) {
	config := testConfig(t)
	config.Programs[0].Capabilities = []domain.Capability{domain.CapabilityCreate}
	factory := &fakeFactory{workspace: &fakeWorkspace{stack: &fakeStack{}}}
	provider, err := newProvisioner(config, factory)
	if err != nil {
		t.Fatal(err)
	}
	request := observationRequest(t)
	request.Capability = domain.CapabilityUpdate
	if _, err := provider.Observe(context.Background(), request); err == nil {
		t.Fatal("unsupported observation capability was not rejected")
	}
	if factory.openCalls != 0 {
		t.Fatal("unsupported observation capability reached Automation API")
	}
}

func TestAllCapabilitiesResolveOneStackIdentityForAResourceType(t *testing.T) {
	config := testConfig(t)
	stack := &fakeStack{summary: updateSummary{kind: "update", result: "succeeded"}}
	workspace := &fakeWorkspace{stack: stack}
	factory := &fakeFactory{workspace: workspace}
	provider, err := newProvisioner(config, factory)
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete} {
		workspace.selectErr = nil
		if capability == domain.CapabilityCreate {
			workspace.selectErr = errStackNotFound
		}
		stack.summary.kind = "update"
		if capability == domain.CapabilityDelete {
			stack.summary.kind = "destroy"
		}
		request := executionRequest(t, capability)
		stack.summary.message = correlationMessage(request.OperationID, request.AttemptNumber)
		submission, err := provider.Submit(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if submission.Observation.Correlation != provisioning.RequestCorrelationFound || submission.Observation.Execution.State != provisioning.ExecutionStateSucceeded {
			t.Fatalf("submission = %+v", submission.Observation)
		}
	}
	if len(workspace.stackNames) != 4 {
		t.Fatalf("stack selections = %d, want 4", len(workspace.stackNames))
	}
	for _, name := range workspace.stackNames {
		if name != workspace.stackNames[0] {
			t.Fatalf("stack identities differ across capabilities: %q", workspace.stackNames)
		}
	}
}

func TestStackNamingVersionValidation(t *testing.T) {
	tests := []struct {
		name    string
		version StackNamingVersion
		wantErr bool
	}{
		{name: "missing version rejected", version: "", wantErr: true},
		{name: "unsupported version rejected", version: "v2", wantErr: true},
		{name: "v1 accepted", version: StackNamingVersionV1, wantErr: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(t)
			config.StackNamingVersion = test.version
			_, err := newProvisioner(config, &fakeFactory{})
			if (err != nil) != test.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestStackNameV1Golden(t *testing.T) {
	const want = "organization/noop/liftr-971a81fa4bfa-6b53238e8319df599de01d81ce44d6b0d6f5b85e"
	got := stackNameV1("test-v1", "test", "noop", "resource-1")
	if got != want {
		t.Fatalf("stackNameV1 = %q, want %q", got, want)
	}
}

func TestStackIdentityIsIndependentOfCapabilityOperationGenerationAndAttempt(t *testing.T) {
	config := testConfig(t)
	provider, err := newProvisioner(config, &fakeFactory{})
	if err != nil {
		t.Fatal(err)
	}
	project := config.Programs[0].ProjectName
	base := provider.stackName(project, "resource-1")
	for _, capability := range []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete} {
		request := executionRequest(t, capability)
		request.OperationID = "different-operation"
		request.AttemptNumber = 7
		request.TargetGeneration = 42
		if got := provider.stackName(project, request.ResourceID); got != base {
			t.Fatalf("stack identity changed with %s: %q != %q", capability, got, base)
		}
	}
}
