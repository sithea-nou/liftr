// SPDX-License-Identifier: Apache-2.0

package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	appfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
)

type allowOperator struct{}

func (allowOperator) AuthorizeOperator(context.Context, identity.Principal, identity.Action, identity.OperatorTarget) error {
	return nil
}

func TestOperatorMutationReplayCreatesOneAuditAndWork(t *testing.T) {
	ctx := context.Background()
	ref := mustProvisionerRef(t, "operator-provider")
	service, store, _, _ := newService(t, ref, provisioningfake.New(provisioningfake.ModeAsynchronous))
	service.OperatorAuthorizer = allowOperator{}
	resourceID := domain.ResourceID("operator-resource")
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	resource, err := domain.NewResource(resourceID, provisioningfake.ResourceType(), domain.OwnerRef{Kind: "team", ID: "platform"}, testSpec(t), now)
	if err != nil {
		t.Fatal(err)
	}
	status, err := domain.NewResourceStatus(resourceID, 1, domain.ResourceStateReady, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		return tx.Resources().CreateResource(ctx, application.ResourceRecord{
			Resource: resource, Status: status, ProvisionerRef: ref, Version: 1,
		})
	}); err != nil {
		t.Fatal(err)
	}

	actor := appfake.Principal("operator")
	command := application.OperatorMutationCommand{Actor: actor, IdempotencyKey: "safe-observe", RequestID: "request-1"}
	first, err := service.TriggerResourcePassiveObservation(ctx, command, resourceID)
	if err != nil {
		t.Fatalf("first mutation: %v", err)
	}
	second, err := service.TriggerResourcePassiveObservation(ctx, command, resourceID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if first.Replay || !second.Replay || first.OperatorActionID != second.OperatorActionID || first.CreatedWorkID != second.CreatedWorkID {
		t.Fatalf("unexpected replay results: first=%+v second=%+v", first, second)
	}
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		work, err := tx.Outbox().SummarizeWorkByResource(ctx, resourceID)
		if err != nil {
			return err
		}
		if len(work.Active) != 1 || work.Active[0].ID != first.CreatedWorkID || work.Counts[application.OutboxPending] != 1 {
			t.Fatalf("work summary = %+v", work)
		}
		action, err := tx.OperatorActions().GetOperatorAction(ctx, first.OperatorActionID)
		if err != nil {
			return err
		}
		if action.CreatedWorkID != first.CreatedWorkID || action.SourceWorkID != "" {
			t.Fatalf("audit = %+v", action)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRejectedOperatorMutationDoesNotBindIdempotencyKey(t *testing.T) {
	ctx := context.Background()
	ref := mustProvisionerRef(t, "operator-provider")
	service, store, _, _ := newService(t, ref, provisioningfake.New(provisioningfake.ModeAsynchronous))
	service.OperatorAuthorizer = allowOperator{}
	actor := appfake.Principal("operator")
	key := "missing-target"
	_, err := service.TriggerResourcePassiveObservation(ctx, application.OperatorMutationCommand{
		Actor: actor, IdempotencyKey: key, RequestID: "request-2",
	}, "missing")
	if !errors.Is(err, application.ErrResourceNotFound) {
		t.Fatalf("error = %v, want resource not found", err)
	}
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		_, err := tx.OperatorIdempotency().GetOperatorIdempotency(ctx, string(actor.ID), key)
		if !errors.Is(err, application.ErrOperatorIdempotencyNotFound) {
			t.Fatalf("idempotency lookup error = %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
