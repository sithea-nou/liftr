// SPDX-License-Identifier: Apache-2.0

package fake_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
)

func TestExecutionOutputMappingBindsOnce(t *testing.T) {
	store := fake.NewStore()
	spec, err := domain.NewResourceSpec(map[string]any{"name": "test"})
	if err != nil {
		t.Fatal(err)
	}
	record := application.ProvisioningExecutionRecord{
		OperationID: "op", ResourceID: "resource", ResourceType: domain.ResourceTypeRef{Name: "Type", Version: "v1"},
		Capability: domain.CapabilityCreate, TargetGeneration: 1, Spec: spec, State: application.AttemptPending,
	}
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		return tx.Executions().CreateExecution(context.Background(), record)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		current, err := tx.Executions().GetExecution(context.Background(), "op")
		if err != nil {
			return err
		}
		current.OutputMappingRef = "mapping-v1"
		return tx.Executions().SaveExecution(context.Background(), current, current.Version)
	}); err != nil {
		t.Fatalf("initial mapping bind failed: %v", err)
	}
	for _, replacement := range []string{"", "mapping-v2"} {
		err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
			current, err := tx.Executions().GetExecution(context.Background(), "op")
			if err != nil {
				return err
			}
			current.OutputMappingRef = replacement
			return tx.Executions().SaveExecution(context.Background(), current, current.Version)
		})
		if !errors.Is(err, application.ErrConcurrencyConflict) {
			t.Fatalf("mapping replacement %q error = %v", replacement, err)
		}
	}
}

func TestDispatchRetryRequiresCurrentExecutionVersion(t *testing.T) {
	ctx := context.Background()
	store := fake.NewStore()
	execution := application.ProvisioningExecutionRecord{
		OperationID: "retry-op", ResourceID: "resource", ResourceType: domain.ResourceTypeRef{Name: "Type", Version: "v1"},
		Capability: domain.CapabilityCreate, TargetGeneration: 1, State: application.AttemptDispatching, Version: 1,
	}
	message := application.DispatchMessage(execution.OperationID, 1, execution.Version)
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		if err := tx.Executions().CreateExecution(ctx, execution); err != nil {
			return err
		}
		return tx.Outbox().Enqueue(ctx, message)
	}); err != nil {
		t.Fatal(err)
	}
	var claimed application.OutboxMessage
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		var found bool
		var err error
		claimed, found, err = tx.Outbox().ClaimOutbox(ctx, "token", time.Millisecond)
		if err == nil && !found {
			return errors.New("dispatch was not claimed")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		return tx.Outbox().RetryDispatchOutbox(ctx, claimed.ID, claimed.LeaseToken, execution.Version+1, 0, "mismatch")
	}); !errors.Is(err, application.ErrConcurrencyConflict) {
		t.Fatalf("active retry mismatch error=%v", err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		return tx.Outbox().RetryExpiredDispatchOutbox(ctx, claimed.ID, claimed.LeaseToken, execution.Version+1, 0, "mismatch")
	}); !errors.Is(err, application.ErrConcurrencyConflict) {
		t.Fatalf("expired retry mismatch error=%v", err)
	}
}

func TestRecoveryReferenceFreezesSourceExecutionAndAttempt(t *testing.T) {
	store := fake.NewStore()
	ctx := context.Background()
	spec, err := domain.NewResourceSpec(map[string]any{"name": "test"})
	if err != nil {
		t.Fatal(err)
	}
	source := application.ProvisioningExecutionRecord{
		OperationID: "source", ResourceID: "resource", ResourceType: domain.ResourceTypeRef{Name: "Type", Version: "v1"},
		Capability: domain.CapabilityCreate, TargetGeneration: 1, Spec: spec, State: application.AttemptPending, Version: 1,
	}
	attempt := application.SubmissionAttemptRecord{OperationID: source.OperationID, AttemptNumber: 1, State: application.SubmissionAttemptPending, DispatchMessage: "dispatch:source:1"}
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		if err := tx.Executions().CreateExecution(ctx, source); err != nil {
			return err
		}
		if err := tx.SubmissionAttempts().CreateSubmissionAttempt(ctx, attempt); err != nil {
			return err
		}
		source.CurrentAttempt = 1
		source.State = application.AttemptSucceeded
		if err := tx.Executions().SaveExecution(ctx, source, source.Version); err != nil {
			return err
		}
		source.Version++
		attempt.State = application.SubmissionAttemptAccepted
		return tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, attempt, application.SubmissionAttemptPending)
	}); err != nil {
		t.Fatalf("normal source terminalization failed: %v", err)
	}

	child := application.ProvisioningExecutionRecord{
		OperationID: "child", ResourceID: source.ResourceID, ResourceType: source.ResourceType,
		Capability: source.Capability, TargetGeneration: source.TargetGeneration, Spec: source.Spec,
		State: application.AttemptSucceeded, OutputResolution: application.OutputResolutionPending,
		RecoverySourceOperationID: source.OperationID, RecoverySourceAttempt: attempt.AttemptNumber,
	}
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		return tx.Executions().CreateExecution(ctx, child)
	}); err != nil {
		t.Fatal(err)
	}

	err = store.Within(ctx, func(tx application.UnitOfWork) error {
		current, err := tx.Executions().GetExecution(ctx, source.OperationID)
		if err != nil {
			return err
		}
		current.State = application.AttemptFailed
		return tx.Executions().SaveExecution(ctx, current, current.Version)
	})
	if !errors.Is(err, application.ErrInvalidApplicationCall) {
		t.Fatalf("referenced source execution mutation error = %v", err)
	}

	err = store.Within(ctx, func(tx application.UnitOfWork) error {
		current, err := tx.SubmissionAttempts().GetSubmissionAttempt(ctx, source.OperationID, attempt.AttemptNumber)
		if err != nil {
			return err
		}
		current.State = application.SubmissionAttemptUnknown
		return tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, current, application.SubmissionAttemptAccepted)
	})
	if !errors.Is(err, application.ErrInvalidApplicationCall) {
		t.Fatalf("referenced source attempt mutation error = %v", err)
	}
}
