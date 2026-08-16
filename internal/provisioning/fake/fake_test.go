// SPDX-License-Identifier: Apache-2.0

package fake_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/provisioning/fake"
)

func TestFakeExecutionModels(t *testing.T) {
	tests := []struct {
		name             string
		mode             fake.Mode
		wantSubmitState  provisioning.ExecutionState
		wantObserveState []provisioning.ExecutionState
		wantReady        []provisioning.ResourceReadiness
		wantExecutionNil []bool
	}{
		{
			name:             "synchronous",
			mode:             fake.ModeSynchronous,
			wantSubmitState:  provisioning.ExecutionStateSucceeded,
			wantObserveState: []provisioning.ExecutionState{provisioning.ExecutionStateSucceeded},
			wantReady:        []provisioning.ResourceReadiness{provisioning.ResourceReadinessReady},
			wantExecutionNil: []bool{false},
		},
		{
			name:             "asynchronous",
			mode:             fake.ModeAsynchronous,
			wantSubmitState:  provisioning.ExecutionStateAccepted,
			wantObserveState: []provisioning.ExecutionState{provisioning.ExecutionStateRunning, provisioning.ExecutionStateSucceeded},
			wantReady:        []provisioning.ResourceReadiness{provisioning.ResourceReadinessNotReady, provisioning.ResourceReadinessReady},
			wantExecutionNil: []bool{false, false},
		},
		{
			name:             "declarative",
			mode:             fake.ModeDeclarative,
			wantSubmitState:  provisioning.ExecutionStateAccepted,
			wantObserveState: []provisioning.ExecutionState{},
			wantReady:        []provisioning.ResourceReadiness{provisioning.ResourceReadinessNotReady, provisioning.ResourceReadinessReady},
			wantExecutionNil: []bool{true, true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := fake.New(tt.mode)
			request := testRequest("operation-1", domain.CapabilityCreate)
			submission, err := provider.Submit(context.Background(), request)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if submission.Observation.Execution == nil || submission.Observation.Execution.State != tt.wantSubmitState {
				t.Fatalf("Submit() execution = %#v, want %s", submission.Observation.Execution, tt.wantSubmitState)
			}

			for i, wantState := range tt.wantObserveState {
				observation, observeErr := provider.Observe(context.Background(), testObservationRequest("operation-1"))
				if observeErr != nil {
					t.Fatalf("Observe() error = %v", observeErr)
				}
				if observation.Resource.Readiness != tt.wantReady[i] {
					t.Fatalf("Observe() readiness = %s, want %s", observation.Resource.Readiness, tt.wantReady[i])
				}
				if (observation.Execution == nil) != tt.wantExecutionNil[i] {
					t.Fatalf("Observe() execution nil = %t, want %t", observation.Execution == nil, tt.wantExecutionNil[i])
				}
				if observation.Execution != nil && observation.Execution.State != wantState {
					t.Fatalf("Observe() execution state = %s, want %s", observation.Execution.State, wantState)
				}
			}
		})
	}
}

func TestFakeExistingResourceHasNoExecution(t *testing.T) {
	provider := fake.New(fake.ModeExisting)
	observation, err := provider.Observe(context.Background(), testObservationRequest(""))
	if err != nil {
		t.Fatalf("Observe() error = %v", err)
	}
	if observation.Execution != nil {
		t.Fatal("existing resource has an unexpected active execution")
	}
	if observation.Resource.Presence != provisioning.ResourcePresencePresent || observation.Resource.Readiness != provisioning.ResourceReadinessReady || observation.Resource.Drift != provisioning.ResourceDriftInSync {
		t.Fatalf("resource observation = %#v, want present/ready/in-sync", observation.Resource)
	}
}

func TestFakeFailureAndObservationOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		mode       fake.Mode
		wantKind   provisioning.ExecutionFailureKind
		wantSubmit bool
	}{
		{name: "execution failure", mode: fake.ModeFailure, wantKind: provisioning.FailureExecution, wantSubmit: true},
		{name: "ambiguous submission", mode: fake.ModeAmbiguous, wantKind: provisioning.FailureUnknown, wantSubmit: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := fake.New(tt.mode)
			submission, err := provider.Submit(context.Background(), testRequest("operation-failure", domain.CapabilityUpdate))
			if tt.mode == fake.ModeAmbiguous {
				if !errors.Is(err, provisioning.ErrAmbiguousSubmission) {
					t.Fatalf("Submit() error = %v, want ambiguous submission", err)
				}
			} else if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if !tt.wantSubmit || submission.Observation.Execution == nil || submission.Observation.Execution.Failure == nil {
				t.Fatal("submission did not include normalized failure")
			}
			if submission.Observation.Execution.Failure.Kind != tt.wantKind {
				t.Fatalf("failure kind = %s, want %s", submission.Observation.Execution.Failure.Kind, tt.wantKind)
			}
			if tt.mode == fake.ModeFailure {
				submission.Observation.Execution.State = provisioning.ExecutionStateSucceeded
				submission.Observation.Execution.Failure.Reason = "Mutated"
				observation, observeErr := provider.Observe(context.Background(), testObservationRequest("operation-failure"))
				if observeErr != nil {
					t.Fatalf("Observe() error = %v", observeErr)
				}
				if observation.Execution == nil || observation.Execution.State != provisioning.ExecutionStateFailed {
					t.Fatalf("terminal observation = %#v, want Failed", observation.Execution)
				}
				if observation.Execution.Failure == nil || observation.Execution.Failure.Reason != "ExecutionFailed" {
					t.Fatalf("terminal failure = %#v, want original failure", observation.Execution.Failure)
				}
			}
		})
	}

	provider := fake.New(fake.ModeObservationFailure)
	if _, err := provider.Submit(context.Background(), testRequest("operation-observation-failure", domain.CapabilityCreate)); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	_, err := provider.Observe(context.Background(), testObservationRequest("operation-observation-failure"))
	var observationErr provisioning.ObservationError
	if !errors.As(err, &observationErr) || observationErr.Failure.Kind != provisioning.FailureUnavailable {
		t.Fatalf("Observe() error = %v, want normalized unavailable observation error", err)
	}
}

func TestFakeUnsupportedCapability(t *testing.T) {
	provider := fake.New(fake.ModeSynchronous)
	submission, err := provider.Submit(context.Background(), testRequest("operation-unsupported", "backup"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if submission.Observation.Execution == nil || submission.Observation.Execution.State != provisioning.ExecutionStateFailed {
		t.Fatalf("unsupported submission = %#v, want failed execution", submission.Observation.Execution)
	}
	if submission.Observation.Execution.Failure == nil || submission.Observation.Execution.Failure.Kind != provisioning.FailureUnsupported {
		t.Fatalf("unsupported failure = %#v, want Unsupported", submission.Observation.Execution.Failure)
	}
}

func TestFakeStableHandleAndIdempotentSubmission(t *testing.T) {
	provider := fake.New(fake.ModeAsynchronous)
	request := testRequest("operation-stable", domain.CapabilityCreate)

	first, err := provider.Submit(context.Background(), request)
	if err != nil {
		t.Fatalf("first Submit() error = %v", err)
	}
	second, err := provider.Submit(context.Background(), request)
	if err != nil {
		t.Fatalf("second Submit() error = %v", err)
	}
	if first.Observation.Execution == nil || second.Observation.Execution == nil {
		t.Fatal("submission did not return execution")
	}
	if first.Observation.Execution.Handle == nil || second.Observation.Execution.Handle == nil {
		t.Fatal("submission did not return a handle")
	}
	if first.Observation.Execution.Handle.String() != second.Observation.Execution.Handle.String() {
		t.Fatal("same OperationID produced different execution handles")
	}
	if provider.SubmissionCount(request.OperationID) != 1 {
		t.Fatalf("submission count = %d, want 1", provider.SubmissionCount(request.OperationID))
	}
}

func TestFakeRepeatedObservationAndDeterminism(t *testing.T) {
	request := testRequest("operation-deterministic", domain.CapabilityCreate)
	firstProvider := fake.New(fake.ModeAsynchronous)
	secondProvider := fake.New(fake.ModeAsynchronous)

	firstSubmission, err := firstProvider.Submit(context.Background(), request)
	if err != nil {
		t.Fatalf("first Submit() error = %v", err)
	}
	secondSubmission, err := secondProvider.Submit(context.Background(), request)
	if err != nil {
		t.Fatalf("second Submit() error = %v", err)
	}
	if firstSubmission.Observation.Execution.Handle.String() != secondSubmission.Observation.Execution.Handle.String() {
		t.Fatal("identical requests produced different handles")
	}

	observationRequest := testObservationRequest(request.OperationID)
	firstObservation, err := firstProvider.Observe(context.Background(), observationRequest)
	if err != nil {
		t.Fatalf("first Observe() error = %v", err)
	}
	secondObservation, err := firstProvider.Observe(context.Background(), observationRequest)
	if err != nil {
		t.Fatalf("second Observe() error = %v", err)
	}
	repeated, err := firstProvider.Observe(context.Background(), observationRequest)
	if err != nil {
		t.Fatalf("repeated Observe() error = %v", err)
	}
	if firstObservation.Execution.State != provisioning.ExecutionStateRunning || secondObservation.Execution.State != provisioning.ExecutionStateSucceeded || repeated.Execution.State != provisioning.ExecutionStateSucceeded {
		t.Fatalf("unexpected observation sequence: %s, %s, %s", firstObservation.Execution.State, secondObservation.Execution.State, repeated.Execution.State)
	}
	if !reflect.DeepEqual(secondObservation.Resource, repeated.Resource) {
		t.Fatal("repeated terminal observation was not stable")
	}
}

func TestFakeResourceNotFoundAndDrift(t *testing.T) {
	provider := fake.New(fake.ModeAsynchronous)
	notFound, err := provider.Observe(context.Background(), testObservationRequest("missing-operation"))
	if err != nil {
		t.Fatalf("NotFound Observe() error = %v", err)
	}
	if notFound.Resource.Presence != provisioning.ResourcePresenceNotFound {
		t.Fatalf("presence = %s, want NotFound", notFound.Resource.Presence)
	}

	driftProvider := fake.New(fake.ModeDrift)
	if _, err := driftProvider.Submit(context.Background(), testRequest("operation-drift", domain.CapabilityCreate)); err != nil {
		t.Fatalf("drift Submit() error = %v", err)
	}
	drift, err := driftProvider.Observe(context.Background(), testObservationRequest("operation-drift"))
	if err != nil {
		t.Fatalf("drift Observe() error = %v", err)
	}
	if drift.Resource.Drift != provisioning.ResourceDriftDrifted {
		t.Fatalf("drift = %s, want Drifted", drift.Resource.Drift)
	}
}

func testRequest(id domain.OperationID, capability domain.Capability) provisioning.ExecutionRequest {
	spec, _ := domain.NewResourceSpec(map[string]any{"intent": "test"})
	return provisioning.ExecutionRequest{
		OperationID:      id,
		ResourceID:       "resource-1",
		ResourceType:     fake.ResourceType(),
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
		ResourceType:     fake.ResourceType(),
		Spec:             spec,
		TargetGeneration: 1,
	}
}
