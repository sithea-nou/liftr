// SPDX-License-Identifier: Apache-2.0

package application_test

import (
	"context"
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

// TestLatestForResourceIsDeterministicWithEqualTimestamps pins Correction 3:
// two Operations for one Resource sharing an identical RequestedAt are
// ordered by descending Operation ID, so repeated LatestForResource calls
// always select the same Operation.
func TestLatestForResourceIsDeterministicWithEqualTimestamps(t *testing.T) {
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
		t.Fatalf("newest timestamp must win: latest=%v found=%t err=%v", latest, found, err)
	}

	var missing bool
	if _, missing, err = store.LatestForResource(context.Background(), "resource-without-operations"); err != nil || missing {
		t.Fatalf("LatestForResource for unknown Resource = found %t err %v, want false nil", missing, err)
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
