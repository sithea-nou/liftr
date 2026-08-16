// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
)

func TestNewCondition(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		typeName string
		status   domain.ConditionStatus
		reason   string
		at       time.Time
		wantErr  bool
	}{
		{name: "healthy", typeName: "Healthy", status: domain.ConditionStatusTrue, reason: "ChecksPassed", at: now},
		{name: "missing type", status: domain.ConditionStatusTrue, reason: "ChecksPassed", at: now, wantErr: true},
		{name: "invalid status", typeName: "Healthy", status: "Sometimes", reason: "ChecksPassed", at: now, wantErr: true},
		{name: "missing reason", typeName: "Healthy", status: domain.ConditionStatusTrue, at: now, wantErr: true},
		{name: "missing transition time", typeName: "Healthy", status: domain.ConditionStatusTrue, reason: "ChecksPassed", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := domain.NewCondition(tt.typeName, tt.status, tt.reason, "message", 2, tt.at)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewCondition() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestNewResourceStatus(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	healthy, _ := domain.NewCondition("Healthy", domain.ConditionStatusTrue, "ChecksPassed", "", 2, now)
	drifted, _ := domain.NewCondition("Drifted", domain.ConditionStatusFalse, "NoDrift", "", 2, now)
	newer, _ := domain.NewCondition("Healthy", domain.ConditionStatusTrue, "ChecksPassed", "", 3, now)
	future, _ := domain.NewCondition("Healthy", domain.ConditionStatusTrue, "ChecksPassed", "", 2, now.Add(time.Minute))

	tests := []struct {
		name       string
		resourceID domain.ResourceID
		generation uint64
		state      domain.ResourceState
		conditions []domain.Condition
		updatedAt  time.Time
		wantErr    bool
	}{
		{name: "normalized status", resourceID: "resource-1", generation: 2, state: domain.ResourceStateReady, conditions: []domain.Condition{healthy, drifted}, updatedAt: now},
		{name: "unknown before observation", resourceID: "resource-1", state: domain.ResourceStateUnknown, updatedAt: now},
		{name: "pending", resourceID: "resource-1", state: domain.ResourceStatePending, updatedAt: now},
		{name: "deleting", resourceID: "resource-1", generation: 2, state: domain.ResourceStateDeleting, updatedAt: now},
		{name: "deleted", resourceID: "resource-1", generation: 2, state: domain.ResourceStateDeleted, updatedAt: now},
		{name: "failed", resourceID: "resource-1", generation: 2, state: domain.ResourceStateFailed, updatedAt: now},
		{name: "missing resource ID", generation: 2, state: domain.ResourceStateReady, updatedAt: now, wantErr: true},
		{name: "invalid state", resourceID: "resource-1", generation: 2, state: "Reconciling", updatedAt: now, wantErr: true},
		{name: "duplicate condition", resourceID: "resource-1", generation: 2, state: domain.ResourceStateReady, conditions: []domain.Condition{healthy, healthy}, updatedAt: now, wantErr: true},
		{name: "condition newer than status", resourceID: "resource-1", generation: 2, state: domain.ResourceStateReady, conditions: []domain.Condition{newer}, updatedAt: now, wantErr: true},
		{name: "condition transition after status", resourceID: "resource-1", generation: 2, state: domain.ResourceStateReady, conditions: []domain.Condition{future}, updatedAt: now, wantErr: true},
		{name: "zero condition", resourceID: "resource-1", generation: 2, state: domain.ResourceStateReady, conditions: []domain.Condition{{}}, updatedAt: now, wantErr: true},
		{name: "missing update time", resourceID: "resource-1", generation: 2, state: domain.ResourceStateReady, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, err := domain.NewResourceStatus(tt.resourceID, tt.generation, tt.state, tt.conditions, tt.updatedAt)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewResourceStatus() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr || len(tt.conditions) == 0 {
				return
			}

			returned := status.Conditions()
			returned[0] = domain.Condition{}
			if status.Conditions()[0].Type() == "" {
				t.Fatal("Conditions() exposed mutable slice state")
			}
		})
	}
}
