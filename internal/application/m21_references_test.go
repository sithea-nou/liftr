// SPDX-License-Identifier: Apache-2.0

package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
	"github.com/sithea-nou/liftr/internal/lifecycle"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/resourcecontract"

	appfake "github.com/sithea-nou/liftr/internal/application/fake"
)

// targetAuthorizer denies resource:read for explicitly listed Resource IDs and
// allows everything else, so tests can model a principal losing read access to
// an already-admitted dependency target.
type targetAuthorizer struct {
	deniedReads map[domain.ResourceID]struct{}
}

func (a *targetAuthorizer) Authorize(_ context.Context, _ identity.Principal, action identity.Action, target identity.ResourceTarget) error {
	if action == identity.ActionResourceRead {
		if _, denied := a.deniedReads[target.ResourceID]; denied {
			return identity.ErrInvalidPrincipal
		}
	}
	return nil
}

func (a *targetAuthorizer) AuthorizeResourceList(context.Context, identity.Principal) (identity.ResourceVisibility, error) {
	return identity.ResourceVisibility{AllOwners: true}, nil
}

type m21Fixture struct {
	service    *application.Service
	store      *appfake.Store
	catalog    *strictCatalog
	authorizer *targetAuthorizer
	ref        application.ProvisionerRef
	clock      int64
}

const m21Type = "Relational"

func newM21Fixture(t *testing.T, slots ...resourcecontract.ReferenceSlot) *m21Fixture {
	t.Helper()
	fixture := &m21Fixture{store: appfake.NewStore(), authorizer: &targetAuthorizer{}}
	ref, err := application.NewProvisionerRef("m21-provider")
	if err != nil {
		t.Fatal(err)
	}
	fixture.ref = ref
	contract := newStrictContract(m21Type)
	if len(slots) > 0 {
		contract.WithReferences(slots...)
	}
	catalog := &strictCatalog{types: map[domain.ResourceTypeRef]*strictContract{contract.Ref(): contract}, order: []domain.ResourceTypeRef{contract.Ref()}}
	fixture.catalog = catalog
	selector := &appfake.Selector{Ref: ref}
	resolver := &appfake.Resolver{Providers: map[application.ProvisionerRef]provisioning.Provisioner{
		ref: appfakeProvider{},
	}}
	service, err := application.NewService(catalog, selector, resolver, fixture.store, fixture.authorizer)
	if err != nil {
		t.Fatal(err)
	}
	fixture.service = service
	return fixture
}

func m21Slot(name string, minItems, maxItems int) resourcecontract.ReferenceSlot {
	return resourcecontract.ReferenceSlot{
		Name:               name,
		AllowedTargetTypes: []domain.ResourceTypeRef{{Name: m21Type, Version: "v1"}},
		MinItems:           minItems,
		MaxItems:           maxItems,
	}
}

// tick hands out strictly increasing instants so lifecycle monotonicity holds.
func (f *m21Fixture) tick() time.Time {
	f.clock++
	return time.Date(2026, 8, 26, 9, 0, int(f.clock)%60, 0, time.UTC).Add(time.Duration(f.clock) * time.Minute)
}

func (f *m21Fixture) currentGeneration(t *testing.T, id string) uint64 {
	t.Helper()
	record, err := f.store.GetResource(context.Background(), domain.ResourceID(id))
	if err != nil {
		t.Fatal(err)
	}
	return record.Resource.Generation()
}

// settle marks the Resource's active admitted Operation durably Succeeded and
// its status Ready with a generation-matched Reconciled condition — the state
// a drained worker would have produced — so admission-focused tests never hit
// the one-active-Operation invariant.
func (f *m21Fixture) settle(t *testing.T, id string) {
	t.Helper()
	err := f.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		ctx := context.Background()
		resourceID := domain.ResourceID(id)
		active, found, err := tx.Operations().ActiveForResource(ctx, resourceID)
		if err != nil || !found {
			return err
		}
		record, err := tx.Resources().GetResource(ctx, resourceID)
		if err != nil {
			return err
		}
		at := f.tick()
		operation := active.Operation
		// Walk the phase cursor forward exactly as a drained worker would.
		for _, phase := range []domain.OperationPhase{domain.OperationPhaseValidating, domain.OperationPhasePlanning, domain.OperationPhaseApplying} {
			if err := operation.AdvancePhase(phase, at); err != nil {
				return err
			}
		}
		if err := operation.Succeed(at); err != nil {
			return err
		}
		if err := tx.Operations().SaveOperation(ctx, application.OperationRecord{Operation: operation, Version: active.Version}, active.Version); err != nil {
			return err
		}
		generation := operation.TargetGeneration()
		conditions := record.Status.Conditions()
		for _, wanted := range []struct {
			name   string
			status domain.ConditionStatus
			reason string
		}{
			{lifecycle.ConditionReconciled, domain.ConditionStatusTrue, "LifecycleSucceeded"},
			{lifecycle.ConditionReady, domain.ConditionStatusTrue, "LifecycleSucceeded"},
			{lifecycle.ConditionReconciling, domain.ConditionStatusFalse, "LifecycleSucceeded"},
			{lifecycle.ConditionOperationFailed, domain.ConditionStatusFalse, "NoFailure"},
		} {
			conditions, err = setTestCondition(conditions, wanted.name, wanted.status, wanted.reason, generation, at)
			if err != nil {
				return err
			}
		}
		status, err := domain.NewResourceStatus(resourceID, generation, domain.ResourceStateReady, conditions, at)
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
		execution.State = application.AttemptSucceeded
		execution.Correlation = provisioning.RequestCorrelationFound
		execution.AcceptanceConfirmed = true
		handle, handleErr := provisioning.NewExecutionHandle("fake-execution-" + string(active.Operation.ID()))
		if handleErr != nil {
			return handleErr
		}
		execution.Handle = &handle
		execution.Submission = &provisioning.Submission{Observation: provisioning.ExecutionObservation{
			Correlation: provisioning.RequestCorrelationFound,
			Execution:   &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle},
		}}
		execution.LastObservedAt = at
		execution.LastProviderObservedAt = at
		return tx.Executions().SaveExecution(ctx, execution, execution.Version)
	})
	if err != nil {
		t.Fatalf("settle %s: %v", id, err)
	}
}

func setTestCondition(conditions []domain.Condition, name string, status domain.ConditionStatus, reason string, generation uint64, at time.Time) ([]domain.Condition, error) {
	condition, err := domain.NewCondition(name, status, reason, "", generation, at)
	if err != nil {
		return nil, err
	}
	for i := range conditions {
		if conditions[i].Type() == name {
			conditions[i] = condition
			return conditions, nil
		}
	}
	return append(conditions, condition), nil
}

func (f *m21Fixture) create(t *testing.T, id string, references map[string][]string) application.Result {
	t.Helper()
	command := application.CreateResourceCommand{
		Actor:          appfake.Principal("tester"),
		ID:             domain.ResourceID(id),
		Type:           domain.ResourceTypeRef{Name: m21Type, Version: "v1"},
		Owner:          domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec:           validSpec(map[string]any{"name": id}),
		References:     references,
		OperationID:    domain.OperationID("op-create-" + id),
		EventID:        domain.EventID("evt-create-" + id),
		RequestedAt:    f.tick(),
		IdempotencyKey: "key-create-" + id,
	}
	result, err := f.service.AdmitCreateResource(context.Background(), command)
	if err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	f.settle(t, id)
	return result
}

func (f *m21Fixture) update(t *testing.T, id string, references map[string][]string, present bool, key string) (application.Result, error) {
	t.Helper()
	return f.updateWithGeneration(t, id, f.currentGeneration(t, id), references, present, key)
}

func (f *m21Fixture) updateWithGeneration(t *testing.T, id string, generation uint64, references map[string][]string, present bool, key string) (application.Result, error) {
	t.Helper()
	command := application.UpdateResourceCommand{
		Actor:              appfake.Principal("tester"),
		ID:                 domain.ResourceID(id),
		ExpectedGeneration: generation,
		Spec:               validSpec(map[string]any{"name": id + "-v" + key}),
		ReferencesPresent:  present,
		References:         references,
		OperationID:        domain.OperationID("op-update-" + id + "-" + key),
		EventID:            domain.EventID("evt-update-" + id + "-" + key),
		RequestedAt:        f.tick(),
		IdempotencyKey:     key,
	}
	result, err := f.service.AdmitUpdateResource(context.Background(), command)
	if err == nil {
		f.settle(t, id)
	}
	return result, err
}

func (f *m21Fixture) desired(t *testing.T, id string) []application.ReferenceEdge {
	t.Helper()
	var edges []application.ReferenceEdge
	err := f.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		var err error
		edges, err = tx.References().DesiredReferences(context.Background(), domain.ResourceID(id))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return edges
}

// TestM21UnknownSlotRejected rejects slots not declared by the contract.
func TestM21UnknownSlotRejected(t *testing.T) {
	fixture := newM21Fixture(t, m21Slot("database", 1, 1))
	fixture.create(t, "target-a", nil)
	baseline := fixture.store.RecordCounts()
	command := application.CreateResourceCommand{
		Actor: appfake.Principal("tester"), ID: "source-x",
		Type: domain.ResourceTypeRef{Name: m21Type, Version: "v1"}, Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: validSpec(map[string]any{"name": "x"}), References: map[string][]string{"cache": {"target-a"}},
		OperationID: "op-x", EventID: "evt-x", RequestedAt: fixture.tick(), IdempotencyKey: "k-x",
	}
	_, err := fixture.service.AdmitCreateResource(context.Background(), command)
	var invalid *application.InvalidReferenceError
	if !errors.As(err, &invalid) || invalid.Violations[0].Keyword != "unknown-slot" {
		t.Fatalf("error = %v, want unknown-slot violation", err)
	}
	if counts := fixture.store.RecordCounts(); counts != baseline {
		t.Fatalf("rejected admission persisted durable state: %+v", counts)
	}
}

// TestM21DuplicateAndSelfReferenceRejected covers structural edge validation.
func TestM21DuplicateAndSelfReferenceRejected(t *testing.T) {
	fixture := newM21Fixture(t, m21Slot("dependency", 1, 4))
	fixture.create(t, "t1", nil)

	command := func(id string, refs map[string][]string, op string) application.CreateResourceCommand {
		return application.CreateResourceCommand{
			Actor: appfake.Principal("tester"), ID: domain.ResourceID(id),
			Type: domain.ResourceTypeRef{Name: m21Type, Version: "v1"}, Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
			Spec: validSpec(map[string]any{"name": id}), References: refs,
			OperationID: domain.OperationID(op), EventID: domain.EventID("evt-" + op), RequestedAt: fixture.tick(), IdempotencyKey: "k-" + op,
		}
	}
	if _, err := fixture.service.AdmitCreateResource(context.Background(), command("s1", map[string][]string{"dependency": {"t1", "t1"}}, "op-s1")); !errors.As(err, new(*application.InvalidReferenceError)) {
		t.Fatalf("duplicate target error = %v", err)
	}
	self := command("self-1", nil, "op-self")
	self.References = map[string][]string{"dependency": {"self-1"}}
	if _, err := fixture.service.AdmitCreateResource(context.Background(), self); !errors.As(err, new(*application.InvalidReferenceError)) {
		t.Fatalf("self reference error = %v", err)
	}
}

// TestM21CardinalityEnforced pins max slot cardinality on supplied sets.
func TestM21CardinalityEnforced(t *testing.T) {
	fixture := newM21Fixture(t, m21Slot("dependency", 1, 1))
	fixture.create(t, "t1", nil)
	fixture.create(t, "t2", nil)
	command := func(refs map[string][]string, id, op string) application.CreateResourceCommand {
		return application.CreateResourceCommand{
			Actor: appfake.Principal("tester"), ID: domain.ResourceID(id),
			Type: domain.ResourceTypeRef{Name: m21Type, Version: "v1"}, Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
			Spec: validSpec(map[string]any{"name": id}), References: refs,
			OperationID: domain.OperationID(op), EventID: domain.EventID("evt-" + op), RequestedAt: fixture.tick(), IdempotencyKey: "k-" + op,
		}
	}
	if _, err := fixture.service.AdmitCreateResource(context.Background(), command(nil, "empty-ok", "op-empty")); err != nil {
		t.Fatalf("omitted references must be admissible: %v", err)
	}
	if _, err := fixture.service.AdmitCreateResource(context.Background(), command(map[string][]string{"dependency": {"t1", "t2"}}, "over", "op-over")); !errors.As(err, new(*application.InvalidReferenceError)) {
		t.Fatalf("maxItems error = %v", err)
	}
}

// TestM21TwoNodeCycleRejected pins Correction 1's canonical case: existing
// B->A; proposing A->B must reject as DEPENDENCY_CYCLE via OUTGOING traversal.
func TestM21TwoNodeCycleRejected(t *testing.T) {
	fixture := newM21Fixture(t, m21Slot("dependency", 0, 1))
	fixture.create(t, "a", nil)
	fixture.create(t, "b", map[string][]string{"dependency": {"a"}})

	if _, err := fixture.update(t, "a", map[string][]string{"dependency": {"b"}}, true, "key-cycle-ab"); !errors.Is(err, application.ErrDependencyCycle) {
		t.Fatalf("error = %v, want ErrDependencyCycle", err)
	}
	if edges := fixture.desired(t, "a"); len(edges) != 0 {
		t.Fatalf("cycle rejection mutated desired set: %+v", edges)
	}
}

// TestM21ThreeNodeCycleRejected proves transitive outgoing-edge detection.
func TestM21ThreeNodeCycleRejected(t *testing.T) {
	fixture := newM21Fixture(t, m21Slot("dependency", 0, 1))
	fixture.create(t, "c3", nil)
	fixture.create(t, "b2", map[string][]string{"dependency": {"c3"}})
	fixture.create(t, "a1", map[string][]string{"dependency": {"b2"}})
	if _, err := fixture.update(t, "c3", map[string][]string{"dependency": {"a1"}}, true, "key-cycle-c3"); !errors.Is(err, application.ErrDependencyCycle) {
		t.Fatalf("error = %v, want ErrDependencyCycle", err)
	}
}

// TestM21DeepGraphFailsConservatively proves that traversal reaching the
// configured depth bound while further outgoing edges exist is NEVER treated
// as proof of acyclicity. The oversized downstream graph is seeded directly so
// the test isolates the proof bound from admission-time construction.
func TestM21DeepGraphFailsConservatively(t *testing.T) {
	fixture := newM21Fixture(t, m21Slot("dependency", 0, 1))
	// Create the chain nodes legitimately first so target validation passes;
	// the oversized downstream EDGES are then seeded directly to isolate the
	// proof bound from admission-time construction.
	const depth = application.MaxDependencyDepth + 4
	nodes := make([]string, 0, depth+2)
	nodes = append(nodes, "deep-root")
	for i := 0; i <= depth; i++ {
		nodes = append(nodes, chainNode(i))
	}
	for _, node := range nodes {
		fixture.create(t, node, nil)
	}
	err := fixture.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		ctx := context.Background()
		// Seed n_i -> n_{i+1} downstream of chainNode(0), ending at
		// deep-root. The graph stays acyclic; the PROPOSED edge
		// deep-root -> chainNode(0) would close it through a path longer than
		// the configured depth bound.
		for i := 1; i < len(nodes)-1; i++ {
			edges := []application.ReferenceEdge{{Slot: "dependency", TargetID: domain.ResourceID(nodes[i+1]), Generation: 1}}
			if err := tx.References().ReplaceDesiredReferences(ctx, domain.ResourceID(nodes[i]), 1, edges); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.update(t, "deep-root", map[string][]string{"dependency": {chainNode(0)}}, true, "key-deep"); !errors.Is(err, application.ErrReferenceGraphLimit) {
		t.Fatalf("error = %v, want ErrReferenceGraphLimit (conservative failure)", err)
	}
}

func chainNode(i int) string {
	return "deep-seed-" + string(rune('a'+i%26)) + string(rune('0'+i/26))
}

// TestM21ReorderedReferencesReplaySameFingerprint pins fingerprint
// canonicalization: same key with reordered arrays replays; the durable PUT
// semantics are unchanged for a DIFFERENT key (Correction 5).
func TestM21ReorderedReferencesReplaySameFingerprint(t *testing.T) {
	fixture := newM21Fixture(t, m21Slot("dependency", 0, 4), m21Slot("cache", 0, 4))
	fixture.create(t, "r1", nil)
	fixture.create(t, "r2", nil)
	fixture.create(t, "r3", nil)
	fixture.create(t, "source", map[string][]string{"dependency": {"r1", "r2"}, "cache": {"r3"}})
	generation := fixture.currentGeneration(t, "source")

	first, err := fixture.updateWithGeneration(t, "source", generation, map[string][]string{"dependency": {"r1", "r2"}, "cache": {"r3"}}, true, "key-reorder")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := fixture.updateWithGeneration(t, "source", generation, map[string][]string{"cache": {"r3"}, "dependency": {"r2", "r1"}}, true, "key-reorder")
	if err != nil {
		t.Fatalf("reordered replay failed: %v", err)
	}
	if !replay.Replay || replay.Operation.ID() != first.Operation.ID() {
		t.Fatalf("reorder produced a new admission instead of replay (replay=%t)", replay.Replay)
	}
	// A DIFFERENT key with reordered content preserves existing PUT semantics:
	// it is admitted as a new generation (Correction 5: no content no-op PUT).
	before := fixture.currentGeneration(t, "source")
	if _, err := fixture.update(t, "source", map[string][]string{"cache": {"r3"}, "dependency": {"r2", "r1"}}, true, "key-fresh"); err != nil {
		t.Fatalf("fresh-key reorder should be admitted normally: %v", err)
	}
	if after := fixture.currentGeneration(t, "source"); after != before+1 {
		t.Fatalf("fresh reorder generation = %d, want %d (existing PUT semantics preserved)", after, before+1)
	}
}

// TestM21UpdateAbsentReferencesPreservesEdgesWithoutTargetAuthorization pins
// Correction 6: preserved durable edges are trusted intent. A caller whose
// read grant on the already-admitted target was revoked can still update the
// source spec while references are absent.
func TestM21UpdateAbsentReferencesPreservesEdgesWithoutTargetAuthorization(t *testing.T) {
	fixture := newM21Fixture(t, m21Slot("dependency", 0, 1))
	fixture.create(t, "kept", nil)
	fixture.create(t, "holder", map[string][]string{"dependency": {"kept"}})
	if edges := fixture.desired(t, "holder"); len(edges) != 1 {
		t.Fatalf("desired edges = %+v", edges)
	}
	fixture.authorizer.deniedReads = map[domain.ResourceID]struct{}{"kept": {}}
	if _, err := fixture.update(t, "holder", nil, false, "key-absent"); err != nil {
		t.Fatalf("absent-reference update must preserve edges without reauthorization: %v", err)
	}
	if edges := fixture.desired(t, "holder"); len(edges) != 1 || edges[0].TargetID != "kept" {
		t.Fatalf("preserved edges = %+v", edges)
	}
}

// TestM21UpdateReplacementAuthorizesOnlyNewTargets proves newly added targets
// require current read authorization while removed targets need none.
func TestM21UpdateReplacementAuthorizesOnlyNewTargets(t *testing.T) {
	fixture := newM21Fixture(t, m21Slot("dependency", 0, 2))
	fixture.create(t, "old-target", nil)
	fixture.create(t, "new-target", nil)
	fixture.create(t, "switcher", map[string][]string{"dependency": {"old-target"}})

	fixture.authorizer.deniedReads = map[domain.ResourceID]struct{}{"new-target": {}}
	if _, err := fixture.update(t, "switcher", map[string][]string{"dependency": {"new-target"}}, true, "key-hidden"); !errors.As(err, new(*application.InvalidReferenceError)) {
		t.Fatalf("unauthorized new target error = %v", err)
	}
	// Same refusal shape for a nonexistent target: no existence oracle.
	if _, err := fixture.update(t, "switcher", map[string][]string{"dependency": {"missing-target"}}, true, "key-missing"); !errors.As(err, new(*application.InvalidReferenceError)) {
		t.Fatalf("missing target error = %v", err)
	}
	// Removing the old edge requires no permission on it.
	fixture.authorizer.deniedReads = map[domain.ResourceID]struct{}{"old-target": {}, "missing-target": {}}
	if _, err := fixture.update(t, "switcher", map[string][]string{"dependency": {}}, true, "key-clear"); err != nil {
		t.Fatalf("clearing references must not reauthorize removed targets: %v", err)
	}
	if edges := fixture.desired(t, "switcher"); len(edges) != 0 {
		t.Fatalf("explicit replacement did not clear: %+v", edges)
	}
}

// TestM21DeleteProtectionAndFailClosed covers protective blocking, release
// after dependent deletion, and fail-closed corruption handling (Correction 4).
func TestM21DeleteProtectionAndFailClosed(t *testing.T) {
	fixture := newM21Fixture(t, m21Slot("dependency", 0, 1))
	fixture.create(t, "precious", nil)
	fixture.create(t, "dependent", map[string][]string{"dependency": {"precious"}})

	deleteCmd := func(id, op string) application.DeleteResourceCommand {
		return application.DeleteResourceCommand{
			Actor: appfake.Principal("tester"), ID: domain.ResourceID(id),
			ExpectedGeneration: fixture.currentGeneration(t, id),
			OperationID:        domain.OperationID(op), EventID: domain.EventID("evt-" + op),
			RequestedAt: fixture.tick(), IdempotencyKey: "k-" + op,
		}
	}
	if _, err := fixture.service.AdmitDeleteResource(context.Background(), deleteCmd("precious", "op-del-1")); !errors.Is(err, application.ErrResourceInUse) {
		t.Fatalf("protected delete error = %v, want ErrResourceInUse", err)
	}

	// Simulate proper dependent deletion: outgoing rows released atomically
	// with the Deleted transition.
	err := fixture.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		return tx.References().DeleteReferencesForSource(context.Background(), "dependent")
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.AdmitDeleteResource(context.Background(), deleteCmd("precious", "op-del-2")); err != nil {
		t.Fatalf("released delete failed: %v", err)
	}

	// Corruption: a Deleted source still owning protective rows fails closed.
	fixture2 := newM21Fixture(t, m21Slot("dependency", 0, 1))
	fixture2.create(t, "p2", nil)
	fixture2.create(t, "d2", map[string][]string{"dependency": {"p2"}})
	err = fixture2.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		ctx := context.Background()
		record, err := tx.Resources().GetResource(ctx, "d2")
		if err != nil {
			return err
		}
		status, err := domain.NewResourceStatus(record.Resource.ID(), record.Status.ObservedGeneration(), domain.ResourceStateDeleted, record.Status.Conditions(), record.Status.UpdatedAt().Add(time.Minute))
		if err != nil {
			return err
		}
		record.Status = status
		return tx.Resources().SaveResource(ctx, record, record.Version)
	})
	if err != nil {
		t.Fatal(err)
	}
	_, delErr := fixture2.service.AdmitDeleteResource(context.Background(), application.DeleteResourceCommand{
		Actor: appfake.Principal("tester"), ID: "p2", ExpectedGeneration: 1,
		OperationID: "op-corrupt-del", EventID: "evt-corrupt-del",
		RequestedAt: fixture2.tick(), IdempotencyKey: "k-corrupt-del",
	})
	if !errors.Is(delErr, application.ErrReferenceInvariant) {
		t.Fatalf("corrupted protective state error = %v, want ErrReferenceInvariant (fail closed)", delErr)
	}
}
