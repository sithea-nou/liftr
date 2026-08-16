// SPDX-License-Identifier: Apache-2.0

package lifecycle_test

import (
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/lifecycle"
)

func TestApplyObservation(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	resource, err := domain.NewResource(
		"resource-1",
		domain.ResourceTypeRef{Name: "Test", Version: "v1"},
		domain.OwnerRef{Kind: "team", ID: "platform"},
		mustSpec(t),
		now,
	)
	if err != nil {
		t.Fatalf("NewResource() error = %v", err)
	}

	tests := []struct {
		name      string
		facts     domain.ObservedFacts
		wantState domain.ResourceState
		wantReady domain.ConditionStatus
		wantDrift domain.ConditionStatus
	}{
		{name: "present ready in sync", facts: domain.ObservedFacts{Presence: domain.ResourcePresencePresent, Readiness: domain.ResourceReadinessReady, Drift: domain.ResourceDriftInSync}, wantState: domain.ResourceStateReady, wantReady: domain.ConditionStatusTrue, wantDrift: domain.ConditionStatusFalse},
		{name: "present not ready", facts: domain.ObservedFacts{Presence: domain.ResourcePresencePresent, Readiness: domain.ResourceReadinessNotReady, Drift: domain.ResourceDriftInSync}, wantState: domain.ResourceStateUnknown, wantReady: domain.ConditionStatusFalse, wantDrift: domain.ConditionStatusFalse},
		{name: "not found", facts: domain.ObservedFacts{Presence: domain.ResourcePresenceNotFound, Readiness: domain.ResourceReadinessUnknown, Drift: domain.ResourceDriftUnknown}, wantState: domain.ResourceStateUnknown, wantReady: domain.ConditionStatusFalse, wantDrift: domain.ConditionStatusUnknown},
		{name: "drifted does not fail or remediate", facts: domain.ObservedFacts{Presence: domain.ResourcePresencePresent, Readiness: domain.ResourceReadinessReady, Drift: domain.ResourceDriftDrifted}, wantState: domain.ResourceStateReady, wantReady: domain.ConditionStatusTrue, wantDrift: domain.ConditionStatusTrue},
		{name: "unknown facts", facts: domain.ObservedFacts{Presence: domain.ResourcePresenceUnknown, Readiness: domain.ResourceReadinessUnknown, Drift: domain.ResourceDriftUnknown}, wantState: domain.ResourceStateUnknown, wantReady: domain.ConditionStatusUnknown, wantDrift: domain.ConditionStatusUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, statusErr := domain.NewResourceStatus(resource.ID(), 1, domain.ResourceStateUnknown, nil, now)
			if statusErr != nil {
				t.Fatalf("NewResourceStatus() error = %v", statusErr)
			}
			result, applyErr := (lifecycle.Engine{}).ApplyObservation(resource, status, tt.facts, now.Add(time.Minute))
			if applyErr != nil {
				t.Fatalf("ApplyObservation() error = %v", applyErr)
			}
			if result.State() != tt.wantState {
				t.Fatalf("state = %s, want %s", result.State(), tt.wantState)
			}
			assertConditionStatus(t, result, lifecycle.ConditionReady, tt.wantReady)
			assertConditionStatus(t, result, "Drifted", tt.wantDrift)
		})
	}
}

func TestApplyObservationDoesNotCreateOperation(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	resource, err := domain.NewResource("resource-1", domain.ResourceTypeRef{Name: "Test", Version: "v1"}, domain.OwnerRef{Kind: "team", ID: "platform"}, mustSpec(t), now)
	if err != nil {
		t.Fatalf("NewResource() error = %v", err)
	}
	status, err := domain.NewResourceStatus(resource.ID(), 1, domain.ResourceStateUnknown, nil, now)
	if err != nil {
		t.Fatalf("NewResourceStatus() error = %v", err)
	}
	result, err := (lifecycle.Engine{}).ApplyObservation(resource, status, domain.ObservedFacts{Presence: domain.ResourcePresencePresent, Readiness: domain.ResourceReadinessReady, Drift: domain.ResourceDriftInSync}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ApplyObservation() error = %v", err)
	}
	if result.ObservedGeneration() != status.ObservedGeneration() {
		t.Fatalf("observed generation = %d, want %d", result.ObservedGeneration(), status.ObservedGeneration())
	}
	for _, condition := range result.Conditions() {
		if condition.Type() == lifecycle.ConditionReconciled && condition.Status() == domain.ConditionStatusTrue {
			t.Fatal("passive observation implied reconciliation success")
		}
	}
}

func TestApplyObservationRejectsActiveOperationStatus(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	resource, err := domain.NewResource("resource-1", domain.ResourceTypeRef{Name: "Test", Version: "v1"}, domain.OwnerRef{Kind: "team", ID: "platform"}, mustSpec(t), now)
	if err != nil {
		t.Fatalf("NewResource() error = %v", err)
	}
	reconciling, err := domain.NewCondition(lifecycle.ConditionReconciling, domain.ConditionStatusTrue, "UpdateRequested", "", 1, now)
	if err != nil {
		t.Fatalf("NewCondition() error = %v", err)
	}
	status, err := domain.NewResourceStatus(resource.ID(), 1, domain.ResourceStateReady, []domain.Condition{reconciling}, now)
	if err != nil {
		t.Fatalf("NewResourceStatus() error = %v", err)
	}
	if _, err := (lifecycle.Engine{}).ApplyObservation(resource, status, domain.ObservedFacts{Presence: domain.ResourcePresencePresent, Readiness: domain.ResourceReadinessReady, Drift: domain.ResourceDriftInSync}, now.Add(time.Minute)); err == nil {
		t.Fatal("ApplyObservation() accepted an active operation status")
	}
}

func TestApplyObservationRejectsInvalidFacts(t *testing.T) {
	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	resource, err := domain.NewResource("resource-invalid-facts", domain.ResourceTypeRef{Name: "Test", Version: "v1"}, domain.OwnerRef{Kind: "team", ID: "platform"}, mustSpec(t), now)
	if err != nil {
		t.Fatalf("NewResource() error = %v", err)
	}
	status, err := domain.NewResourceStatus(resource.ID(), 1, domain.ResourceStateUnknown, nil, now)
	if err != nil {
		t.Fatalf("NewResourceStatus() error = %v", err)
	}
	invalid := domain.ObservedFacts{Presence: domain.ResourcePresenceUnknown, Readiness: domain.ResourceReadinessReady, Drift: domain.ResourceDriftUnknown}
	if _, err := (lifecycle.Engine{}).ApplyObservation(resource, status, invalid, now.Add(time.Minute)); err == nil {
		t.Fatal("ApplyObservation() accepted contradictory presence/readiness")
	}
}

func assertConditionStatus(t *testing.T, status domain.ResourceStatus, typeName string, want domain.ConditionStatus) {
	t.Helper()
	for _, condition := range status.Conditions() {
		if condition.Type() == typeName {
			if condition.Status() != want {
				t.Fatalf("condition %s = %s, want %s", typeName, condition.Status(), want)
			}
			return
		}
	}
	t.Fatalf("condition %s missing", typeName)
}

func mustSpec(t *testing.T) domain.ResourceSpec {
	t.Helper()
	spec, err := domain.NewResourceSpec(map[string]any{"intent": "test"})
	if err != nil {
		t.Fatalf("NewResourceSpec() error = %v", err)
	}
	return spec
}
