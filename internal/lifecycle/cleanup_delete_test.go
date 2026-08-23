// SPDX-License-Identifier: Apache-2.0

package lifecycle_test

import (
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/lifecycle"
)

// cleanupFixture builds a Failed Resource that owns backend state: a create
// whose execution succeeded but whose required outputs were rejected.
func cleanupFixture(t *testing.T) (domain.Resource, domain.ResourceType, domain.ResourceStatus, *domain.Operation, time.Time) {
	t.Helper()
	at := time.Date(2026, 8, 23, 8, 0, 0, 0, time.UTC)
	resourceType, err := domain.NewResourceType(
		domain.ResourceTypeRef{Name: "Widget", Version: "v1"},
		"Test widget contract.",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete},
	)
	if err != nil {
		t.Fatal(err)
	}
	resource, err := domain.NewResource("orders-db", resourceType.Ref(),
		domain.OwnerRef{Kind: "team", ID: "platform"},
		mustCleanupSpec(t), at)
	if err != nil {
		t.Fatal(err)
	}
	status, err := domain.NewResourceStatus(resource.ID(), 0, domain.ResourceStateUnknown, nil, at)
	if err != nil {
		t.Fatal(err)
	}
	engine := lifecycle.Engine{}
	createID := domain.OperationID("op-create")
	requested, err := engine.Request(resource, resourceType, status, nil, domain.CapabilityCreate, createID,
		domain.EventID("evt-req"), at)
	if err != nil {
		t.Fatal(err)
	}
	runningAt := at.Add(time.Second)
	operation := requested.Operation
	if err := operation.Start(runningAt); err != nil {
		t.Fatal(err)
	}
	if err := operation.AdvancePhase(domain.OperationPhasePlanning, runningAt); err != nil {
		t.Fatal(err)
	}
	if err := operation.AdvancePhase(domain.OperationPhaseApplying, runningAt); err != nil {
		t.Fatal(err)
	}
	failed, err := engine.Fail(resource, requested.Status, operation, "OutputPostconditionRejected",
		"required output field port is missing", domain.EventID("evt-fail"), at.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return resource, resourceType, failed.Status, &failed.Operation, at
}

func mustCleanupSpec(t *testing.T) domain.ResourceSpec {
	t.Helper()
	spec, err := domain.NewResourceSpec(map[string]any{"size": int64(5)})
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// TestDeleteAdmissibleFromFailed pins the M10 precondition change.
func TestDeleteAdmissibleFromFailed(t *testing.T) {
	resource, resourceType, status, latest, at := cleanupFixture(t)
	engine := lifecycle.Engine{}
	result, err := engine.Request(resource, resourceType, status, latest, domain.CapabilityDelete,
		domain.OperationID("op-cleanup"), domain.EventID("evt-cleanup"), at.Add(time.Minute))
	if err != nil {
		t.Fatalf("cleanup delete rejected: %v", err)
	}
	if result.Status.State() != domain.ResourceStateDeleting {
		t.Fatalf("state = %s", result.Status.State())
	}
	if result.Status.ObservedGeneration() != 1 {
		t.Fatalf("observed generation = %d", result.Status.ObservedGeneration())
	}
}

func TestFailedCleanupDeleteStaysFailed(t *testing.T) {
	resource, resourceType, status, latest, at := cleanupFixture(t)
	engine := lifecycle.Engine{}
	requested, err := engine.Request(resource, resourceType, status, latest, domain.CapabilityDelete,
		domain.OperationID("op-cleanup"), domain.EventID("evt-cleanup"), at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	operation := requested.Operation
	runningAt := at.Add(time.Minute + time.Second)
	if err := operation.Start(runningAt); err != nil {
		t.Fatal(err)
	}
	if err := operation.AdvancePhase(domain.OperationPhaseDestroying, runningAt); err != nil {
		t.Fatal(err)
	}
	result, err := engine.Fail(resource, requested.Status, operation, "DestroyFailed", "destroy did not complete",
		domain.EventID("evt-fail"), runningAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status.State() != domain.ResourceStateFailed {
		t.Fatalf("state = %s, want Failed: the Resource was never usable and the backend may remain", result.Status.State())
	}
	deletedCondition := findCondition(result.Status.Conditions(), "Deleted")
	if deletedCondition == nil || deletedCondition.Status() != domain.ConditionStatusFalse {
		t.Fatalf("Deleted condition = %+v", deletedCondition)
	}
}

func TestSuccessfulCleanupDeleteBecomesDeleted(t *testing.T) {
	resource, resourceType, status, latest, at := cleanupFixture(t)
	engine := lifecycle.Engine{}
	requested, err := engine.Request(resource, resourceType, status, latest, domain.CapabilityDelete,
		domain.OperationID("op-cleanup"), domain.EventID("evt-cleanup"), at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	operation := requested.Operation
	runningAt := at.Add(time.Minute + time.Second)
	if err := operation.Start(runningAt); err != nil {
		t.Fatal(err)
	}
	if err := operation.AdvancePhase(domain.OperationPhaseDestroying, runningAt); err != nil {
		t.Fatal(err)
	}
	result, err := engine.Complete(resource, requested.Status, operation, domain.EventID("evt-done"), runningAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status.State() != domain.ResourceStateDeleted {
		t.Fatalf("state = %s, want Deleted after successful destruction", result.Status.State())
	}
}

func TestFailedReadyDeleteStillRestoresReady(t *testing.T) {
	at := time.Date(2026, 8, 23, 8, 0, 0, 0, time.UTC)
	resourceType, _ := domain.NewResourceType(
		domain.ResourceTypeRef{Name: "Widget", Version: "v1"}, "w",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	resource, _ := domain.NewResource("r", resourceType.Ref(), domain.OwnerRef{Kind: "team", ID: "p"}, mustCleanupSpec(t), at)
	status, err := domain.NewResourceStatus(resource.ID(), 1, domain.ResourceStateDeleting,
		[]domain.Condition{
			mustCleanupCondition(t, "Ready", domain.ConditionStatusTrue, "LifecycleSucceeded", 1, at),
			mustCleanupCondition(t, "Reconciling", domain.ConditionStatusTrue, "DeleteRequested", 1, at.Add(time.Minute)),
			mustCleanupCondition(t, "Deleted", domain.ConditionStatusFalse, "DeletePending", 1, at.Add(time.Minute)),
		}, at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	operation, _ := domain.NewOperation("op-del", resource.ID(), domain.CapabilityDelete, 1, at.Add(time.Minute))
	_ = operation.Start(at.Add(time.Minute))
	_ = operation.AdvancePhase(domain.OperationPhaseDestroying, at.Add(time.Minute))
	engine := lifecycle.Engine{}
	result, err := engine.Fail(resource, status, operation, "DestroyFailed", "boom", domain.EventID("e"), at.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status.State() != domain.ResourceStateReady {
		t.Fatalf("previously Ready Resource restored to %s", result.Status.State())
	}
}

func TestAmbiguousDestroyNeverCompletesThroughRequest(t *testing.T) {
	// The engine has no path from an active delete to Deleted except Complete;
	// ambiguity is represented by the operation staying Running with Unknown
	// evidence. Pin that a Running delete cannot be completed without passing
	// through correlated success — Complete requires the final phase, which is
	// only reached via explicit destroy advancement, so any false completion
	// attempt fails loudly.
	resource, resourceType, status, latest, at := cleanupFixture(t)
	engine := lifecycle.Engine{}
	requested, err := engine.Request(resource, resourceType, status, latest, domain.CapabilityDelete,
		domain.OperationID("op-cleanup"), domain.EventID("evt-cleanup"), at.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	operation := requested.Operation
	if err := operation.Start(at.Add(time.Minute + time.Second)); err != nil {
		t.Fatal(err)
	}
	// Still in Validating: a destroy whose outcome is ambiguous never leaves
	// this phase without correlated terminal evidence.
	if _, err := engine.Complete(resource, requested.Status, operation, domain.EventID("evt-x"), at.Add(2*time.Minute)); err == nil {
		t.Fatal("completed a delete from a non-final phase")
	}
}

func findCondition(conditions []domain.Condition, typeName string) *domain.Condition {
	for i := range conditions {
		if conditions[i].Type() == typeName {
			return &conditions[i]
		}
	}
	return nil
}

func mustCleanupCondition(t *testing.T, typeName string, status domain.ConditionStatus, reason string, generation uint64, at time.Time) domain.Condition {
	t.Helper()
	condition, err := domain.NewCondition(typeName, status, reason, "", generation, at)
	if err != nil {
		t.Fatal(err)
	}
	return condition
}
