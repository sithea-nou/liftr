// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
)

func TestNewOperation(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		id         domain.OperationID
		resourceID domain.ResourceID
		capability domain.Capability
		generation uint64
		at         time.Time
		wantErr    bool
	}{
		{name: "valid", id: "operation-1", resourceID: "resource-1", capability: domain.CapabilityCreate, generation: 3, at: now},
		{name: "missing ID", resourceID: "resource-1", capability: domain.CapabilityCreate, generation: 3, at: now, wantErr: true},
		{name: "missing resource ID", id: "operation-1", capability: domain.CapabilityCreate, generation: 3, at: now, wantErr: true},
		{name: "missing capability", id: "operation-1", resourceID: "resource-1", generation: 3, at: now, wantErr: true},
		{name: "missing target generation", id: "operation-1", resourceID: "resource-1", capability: domain.CapabilityCreate, at: now, wantErr: true},
		{name: "missing request time", id: "operation-1", resourceID: "resource-1", capability: domain.CapabilityCreate, generation: 3, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			operation, err := domain.NewOperation(tt.id, tt.resourceID, tt.capability, tt.generation, tt.at)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewOperation() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if operation.State() != domain.OperationStatePending {
				t.Fatalf("State() = %s, want %s", operation.State(), domain.OperationStatePending)
			}
			if operation.TargetGeneration() != tt.generation {
				t.Fatalf("TargetGeneration() = %d, want %d", operation.TargetGeneration(), tt.generation)
			}
		})
	}
}

func TestOperationTransitions(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		transition  func(*domain.Operation) error
		wantState   domain.OperationState
		wantErr     bool
		wantInvalid bool
		wantFailure bool
	}{
		{name: "start", transition: func(o *domain.Operation) error { return o.Start(now.Add(time.Minute)) }, wantState: domain.OperationStateRunning},
		{name: "succeed after start", transition: func(o *domain.Operation) error {
			_ = o.Start(now.Add(time.Minute))
			return o.Succeed(now.Add(2 * time.Minute))
		}, wantState: domain.OperationStateSucceeded},
		{name: "fail while pending", transition: func(o *domain.Operation) error {
			return o.Fail("DispatchFailed", "not dispatched", now.Add(time.Minute))
		}, wantState: domain.OperationStateFailed, wantFailure: true},
		{name: "fail while running", transition: func(o *domain.Operation) error {
			_ = o.Start(now.Add(time.Minute))
			return o.Fail("ExecutionFailed", "failed", now.Add(2*time.Minute))
		}, wantState: domain.OperationStateFailed, wantFailure: true},
		{name: "cancel while pending", transition: func(o *domain.Operation) error { return o.Cancel(now.Add(time.Minute)) }, wantState: domain.OperationStateCanceled},
		{name: "cancel while running", transition: func(o *domain.Operation) error {
			_ = o.Start(now.Add(time.Minute))
			return o.Cancel(now.Add(2 * time.Minute))
		}, wantState: domain.OperationStateCanceled},
		{name: "succeed while pending", transition: func(o *domain.Operation) error { return o.Succeed(now.Add(time.Minute)) }, wantState: domain.OperationStatePending, wantErr: true, wantInvalid: true},
		{name: "start twice", transition: func(o *domain.Operation) error {
			_ = o.Start(now.Add(time.Minute))
			return o.Start(now.Add(2 * time.Minute))
		}, wantState: domain.OperationStateRunning, wantErr: true, wantInvalid: true},
		{name: "transition after terminal", transition: func(o *domain.Operation) error {
			_ = o.Cancel(now.Add(time.Minute))
			return o.Fail("LateFailure", "", now.Add(2*time.Minute))
		}, wantState: domain.OperationStateCanceled, wantErr: true, wantInvalid: true},
		{name: "transition before request", transition: func(o *domain.Operation) error { return o.Start(now.Add(-time.Minute)) }, wantState: domain.OperationStatePending, wantErr: true},
		{name: "missing transition time", transition: func(o *domain.Operation) error { return o.Start(time.Time{}) }, wantState: domain.OperationStatePending, wantErr: true},
		{name: "completion before start", transition: func(o *domain.Operation) error {
			_ = o.Start(now.Add(2 * time.Minute))
			return o.Succeed(now.Add(time.Minute))
		}, wantState: domain.OperationStateRunning, wantErr: true},
		{name: "missing failure reason", transition: func(o *domain.Operation) error { return o.Fail("", "failed", now.Add(time.Minute)) }, wantState: domain.OperationStatePending, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			operation, err := domain.NewOperation("operation-1", "resource-1", domain.CapabilityCreate, 3, now)
			if err != nil {
				t.Fatalf("NewOperation() error = %v", err)
			}

			err = tt.transition(&operation)
			if (err != nil) != tt.wantErr {
				t.Fatalf("transition error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantInvalid && !errors.Is(err, domain.ErrInvalidOperationTransition) {
				t.Fatalf("transition error = %v, want ErrInvalidOperationTransition", err)
			}
			if operation.State() != tt.wantState {
				t.Fatalf("State() = %s, want %s", operation.State(), tt.wantState)
			}
			failure, hasFailure := operation.Failure()
			if hasFailure != tt.wantFailure {
				t.Fatalf("Failure() present = %v, want %v", hasFailure, tt.wantFailure)
			}
			if tt.wantFailure && failure.Reason() == "" {
				t.Fatal("Failure().Reason() is empty")
			}
		})
	}
}
