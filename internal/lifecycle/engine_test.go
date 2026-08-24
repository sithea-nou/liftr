// SPDX-License-Identifier: Apache-2.0

package lifecycle_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/lifecycle"
)

var testTime = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

func TestLifecycleScenarios(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T)
	}{
		{name: "successful create", run: testSuccessfulCreate},
		{name: "successful update", run: testSuccessfulUpdate},
		{name: "successful delete", run: testSuccessfulDelete},
		{name: "failed update preserves ready", run: testFailedUpdatePreservesReady},
		{name: "failed create", run: testFailedCreate},
		{name: "failed delete preserves existence", run: testFailedDeletePreservesExistence},
		{name: "stale generation completion", run: testStaleGenerationCompletion},
		{name: "stale delete permits newer create", run: testStaleDeletePermitsNewerCreate},
		{name: "concurrent delete rejection", run: testConcurrentDeleteRejection},
		{name: "duplicate transition rejection", run: testDuplicateTransitionRejection},
		{name: "retry creates new operation", run: testRetryCreatesNewOperation},
		{name: "invalid skipped phase", run: testInvalidSkippedPhase},
		{name: "terminal operation mutation rejection", run: testTerminalOperationMutationRejection},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.run)
	}
}

func TestRequestValidation(t *testing.T) {
	resource, resourceType := newResource(t, 1)
	unknown := newStatus(t, resource.ID(), 0, domain.ResourceStateUnknown)
	ready := newStatus(t, resource.ID(), 1, domain.ResourceStateReady)

	tests := []struct {
		name       string
		resource   domain.Resource
		status     domain.ResourceStatus
		capability domain.Capability
		wantErr    error
	}{
		{name: "observe has no lifecycle flow", resource: resource, status: unknown, capability: domain.CapabilityObserve, wantErr: lifecycle.ErrInvalidTransition},
		{name: "create from ready", resource: resource, status: ready, capability: domain.CapabilityCreate, wantErr: lifecycle.ErrInvalidTransition},
		{name: "update without newer generation", resource: resource, status: ready, capability: domain.CapabilityUpdate, wantErr: lifecycle.ErrInvalidTransition},
	}

	engine := lifecycle.Engine{}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := engine.Request(tt.resource, resourceType, tt.status, nil, tt.capability, domain.OperationID(fmt.Sprintf("operation-%d", i)), domain.EventID(fmt.Sprintf("event-%d", i)), testTime.Add(time.Hour))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Request() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestOperationSnapshotConsistency(t *testing.T) {
	tests := []struct {
		name       string
		status     domain.ResourceStatus
		latest     *domain.Operation
		capability domain.Capability
		wantErr    bool
	}{
		{name: "pending resource without create operation", status: newStatusForTest("resource-1", 1, domain.ResourceStatePending), capability: domain.CapabilityCreate, wantErr: true},
		{name: "deleting resource without delete operation", status: newStatusForTest("resource-1", 1, domain.ResourceStateDeleting), capability: domain.CapabilityDelete, wantErr: true},
		{name: "ready and reconciling without update operation", status: readyReconcilingStatus("resource-1", 1), capability: domain.CapabilityUpdate, wantErr: true},
		{name: "active update with second update request", status: readyReconcilingStatus("resource-1", 1), latest: activeOperation(t, "update-active", "resource-1", domain.CapabilityUpdate, 1), capability: domain.CapabilityUpdate, wantErr: true},
		{name: "active create with second create request", status: newStatusForTest("resource-1", 1, domain.ResourceStatePending), latest: activeOperation(t, "create-active", "resource-1", domain.CapabilityCreate, 1), capability: domain.CapabilityCreate, wantErr: true},
		{name: "active delete with second delete request", status: newStatusForTest("resource-1", 1, domain.ResourceStateDeleting), latest: activeOperation(t, "delete-active", "resource-1", domain.CapabilityDelete, 1), capability: domain.CapabilityDelete, wantErr: true},
		{name: "active operation wrong capability", status: newStatusForTest("resource-1", 1, domain.ResourceStatePending), latest: activeOperation(t, "delete-wrong", "resource-1", domain.CapabilityDelete, 1), capability: domain.CapabilityCreate, wantErr: true},
		{name: "active operation wrong generation", status: newStatusForTest("resource-1", 1, domain.ResourceStatePending), latest: activeOperation(t, "create-wrong-generation", "resource-1", domain.CapabilityCreate, 2), capability: domain.CapabilityCreate, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resource, resourceType := newResource(t, 2)
			if tt.status.State() == domain.ResourceStatePending || tt.status.State() == domain.ResourceStateDeleting {
				resource, resourceType = newResource(t, 1)
			}
			beforeStatus := tt.status
			beforeGeneration := resource.Generation()
			beforeOperation := tt.latest

			result, err := (lifecycle.Engine{}).Request(resource, resourceType, tt.status, tt.latest, tt.capability, "new-operation", "new-event", testTime.Add(time.Hour))
			if (err != nil) != tt.wantErr {
				t.Fatalf("Request() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr {
				return
			}
			assertEmptyResult(t, result)
			if resource.Generation() != beforeGeneration {
				t.Fatalf("resource generation changed from %d to %d", beforeGeneration, resource.Generation())
			}
			if tt.status.State() != beforeStatus.State() || tt.status.ObservedGeneration() != beforeStatus.ObservedGeneration() || tt.status.UpdatedAt() != beforeStatus.UpdatedAt() {
				t.Fatal("resource status was mutated")
			}
			if beforeOperation != nil && (tt.latest.State() != beforeOperation.State() || tt.latest.Phase() != beforeOperation.Phase()) {
				t.Fatal("latest operation was mutated")
			}
		})
	}
}

func TestTerminalOperationAllowsNewRequest(t *testing.T) {
	resource, resourceType := newResource(t, 2)
	status := newStatus(t, resource.ID(), 1, domain.ResourceStateReady)
	terminal := terminalOperation(t, "update-complete", resource.ID(), domain.CapabilityUpdate, 1)

	result, err := (lifecycle.Engine{}).Request(resource, resourceType, status, &terminal, domain.CapabilityUpdate, "update-next", "event-next", testTime.Add(time.Hour))
	if err != nil {
		t.Fatalf("Request() error = %v", err)
	}
	if result.Operation.ID() != "update-next" {
		t.Fatalf("operation ID = %q, want update-next", result.Operation.ID())
	}
}

func TestActiveOperationRequiresActiveStatus(t *testing.T) {
	resource, _ := newResource(t, 1)
	status := newStatus(t, resource.ID(), 1, domain.ResourceStateReady)
	operation := activeOperation(t, "update-active", resource.ID(), domain.CapabilityUpdate, 1)

	result, err := (lifecycle.Engine{}).Advance(resource, status, *operation, domain.OperationPhaseValidating, "event-rejected", testTime.Add(time.Hour))
	if err == nil {
		t.Fatal("Advance() succeeded for an active operation absent from status")
	}
	assertEmptyResult(t, result)
	if operation.State() != domain.OperationStatePending || operation.Phase() != domain.OperationPhaseRequested {
		t.Fatalf("operation mutated to state=%s phase=%s", operation.State(), operation.Phase())
	}
}

func testSuccessfulCreate(t *testing.T) {
	resource, resourceType := newResource(t, 1)
	status := newStatus(t, resource.ID(), 0, domain.ResourceStateUnknown)
	engine := lifecycle.Engine{}

	result := request(t, engine, resource, resourceType, status, nil, domain.CapabilityCreate, "create-1", 1)
	result = advance(t, engine, resource, result, domain.OperationPhaseValidating, 2)
	result = advance(t, engine, resource, result, domain.OperationPhasePlanning, 3)
	result = advance(t, engine, resource, result, domain.OperationPhaseApplying, 4)
	result = complete(t, engine, resource, result, 5)

	if result.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("operation state = %s, want %s", result.Operation.State(), domain.OperationStateSucceeded)
	}
	if result.Status.State() != domain.ResourceStateReady {
		t.Fatalf("resource state = %s, want %s", result.Status.State(), domain.ResourceStateReady)
	}
	assertCondition(t, result.Status, lifecycle.ConditionReady, domain.ConditionStatusTrue, 1)
	assertCondition(t, result.Status, lifecycle.ConditionReconciled, domain.ConditionStatusTrue, 1)
}

func testSuccessfulUpdate(t *testing.T) {
	resource, resourceType := newResource(t, 2)
	status := newStatus(t, resource.ID(), 1, domain.ResourceStateReady)
	engine := lifecycle.Engine{}

	result := request(t, engine, resource, resourceType, status, nil, domain.CapabilityUpdate, "update-1", 10)
	if result.Status.ObservedGeneration() != 2 {
		t.Fatalf("observed generation = %d, want 2", result.Status.ObservedGeneration())
	}
	result = advance(t, engine, resource, result, domain.OperationPhaseValidating, 11)
	result = advance(t, engine, resource, result, domain.OperationPhasePlanning, 12)
	result = advance(t, engine, resource, result, domain.OperationPhaseApplying, 13)
	result = complete(t, engine, resource, result, 14)

	if result.Status.State() != domain.ResourceStateReady {
		t.Fatalf("resource state = %s, want %s", result.Status.State(), domain.ResourceStateReady)
	}
	assertCondition(t, result.Status, lifecycle.ConditionReady, domain.ConditionStatusTrue, 2)
	assertCondition(t, result.Status, lifecycle.ConditionReconciled, domain.ConditionStatusTrue, 2)
}

func testSuccessfulDelete(t *testing.T) {
	resource, resourceType := newResource(t, 1)
	status := newStatus(t, resource.ID(), 1, domain.ResourceStateReady)
	engine := lifecycle.Engine{}

	result := request(t, engine, resource, resourceType, status, nil, domain.CapabilityDelete, "delete-1", 10)
	result = advance(t, engine, resource, result, domain.OperationPhaseValidating, 11)
	result = advance(t, engine, resource, result, domain.OperationPhaseDestroying, 12)
	result = complete(t, engine, resource, result, 13)

	if result.Status.State() != domain.ResourceStateDeleted {
		t.Fatalf("resource state = %s, want %s", result.Status.State(), domain.ResourceStateDeleted)
	}
	assertCondition(t, result.Status, lifecycle.ConditionDeleted, domain.ConditionStatusTrue, 1)
	assertCondition(t, result.Status, lifecycle.ConditionReconciled, domain.ConditionStatusTrue, 1)
}

func testFailedUpdatePreservesReady(t *testing.T) {
	resource, resourceType := newResource(t, 2)
	status := newStatus(t, resource.ID(), 1, domain.ResourceStateReady)
	engine := lifecycle.Engine{}

	result := request(t, engine, resource, resourceType, status, nil, domain.CapabilityUpdate, "update-failed", 10)
	result = advance(t, engine, resource, result, domain.OperationPhaseValidating, 11)
	result = advance(t, engine, resource, result, domain.OperationPhasePlanning, 12)
	result = advance(t, engine, resource, result, domain.OperationPhaseApplying, 13)
	result = fail(t, engine, resource, result, "ApplyFailed", 14)

	if result.Operation.State() != domain.OperationStateFailed {
		t.Fatalf("operation state = %s, want %s", result.Operation.State(), domain.OperationStateFailed)
	}
	if result.Status.State() != domain.ResourceStateReady {
		t.Fatalf("resource state = %s, want %s", result.Status.State(), domain.ResourceStateReady)
	}
	assertCondition(t, result.Status, lifecycle.ConditionReady, domain.ConditionStatusTrue, 1)
	assertCondition(t, result.Status, lifecycle.ConditionReconciled, domain.ConditionStatusFalse, 2)
	assertCondition(t, result.Status, lifecycle.ConditionOperationFailed, domain.ConditionStatusTrue, 2)
}

func testFailedCreate(t *testing.T) {
	resource, resourceType := newResource(t, 1)
	status := newStatus(t, resource.ID(), 0, domain.ResourceStateUnknown)
	engine := lifecycle.Engine{}

	result := request(t, engine, resource, resourceType, status, nil, domain.CapabilityCreate, "create-failed", 1)
	result = advance(t, engine, resource, result, domain.OperationPhaseValidating, 2)
	result = advance(t, engine, resource, result, domain.OperationPhasePlanning, 3)
	result = advance(t, engine, resource, result, domain.OperationPhaseApplying, 4)
	result = fail(t, engine, resource, result, "ApplyFailed", 5)

	if result.Status.State() != domain.ResourceStateFailed {
		t.Fatalf("resource state = %s, want %s", result.Status.State(), domain.ResourceStateFailed)
	}
	assertCondition(t, result.Status, lifecycle.ConditionReady, domain.ConditionStatusFalse, 1)
	assertCondition(t, result.Status, lifecycle.ConditionOperationFailed, domain.ConditionStatusTrue, 1)
}

func testFailedDeletePreservesExistence(t *testing.T) {
	resource, resourceType := newResource(t, 1)
	status := newStatus(t, resource.ID(), 1, domain.ResourceStateReady)
	engine := lifecycle.Engine{}

	result := request(t, engine, resource, resourceType, status, nil, domain.CapabilityDelete, "delete-failed", 10)
	result = advance(t, engine, resource, result, domain.OperationPhaseValidating, 11)
	result = advance(t, engine, resource, result, domain.OperationPhaseDestroying, 12)
	result = fail(t, engine, resource, result, "DestroyFailed", 13)

	if result.Status.State() != domain.ResourceStateReady {
		t.Fatalf("resource state = %s, want %s", result.Status.State(), domain.ResourceStateReady)
	}
	assertCondition(t, result.Status, lifecycle.ConditionReady, domain.ConditionStatusTrue, 1)
	assertCondition(t, result.Status, lifecycle.ConditionDeleted, domain.ConditionStatusFalse, 1)
	assertCondition(t, result.Status, lifecycle.ConditionOperationFailed, domain.ConditionStatusTrue, 1)
}

func testStaleGenerationCompletion(t *testing.T) {
	resource, resourceType := newResource(t, 4)
	status := newStatus(t, resource.ID(), 3, domain.ResourceStateReady)
	engine := lifecycle.Engine{}

	result := request(t, engine, resource, resourceType, status, nil, domain.CapabilityUpdate, "update-stale", 10)
	result = advance(t, engine, resource, result, domain.OperationPhaseValidating, 11)
	result = advance(t, engine, resource, result, domain.OperationPhasePlanning, 12)
	result = advance(t, engine, resource, result, domain.OperationPhaseApplying, 13)

	spec, err := domain.NewResourceSpec(map[string]any{"revision": int64(5)})
	if err != nil {
		t.Fatalf("NewResourceSpec() error = %v", err)
	}
	if err := resource.UpdateSpec(spec, testTime.Add(14*time.Minute)); err != nil {
		t.Fatalf("UpdateSpec() error = %v", err)
	}
	result = complete(t, engine, resource, result, 15)

	if resource.Generation() != 5 {
		t.Fatalf("resource generation = %d, want 5", resource.Generation())
	}
	if result.Status.ObservedGeneration() != 4 {
		t.Fatalf("observed generation = %d, want 4", result.Status.ObservedGeneration())
	}
	assertCondition(t, result.Status, lifecycle.ConditionReconciled, domain.ConditionStatusFalse, 4)
	reconciling := assertCondition(t, result.Status, lifecycle.ConditionReconciling, domain.ConditionStatusFalse, 4)
	if reconciling.Reason() != "NewerGenerationPending" {
		t.Fatalf("Reconciling reason = %q, want NewerGenerationPending", reconciling.Reason())
	}
	assertCondition(t, result.Status, lifecycle.ConditionReconciled, domain.ConditionStatusFalse, 4)
}

func testStaleDeletePermitsNewerCreate(t *testing.T) {
	resource, resourceType := newResource(t, 1)
	status := newStatus(t, resource.ID(), 1, domain.ResourceStateReady)
	engine := lifecycle.Engine{}

	result := request(t, engine, resource, resourceType, status, nil, domain.CapabilityDelete, "delete-stale", 10)
	result = advance(t, engine, resource, result, domain.OperationPhaseValidating, 11)
	result = advance(t, engine, resource, result, domain.OperationPhaseDestroying, 12)
	spec, err := domain.NewResourceSpec(map[string]any{"revision": int64(2)})
	if err != nil {
		t.Fatalf("NewResourceSpec() error = %v", err)
	}
	if err := resource.UpdateSpec(spec, testTime.Add(13*time.Minute)); err != nil {
		t.Fatalf("UpdateSpec() error = %v", err)
	}
	result = complete(t, engine, resource, result, 14)
	if result.Status.State() != domain.ResourceStateDeleted {
		t.Fatalf("resource state = %s, want %s", result.Status.State(), domain.ResourceStateDeleted)
	}

	create := request(t, engine, resource, resourceType, result.Status, &result.Operation, domain.CapabilityCreate, "create-newer", 15)
	if create.Operation.TargetGeneration() != 2 {
		t.Fatalf("create target generation = %d, want 2", create.Operation.TargetGeneration())
	}
	if create.Status.State() != domain.ResourceStatePending {
		t.Fatalf("resource state = %s, want %s", create.Status.State(), domain.ResourceStatePending)
	}
	assertCondition(t, create.Status, lifecycle.ConditionDeleted, domain.ConditionStatusFalse, 2)
}

func testConcurrentDeleteRejection(t *testing.T) {
	resource, resourceType := newResource(t, 1)
	status := readyReconcilingStatus(resource.ID(), 1)
	active := activeOperation(t, "operation-active", resource.ID(), domain.CapabilityUpdate, 1)

	_, err := (lifecycle.Engine{}).Request(resource, resourceType, status, active, domain.CapabilityDelete, "delete-rejected", "event-rejected", testTime.Add(2*time.Minute))
	if !errors.Is(err, lifecycle.ErrOperationActive) {
		t.Fatalf("Request() error = %v, want ErrOperationActive", err)
	}
}

func testDuplicateTransitionRejection(t *testing.T) {
	resource, resourceType := newResource(t, 1)
	status := newStatus(t, resource.ID(), 0, domain.ResourceStateUnknown)
	engine := lifecycle.Engine{}
	result := request(t, engine, resource, resourceType, status, nil, domain.CapabilityCreate, "create-duplicate", 1)
	result = advance(t, engine, resource, result, domain.OperationPhaseValidating, 2)

	_, err := engine.Advance(resource, result.Status, result.Operation, domain.OperationPhaseValidating, "event-duplicate", testTime.Add(3*time.Minute))
	if !errors.Is(err, domain.ErrInvalidOperationTransition) {
		t.Fatalf("Advance() error = %v, want ErrInvalidOperationTransition", err)
	}
	if result.Operation.Phase() != domain.OperationPhaseValidating {
		t.Fatalf("original phase = %s, want %s", result.Operation.Phase(), domain.OperationPhaseValidating)
	}
}

func testRetryCreatesNewOperation(t *testing.T) {
	resource, resourceType := newResource(t, 2)
	status := newStatus(t, resource.ID(), 1, domain.ResourceStateReady)
	engine := lifecycle.Engine{}
	failed := request(t, engine, resource, resourceType, status, nil, domain.CapabilityUpdate, "update-original", 10)
	failed = advance(t, engine, resource, failed, domain.OperationPhaseValidating, 11)
	failed = fail(t, engine, resource, failed, "ValidationFailed", 12)

	spec, err := domain.NewResourceSpec(map[string]any{"revision": int64(3)})
	if err != nil {
		t.Fatalf("NewResourceSpec() error = %v", err)
	}
	if err := resource.UpdateSpec(spec, testTime.Add(13*time.Minute)); err != nil {
		t.Fatalf("UpdateSpec() error = %v", err)
	}
	retry := request(t, engine, resource, resourceType, failed.Status, &failed.Operation, domain.CapabilityUpdate, "update-retry", 14)

	if retry.Operation.ID() == failed.Operation.ID() {
		t.Fatal("retry reused the failed operation ID")
	}
	if retry.Operation.TargetGeneration() != failed.Operation.TargetGeneration() {
		t.Fatalf("retry target generation = %d, want failed generation %d", retry.Operation.TargetGeneration(), failed.Operation.TargetGeneration())
	}
	if retry.Operation.RetryOfOperationID() != failed.Operation.ID() {
		t.Fatalf("retry source = %q, want %q", retry.Operation.RetryOfOperationID(), failed.Operation.ID())
	}
	if retry.Event.Reason() != "UpdateRetryRequested" {
		t.Fatalf("retry event reason = %q, want UpdateRetryRequested", retry.Event.Reason())
	}
	if failed.Operation.State() != domain.OperationStateFailed {
		t.Fatalf("original operation state = %s, want %s", failed.Operation.State(), domain.OperationStateFailed)
	}
}

func testInvalidSkippedPhase(t *testing.T) {
	resource, resourceType := newResource(t, 1)
	status := newStatus(t, resource.ID(), 0, domain.ResourceStateUnknown)
	engine := lifecycle.Engine{}
	result := request(t, engine, resource, resourceType, status, nil, domain.CapabilityCreate, "create-skip", 1)

	_, err := engine.Advance(resource, result.Status, result.Operation, domain.OperationPhasePlanning, "event-skip", testTime.Add(2*time.Minute))
	if !errors.Is(err, domain.ErrInvalidOperationTransition) {
		t.Fatalf("Advance() error = %v, want ErrInvalidOperationTransition", err)
	}
	if result.Operation.Phase() != domain.OperationPhaseRequested || result.Operation.State() != domain.OperationStatePending {
		t.Fatalf("original operation changed to state=%s phase=%s", result.Operation.State(), result.Operation.Phase())
	}
}

func testTerminalOperationMutationRejection(t *testing.T) {
	resource, resourceType := newResource(t, 1)
	status := newStatus(t, resource.ID(), 0, domain.ResourceStateUnknown)
	engine := lifecycle.Engine{}
	result := request(t, engine, resource, resourceType, status, nil, domain.CapabilityCreate, "create-terminal", 1)
	result = advance(t, engine, resource, result, domain.OperationPhaseValidating, 2)
	result = advance(t, engine, resource, result, domain.OperationPhasePlanning, 3)
	result = advance(t, engine, resource, result, domain.OperationPhaseApplying, 4)
	result = complete(t, engine, resource, result, 5)

	_, err := engine.Complete(resource, result.Status, result.Operation, "event-terminal", testTime.Add(6*time.Minute))
	if !errors.Is(err, domain.ErrInvalidOperationTransition) {
		t.Fatalf("Complete() error = %v, want ErrInvalidOperationTransition", err)
	}
	if result.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("original operation state = %s, want %s", result.Operation.State(), domain.OperationStateSucceeded)
	}
}

func newResource(t *testing.T, generation uint64) (domain.Resource, domain.ResourceType) {
	t.Helper()
	spec, err := domain.NewResourceSpec(map[string]any{"revision": int64(1)})
	if err != nil {
		t.Fatalf("NewResourceSpec() error = %v", err)
	}
	resource, err := domain.NewResource(
		"resource-1",
		domain.ResourceTypeRef{Name: "Database", Version: "v1"},
		domain.OwnerRef{Kind: "team", ID: "payments"},
		spec,
		testTime,
	)
	if err != nil {
		t.Fatalf("NewResource() error = %v", err)
	}
	for current := uint64(2); current <= generation; current++ {
		nextSpec, specErr := domain.NewResourceSpec(map[string]any{"revision": int64(current)})
		if specErr != nil {
			t.Fatalf("NewResourceSpec() error = %v", specErr)
		}
		if updateErr := resource.UpdateSpec(nextSpec, testTime.Add(time.Duration(current-1)*time.Minute)); updateErr != nil {
			t.Fatalf("UpdateSpec() error = %v", updateErr)
		}
	}
	resourceType, err := domain.NewResourceType(
		resource.Type(),
		"A test resource",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete, domain.CapabilityObserve},
	)
	if err != nil {
		t.Fatalf("NewResourceType() error = %v", err)
	}
	return resource, resourceType
}

func newStatus(t *testing.T, resourceID domain.ResourceID, observedGeneration uint64, state domain.ResourceState) domain.ResourceStatus {
	t.Helper()
	conditions := []domain.Condition{}
	if state == domain.ResourceStateReady {
		ready, err := domain.NewCondition(lifecycle.ConditionReady, domain.ConditionStatusTrue, "PreviouslyReady", "", observedGeneration, testTime)
		if err != nil {
			t.Fatalf("NewCondition() error = %v", err)
		}
		reconciled, err := domain.NewCondition(lifecycle.ConditionReconciled, domain.ConditionStatusTrue, "PreviouslyReconciled", "", observedGeneration, testTime)
		if err != nil {
			t.Fatalf("NewCondition() error = %v", err)
		}
		conditions = append(conditions, ready, reconciled)
	}
	status, err := domain.NewResourceStatus(resourceID, observedGeneration, state, conditions, testTime)
	if err != nil {
		t.Fatalf("NewResourceStatus() error = %v", err)
	}
	return status
}

func request(t *testing.T, engine lifecycle.Engine, resource domain.Resource, resourceType domain.ResourceType, status domain.ResourceStatus, latest *domain.Operation, capability domain.Capability, operationID domain.OperationID, minute int) lifecycle.Result {
	t.Helper()
	result, err := engine.Request(resource, resourceType, status, latest, capability, operationID, domain.EventID(operationID+"-requested"), testTime.Add(time.Duration(minute)*time.Minute))
	if err != nil {
		t.Fatalf("Request() error = %v", err)
	}
	return result
}

func advance(t *testing.T, engine lifecycle.Engine, resource domain.Resource, current lifecycle.Result, phase domain.OperationPhase, minute int) lifecycle.Result {
	t.Helper()
	result, err := engine.Advance(resource, current.Status, current.Operation, phase, domain.EventID(fmt.Sprintf("%s-%s", current.Operation.ID(), phase)), testTime.Add(time.Duration(minute)*time.Minute))
	if err != nil {
		t.Fatalf("Advance(%s) error = %v", phase, err)
	}
	return result
}

func complete(t *testing.T, engine lifecycle.Engine, resource domain.Resource, current lifecycle.Result, minute int) lifecycle.Result {
	t.Helper()
	result, err := engine.Complete(resource, current.Status, current.Operation, domain.EventID(current.Operation.ID()+"-succeeded"), testTime.Add(time.Duration(minute)*time.Minute))
	if err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	return result
}

func fail(t *testing.T, engine lifecycle.Engine, resource domain.Resource, current lifecycle.Result, reason string, minute int) lifecycle.Result {
	t.Helper()
	result, err := engine.Fail(resource, current.Status, current.Operation, reason, "failure message", domain.EventID(current.Operation.ID()+"-failed"), testTime.Add(time.Duration(minute)*time.Minute))
	if err != nil {
		t.Fatalf("Fail() error = %v", err)
	}
	return result
}

func assertCondition(t *testing.T, status domain.ResourceStatus, typeName string, wantStatus domain.ConditionStatus, wantGeneration uint64) domain.Condition {
	t.Helper()
	for _, condition := range status.Conditions() {
		if condition.Type() != typeName {
			continue
		}
		if condition.Status() != wantStatus {
			t.Fatalf("condition %s status = %s, want %s", typeName, condition.Status(), wantStatus)
		}
		if condition.ObservedGeneration() != wantGeneration {
			t.Fatalf("condition %s generation = %d, want %d", typeName, condition.ObservedGeneration(), wantGeneration)
		}
		return condition
	}
	t.Fatalf("condition %s not found", typeName)
	return domain.Condition{}
}

func newStatusForTest(resourceID domain.ResourceID, observedGeneration uint64, state domain.ResourceState) domain.ResourceStatus {
	status, err := domain.NewResourceStatus(resourceID, observedGeneration, state, nil, testTime)
	if err != nil {
		panic(err)
	}
	return status
}

func readyReconcilingStatus(resourceID domain.ResourceID, observedGeneration uint64) domain.ResourceStatus {
	reconciling, err := domain.NewCondition(lifecycle.ConditionReconciling, domain.ConditionStatusTrue, "UpdateRequested", "", observedGeneration, testTime)
	if err != nil {
		panic(err)
	}
	ready, err := domain.NewCondition(lifecycle.ConditionReady, domain.ConditionStatusTrue, "PreviouslyReady", "", observedGeneration, testTime)
	if err != nil {
		panic(err)
	}
	status, err := domain.NewResourceStatus(resourceID, observedGeneration, domain.ResourceStateReady, []domain.Condition{ready, reconciling}, testTime)
	if err != nil {
		panic(err)
	}
	return status
}

func activeOperation(t *testing.T, id domain.OperationID, resourceID domain.ResourceID, capability domain.Capability, generation uint64) *domain.Operation {
	t.Helper()
	operation, err := domain.NewOperation(id, resourceID, capability, generation, testTime)
	if err != nil {
		t.Fatalf("NewOperation() error = %v", err)
	}
	return &operation
}

func terminalOperation(t *testing.T, id domain.OperationID, resourceID domain.ResourceID, capability domain.Capability, generation uint64) domain.Operation {
	t.Helper()
	operation := activeOperation(t, id, resourceID, capability, generation)
	if err := operation.Start(testTime.Add(time.Minute)); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := operation.AdvancePhase(domain.OperationPhasePlanning, testTime.Add(2*time.Minute)); err != nil {
		t.Fatalf("AdvancePhase() error = %v", err)
	}
	if err := operation.AdvancePhase(domain.OperationPhaseApplying, testTime.Add(3*time.Minute)); err != nil {
		t.Fatalf("AdvancePhase() error = %v", err)
	}
	if err := operation.Succeed(testTime.Add(4 * time.Minute)); err != nil {
		t.Fatalf("Succeed() error = %v", err)
	}
	return *operation
}

func assertEmptyResult(t *testing.T, result lifecycle.Result) {
	t.Helper()
	if result.Operation.ID() != "" || result.Status.ResourceID() != "" || result.Event.ID() != "" {
		t.Fatalf("rejected request returned non-empty result: operation=%q status=%q event=%q", result.Operation.ID(), result.Status.ResourceID(), result.Event.ID())
	}
}
