// SPDX-License-Identifier: Apache-2.0

package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
)

func readTestOperation(t *testing.T, id domain.OperationID, resourceID domain.ResourceID, capability domain.Capability, requestedAt time.Time) domain.Operation {
	t.Helper()
	operation, err := domain.NewOperation(id, resourceID, capability, 1, requestedAt)
	if err != nil {
		t.Fatal(err)
	}
	return operation
}

// seedOperations stores Operations directly so tests can pin repository
// ordering semantics without driving lifecycle flows.
func seedOperations(t *testing.T, store *fake.Store, operations ...domain.Operation) {
	t.Helper()
	err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		for _, operation := range operations {
			if err := tx.Operations().CreateOperation(context.Background(), application.OperationRecord{Operation: operation, Version: 1}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOperationHistoryUsesInsertionSequence(t *testing.T) {
	store := fake.NewStore()
	requestedAt := applicationTime
	resourceID := domain.ResourceID("resource-latest")
	olderID := domain.OperationID("op-a-older")
	newerID := domain.OperationID("op-b-newer")

	seedOperations(t, store,
		readTestOperation(t, olderID, resourceID, domain.CapabilityCreate, requestedAt),
		readTestOperation(t, newerID, resourceID, domain.CapabilityDelete, requestedAt),
	)

	for attempt := 0; attempt < 25; attempt++ {
		latest, found, err := store.LatestForResource(context.Background(), resourceID)
		if err != nil || !found {
			t.Fatalf("attempt %d: latest found=%t err=%v", attempt, found, err)
		}
		if latest.Operation.ID() != newerID {
			t.Fatalf("attempt %d: equal RequestedAt selected %q, want deterministic %q", attempt, latest.Operation.ID(), newerID)
		}
	}

	newestID := domain.OperationID("op-c-newer-time")
	seedOperations(t, store,
		readTestOperation(t, newestID, resourceID, domain.CapabilityUpdate, requestedAt.Add(time.Second)),
	)
	latest, found, err := store.LatestForResource(context.Background(), resourceID)
	if err != nil || !found || latest.Operation.ID() != newestID {
		t.Fatalf("newest insertion must win: latest=%v found=%t err=%v", latest, found, err)
	}
	regressedClockID := domain.OperationID("op-d-regressed-clock")
	seedOperations(t, store,
		readTestOperation(t, regressedClockID, resourceID, domain.CapabilityDelete, requestedAt.Add(-time.Second)),
	)
	latest, found, err = store.LatestForResource(context.Background(), resourceID)
	if err != nil || !found || latest.Operation.ID() != regressedClockID {
		t.Fatalf("clock regression changed insertion order: latest=%v found=%t err=%v", latest, found, err)
	}

	var missing bool
	if _, missing, err = store.LatestForResource(context.Background(), "resource-without-operations"); err != nil || missing {
		t.Fatalf("LatestForResource for unknown Resource = found %t err %v, want false nil", missing, err)
	}

	firstPage, err := store.PageForResource(context.Background(), resourceID, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstPage.Records) != 2 || firstPage.Records[0].Operation.ID() != regressedClockID || firstPage.Records[1].Operation.ID() != newestID || firstPage.NextSequence == 0 {
		t.Fatalf("first page = %#v", firstPage)
	}
	secondPage, err := store.PageForResource(context.Background(), resourceID, firstPage.NextSequence, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(secondPage.Records) != 2 || secondPage.Records[0].Operation.ID() != newerID || secondPage.Records[1].Operation.ID() != olderID || secondPage.NextSequence != 0 {
		t.Fatalf("second page = %#v", secondPage)
	}
	if _, err := store.PageForResource(context.Background(), resourceID, 0, 0); err == nil {
		t.Fatal("PageForResource accepted a zero limit")
	}
}

func TestFakeOperationRetryValidationAndTerminalImmutability(t *testing.T) {
	store := fake.NewStore()
	failed := readTestOperation(t, "operation-failed", "resource-retry-history", domain.CapabilityUpdate, applicationTime)
	if err := failed.Fail("ApplyFailed", "failed", applicationTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateOperation(context.Background(), application.OperationRecord{Operation: failed, Version: 1}); err != nil {
		t.Fatal(err)
	}

	retry, err := domain.NewOperation("operation-retry", failed.ResourceID(), failed.Capability(), failed.TargetGeneration(), applicationTime.Add(time.Minute), failed.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateOperation(context.Background(), application.OperationRecord{Operation: retry, Version: 1}); err != nil {
		t.Fatalf("valid retry rejected: %v", err)
	}
	retryRecord, err := store.GetOperation(context.Background(), retry.ID())
	if err != nil || retryRecord.Sequence == 0 {
		t.Fatalf("stored retry sequence=%d err=%v", retryRecord.Sequence, err)
	}

	wrongIntent, err := domain.NewOperation("operation-wrong-intent", failed.ResourceID(), failed.Capability(), failed.TargetGeneration()+1, applicationTime.Add(2*time.Minute), failed.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateOperation(context.Background(), application.OperationRecord{Operation: wrongIntent, Version: 1}); !errors.Is(err, application.ErrInvalidApplicationCall) {
		t.Fatalf("mismatched retry error = %v, want ErrInvalidApplicationCall", err)
	}
	if err := store.SaveOperation(context.Background(), application.OperationRecord{Operation: failed}, 1); !errors.Is(err, application.ErrInvalidApplicationCall) {
		t.Fatalf("terminal save error = %v, want ErrInvalidApplicationCall", err)
	}
}

func TestFakeOperationSequenceSurvivesRollback(t *testing.T) {
	store := fake.NewStore()
	rolledBack := errors.New("roll back")
	err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		operation := readTestOperation(t, "operation-rolled-back", "resource-sequence", domain.CapabilityCreate, applicationTime)
		if err := tx.Operations().CreateOperation(context.Background(), application.OperationRecord{Operation: operation}); err != nil {
			return err
		}
		return rolledBack
	})
	if !errors.Is(err, rolledBack) {
		t.Fatalf("transaction error = %v, want rollback sentinel", err)
	}
	operation := readTestOperation(t, "operation-after-rollback", "resource-sequence", domain.CapabilityCreate, applicationTime.Add(time.Minute))
	if err := store.CreateOperation(context.Background(), application.OperationRecord{Operation: operation}); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetOperation(context.Background(), operation.ID())
	if err != nil || record.Sequence != 2 {
		t.Fatalf("sequence after rollback = %d, err=%v, want 2", record.Sequence, err)
	}
}

// TestReadUseCasesReturnStoredState covers the minimal approved read paths
// against the fake repository.
func TestReadUseCasesReturnStoredState(t *testing.T) {
	service, _, _, _ := newService(t, mustProvisionerRef(t, "read-provider"), provisioningfake.New(provisioningfake.ModeSynchronous))
	ctx := context.Background()

	if _, err := service.GetResource(ctx, fake.Principal("tester"), "missing"); err == nil {
		t.Fatal("GetResource for a missing Resource must fail")
	}
	if _, err := service.GetOperation(ctx, fake.Principal("tester"), "missing"); err == nil {
		t.Fatal("GetOperation for a missing Operation must fail")
	}
	if _, err := service.GetResourceOperation(ctx, fake.Principal("tester"), "missing"); err == nil {
		t.Fatal("GetResourceOperation for a missing Resource must fail")
	}

	command := application.CreateResourceCommand{Actor: fake.Principal("tester"),
		ID: "resource-read", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-read", EventID: "event-read", RequestedAt: applicationTime,
		IdempotencyKey: "read-key",
	}
	admitted, err := service.AdmitCreateResource(ctx, command)
	if err != nil {
		t.Fatal(err)
	}

	record, err := service.GetResource(ctx, fake.Principal("tester"), command.ID)
	if err != nil || record.Resource.ID() != command.ID {
		t.Fatalf("GetResource = %v, %v", record, err)
	}

	view, err := service.GetResourceOperation(ctx, fake.Principal("tester"), command.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Latest == nil || view.Latest.ID() != admitted.Operation.ID() {
		t.Fatalf("GetResourceOperation latest = %v, want %q", view.Latest, admitted.Operation.ID())
	}

	operation, err := service.GetOperation(ctx, fake.Principal("tester"), admitted.Operation.ID())
	if err != nil || operation.Operation.ID() != admitted.Operation.ID() {
		t.Fatalf("GetOperation = %v, %v", operation, err)
	}
}
