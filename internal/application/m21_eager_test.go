// SPDX-License-Identifier: Apache-2.0

package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/lifecycle"
	"github.com/sithea-nou/liftr/internal/provisioning"

	appfake "github.com/sithea-nou/liftr/internal/application/fake"
)

// countingProvider submits synchronously with valid ready facts and counts
// submissions per Operation, so eager-path tests can pin Submit counts.
type countingProvider struct {
	submissions map[domain.OperationID]int
}

func newCountingProvider() *countingProvider {
	return &countingProvider{submissions: map[domain.OperationID]int{}}
}

func (p *countingProvider) Capabilities() []provisioning.ProvisionerCapability { return nil }

func (p *countingProvider) Submit(_ context.Context, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	p.submissions[request.OperationID]++
	handle, err := provisioning.NewExecutionHandle("eager-" + string(request.OperationID))
	if err != nil {
		return provisioning.Submission{}, err
	}
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle},
		Resource: provisioning.ResourceObservation{
			Presence:  provisioning.ResourcePresencePresent,
			Readiness: provisioning.ResourceReadinessReady,
			Drift:     provisioning.ResourceDriftInSync,
		},
	}}, nil
}

func (p *countingProvider) Observe(_ context.Context, request provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationUnknown}, nil
}

func (p *countingProvider) count(id domain.OperationID) int { return p.submissions[id] }

type eagerFixture struct {
	service   *application.Service
	store     *appfake.Store
	catalog   *strictCatalog
	provider  *countingProvider
	eagerOn   bool
	nextClock int
}

func newEagerFixture(t *testing.T, slots ...int) *eagerFixture {
	t.Helper()
	f := &eagerFixture{store: appfake.NewStore(), provider: newCountingProvider()}
	ref, err := application.NewProvisionerRef("eager-provider")
	if err != nil {
		t.Fatal(err)
	}
	contract := newStrictContract(m21Type)
	if len(slots) > 0 {
		contract.WithReferences(m21Slot("dependency", 0, 1))
	}
	f.catalog = &strictCatalog{
		types: map[domain.ResourceTypeRef]*strictContract{contract.Ref(): contract},
		order: []domain.ResourceTypeRef{contract.Ref()},
	}
	resolver := &appfake.Resolver{Providers: map[application.ProvisionerRef]provisioning.Provisioner{ref: f.provider}}
	service, err := application.NewService(f.catalog, &appfake.Selector{Ref: ref}, resolver, f.store, appfake.AllowAll{})
	if err != nil {
		t.Fatal(err)
	}
	f.service = service
	return f
}

func (f *eagerFixture) enableEager() {
	if !f.eagerOn {
		f.service.EnableEagerExecutionForTesting()
		f.eagerOn = true
	}
}

func (f *eagerFixture) tick() time.Time {
	f.nextClock++
	return time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC).Add(time.Duration(f.nextClock) * time.Minute)
}

func (f *eagerFixture) admit(t *testing.T, id string, references map[string][]string) application.Result {
	t.Helper()
	command := application.CreateResourceCommand{
		Actor: appfake.Principal("eager"), ID: domain.ResourceID(id),
		Type: domain.ResourceTypeRef{Name: m21Type, Version: "v1"}, Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: validSpec(map[string]any{"name": id}), References: references,
		OperationID:    domain.OperationID("op-eager-create-" + id),
		EventID:        domain.EventID("evt-eager-create-" + id),
		RequestedAt:    f.tick(),
		IdempotencyKey: "key-eager-create-" + id,
	}
	result, err := f.service.AdmitCreateResource(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (f *eagerFixture) create(t *testing.T, id string, references map[string][]string) (application.Result, error) {
	t.Helper()
	command := application.CreateResourceCommand{
		Actor: appfake.Principal("eager"), ID: domain.ResourceID(id),
		Type: domain.ResourceTypeRef{Name: m21Type, Version: "v1"}, Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: validSpec(map[string]any{"name": id}), References: references,
		OperationID:    domain.OperationID("op-eager-create-" + id),
		EventID:        domain.EventID("evt-eager-create-" + id),
		RequestedAt:    f.tick(),
		IdempotencyKey: "key-eager-create-" + id,
	}
	return f.service.CreateResource(context.Background(), command)
}

// settleAnchorState rewrites the anchor's durable lifecycle to the requested
// state exactly as a drained worker would have produced it.
func (f *eagerFixture) settleAnchorState(t *testing.T, id string, state domain.ResourceState) {
	t.Helper()
	err := f.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		ctx := context.Background()
		resourceID := domain.ResourceID(id)
		active, found, err := tx.Operations().ActiveForResource(ctx, resourceID)
		if err != nil || !found {
			if err != nil {
				return err
			}
			return nil
		}
		record, err := tx.Resources().GetResource(ctx, resourceID)
		if err != nil {
			return err
		}
		at := f.tick()
		operation := active.Operation
		for _, phase := range []domain.OperationPhase{domain.OperationPhaseValidating, domain.OperationPhasePlanning} {
			if err := operation.AdvancePhase(phase, at); err != nil {
				return err
			}
		}
		switch state {
		case domain.ResourceStateReady:
			if err := operation.AdvancePhase(domain.OperationPhaseApplying, at); err != nil {
				return err
			}
			if err := operation.Succeed(at); err != nil {
				return err
			}
		case domain.ResourceStateFailed:
			if err := operation.Fail(string(lifecycle.ReasonDependencyFailed), "anchor failed for eager test", at); err != nil {
				return err
			}
		case domain.ResourceStateDeleting:
			// Classification reads only the STATUS state; a create Operation
			// has no Destroying phase, so the cursor stays untouched here.
		default:
			return errors.New("unsupported settle state")
		}
		if err := tx.Operations().SaveOperation(ctx, application.OperationRecord{Operation: operation, Version: active.Version}, active.Version); err != nil {
			return err
		}
		conditions := record.Status.Conditions()
		readyStatus := domain.ConditionStatusFalse
		reconciledStatus := domain.ConditionStatusFalse
		if state == domain.ResourceStateReady {
			readyStatus = domain.ConditionStatusTrue
			reconciledStatus = domain.ConditionStatusTrue
		}
		for _, wanted := range []struct {
			name   string
			status domain.ConditionStatus
		}{
			{lifecycle.ConditionReady, readyStatus},
			{lifecycle.ConditionReconciled, reconciledStatus},
			{lifecycle.ConditionReconciling, domain.ConditionStatusFalse},
		} {
			condition, err := domain.NewCondition(wanted.name, wanted.status, "EagerTestSettle", "", operation.TargetGeneration(), at)
			if err != nil {
				return err
			}
			replaced := false
			for i := range conditions {
				if conditions[i].Type() == wanted.name {
					conditions[i] = condition
					replaced = true
				}
			}
			if !replaced {
				conditions = append(conditions, condition)
			}
		}
		status, err := domain.NewResourceStatus(resourceID, operation.TargetGeneration(), state, conditions, at)
		if err != nil {
			return err
		}
		record.Status = status
		if err := tx.Resources().SaveResource(ctx, record, record.Version); err != nil {
			return err
		}
		execution, err := tx.Executions().GetExecution(ctx, active.Operation.ID())
		if err != nil {
			return err
		}
		if state == domain.ResourceStateReady {
			execution.State = application.AttemptSucceeded
		} else if state == domain.ResourceStateFailed {
			execution.State = application.AttemptFailed
		}
		return tx.Executions().SaveExecution(ctx, execution, execution.Version)
	})
	if err != nil {
		t.Fatalf("settle %s -> %s: %v", id, state, err)
	}
}

func (f *eagerFixture) pendingDrives(t *testing.T, operationID domain.OperationID) []application.OutboxMessage {
	t.Helper()
	var messages []application.OutboxMessage
	err := f.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		summary, err := tx.Outbox().SummarizeWorkByOperation(context.Background(), operationID)
		if err != nil {
			return err
		}
		messages = summary.Active
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return messages
}

// TestM21EagerPendingDependencyNeverSubmits is required case 1: a Pending hard
// dependency plus eager composition must produce ZERO provider submissions and
// hand the operation to the durable worker with a fresh canonical Drive.
func TestM21EagerPendingDependencyNeverSubmits(t *testing.T) {
	f := newEagerFixture(t, 1)
	anchor := f.admit(t, "e-anchor", nil)
	f.enableEager()

	_, err := f.create(t, "e-dependent", map[string][]string{"dependency": {"e-anchor"}})
	if !errors.Is(err, application.ErrEagerExecutionBlockedByDependencies) {
		t.Fatalf("error = %v, want ErrEagerExecutionBlockedByDependencies", err)
	}
	if got := f.provider.count(dependentOperationID(anchor, "e-dependent")); got != 0 {
		t.Fatalf("submissions = %d, want 0", got)
	}
	drives := f.pendingDrives(t, dependentOperationID(anchor, "e-dependent"))
	foundHandoff := false
	for _, message := range drives {
		if message.Kind == application.OutboxDrive && message.State == application.OutboxPending {
			foundHandoff = true
		}
	}
	if !foundHandoff {
		t.Fatal("hand-off left no pending canonical Drive for the durable worker")
	}
}

// TestM21EagerFailedAndInvalidDependenciesNeverSubmit covers required cases 2
// and 3 in one flow over two fixtures.
func TestM21EagerFailedAndInvalidDependenciesNeverSubmit(t *testing.T) {
	// Failed dependency.
	failed := newEagerFixture(t, 1)
	failed.admit(t, "f-anchor", nil)
	failed.settleAnchorState(t, "f-anchor", domain.ResourceStateFailed)
	failed.enableEager()
	if _, err := failed.create(t, "f-dep", map[string][]string{"dependency": {"f-anchor"}}); !errors.Is(err, application.ErrEagerExecutionBlockedByDependencies) {
		t.Fatalf("failed-dependency error = %v", err)
	}
	if got := failed.provider.count(opIDOf("op-eager-create-f-dep")); got != 0 {
		t.Fatalf("failed-dependency submissions = %d, want 0", got)
	}

	// Invalid dependency AFTER admission: the anchor begins deleting between
	// the dependent's admission/convergence and its next execution — exactly
	// the post-admission transition the pre-Submit gate owns. The eager UPDATE
	// re-runs the gate against the now-Deleting target.
	invalid := newEagerFixture(t, 1)
	invalid.admit(t, "i-anchor", nil)
	invalid.admit(t, "i-dep", map[string][]string{"dependency": {"i-anchor"}})
	invalid.settleBothReady(t, "i-anchor", "i-dep")
	invalid.enableEager()
	invalid.flipStatus(t, "i-anchor", domain.ResourceStateDeleting)

	update := application.UpdateResourceCommand{
		Actor:              appfake.Principal("eager"),
		ID:                 "i-dep",
		ExpectedGeneration: invalid.generationOf(t, "i-dep"),
		Spec:               validSpec(map[string]any{"name": "i-dep-v2"}),
		ReferencesPresent:  true,
		References:         map[string][]string{"dependency": {"i-anchor"}},
		OperationID:        domain.OperationID("op-eager-update-i-dep"),
		EventID:            domain.EventID("evt-eager-update-i-dep"),
		RequestedAt:        invalid.tick(),
		IdempotencyKey:     "key-eager-update-i-dep",
	}
	if _, err := invalid.service.UpdateResource(context.Background(), update); !errors.Is(err, application.ErrEagerExecutionBlockedByDependencies) {
		t.Fatalf("invalid-dependency update error = %v", err)
	}
	if got := invalid.provider.count(opIDOf("op-eager-update-i-dep")); got != 0 {
		t.Fatalf("invalid-dependency submissions = %d, want 0", got)
	}
}

// TestM21EagerReadyDependencyMayProceedInline is required case 4: an all-READY
// reference set permits the supported inline path to complete normally.
func TestM21EagerReadyDependencyMayProceedInline(t *testing.T) {
	f := newEagerFixture(t, 1)
	f.admit(t, "r-anchor", nil)
	f.settleAnchorState(t, "r-anchor", domain.ResourceStateReady)
	f.enableEager()

	result, err := f.create(t, "r-dep", map[string][]string{"dependency": {"r-anchor"}})
	if err != nil {
		t.Fatalf("ready-dependency eager execution failed: %v", err)
	}
	if result.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("dependent state = %s, want Succeeded", result.Operation.State())
	}
	if got := f.provider.count(result.Operation.ID()); got != 1 {
		t.Fatalf("submissions = %d, want exactly 1", got)
	}
	var applied int
	err = f.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		edges, err := tx.References().AppliedReferences(context.Background(), "r-dep")
		applied = len(edges)
		return err
	})
	if err != nil || applied != 1 {
		t.Fatalf("applied edges = %d err=%v, want 1", applied, err)
	}
}

// TestM21EagerZeroReferenceBehaviorUnchanged is required case 5: types without
// reference contracts keep today's eager behavior byte-for-byte.
func TestM21EagerZeroReferenceBehaviorUnchanged(t *testing.T) {
	f := newEagerFixture(t) // no slots declared
	f.enableEager()
	result, err := f.create(t, "z-plain", nil)
	if err != nil {
		t.Fatalf("zero-reference eager create failed: %v", err)
	}
	if result.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("state = %s, want Succeeded", result.Operation.State())
	}
	if got := f.provider.count(result.Operation.ID()); got != 1 {
		t.Fatalf("submissions = %d, want exactly 1", got)
	}
}

func dependentOperationID(seed application.Result, id string) domain.OperationID {
	_ = seed
	return opIDOf("op-eager-create-" + id)
}

func opIDOf(value string) domain.OperationID { return domain.OperationID(value) }

func (f *eagerFixture) generationOf(t *testing.T, id string) uint64 {
	t.Helper()
	var generation uint64
	err := f.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		record, err := tx.Resources().GetResource(context.Background(), domain.ResourceID(id))
		if err != nil {
			return err
		}
		generation = record.Resource.Generation()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return generation
}

// settleBothReady converges two admitted resources exactly as a drained
// worker would, including applied-reference advancement for the dependent.
func (f *eagerFixture) settleBothReady(t *testing.T, anchorID, dependentID string) {
	t.Helper()
	f.settleTerminalReady(t, anchorID)
	f.settleTerminalReady(t, dependentID)
	err := f.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		ctx := context.Background()
		edges, err := tx.References().DesiredReferences(ctx, domain.ResourceID(dependentID))
		if err != nil {
			return err
		}
		return tx.References().AdvanceAppliedReferences(ctx, domain.ResourceID(dependentID), 1, edges)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *eagerFixture) settleTerminalReady(t *testing.T, id string) {
	f.settleAnchorState(t, id, domain.ResourceStateReady)
}

// flipStatus rewrites only the durable STATUS state of a terminal Resource,
// modeling the post-admission gate-relevant transition under test.
func (f *eagerFixture) flipStatus(t *testing.T, id string, state domain.ResourceState) {
	t.Helper()
	err := f.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		ctx := context.Background()
		resourceID := domain.ResourceID(id)
		record, err := tx.Resources().GetResource(ctx, resourceID)
		if err != nil {
			return err
		}
		at := f.tick()
		status, err := domain.NewResourceStatus(resourceID, record.Status.ObservedGeneration(), state, record.Status.Conditions(), at)
		if err != nil {
			return err
		}
		record.Status = status
		return tx.Resources().SaveResource(ctx, record, record.Version)
	})
	if err != nil {
		t.Fatalf("flip %s -> %s: %v", id, state, err)
	}
}
