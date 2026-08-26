// SPDX-License-Identifier: Apache-2.0

package worker_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	applicationfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/lifecycle"
	"github.com/sithea-nou/liftr/internal/provisioning"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
	"github.com/sithea-nou/liftr/internal/worker"
)

// m21Provider fails submission for explicitly listed Operations and otherwise
// behaves as the deterministic synchronous fake.
type m21Provider struct {
	inner   *provisioningfake.Provisioner
	failFor map[domain.OperationID]struct{}
}

func newM21Provider() *m21Provider {
	return &m21Provider{inner: provisioningfake.New(provisioningfake.ModeSynchronous), failFor: map[domain.OperationID]struct{}{}}
}

func (p *m21Provider) Capabilities() []provisioning.ProvisionerCapability {
	return p.inner.Capabilities()
}

func (p *m21Provider) Submit(ctx context.Context, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	if _, fail := p.failFor[request.OperationID]; !fail {
		return p.inner.Submit(ctx, request)
	}
	handle, err := provisioning.NewExecutionHandle("failed-" + string(request.OperationID))
	if err != nil {
		return provisioning.Submission{}, err
	}
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{
			State:   provisioning.ExecutionStateFailed,
			Handle:  &handle,
			Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureExecution, Reason: "ExecutionFailed", Message: "dependency anchor failed"},
		},
		Resource: provisioning.ResourceObservation{Presence: provisioning.ResourcePresenceUnknown, Readiness: provisioning.ResourceReadinessUnknown, Drift: provisioning.ResourceDriftUnknown},
	}}, nil
}

func (p *m21Provider) Observe(ctx context.Context, request provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	if _, fail := p.failFor[request.OperationID]; !fail {
		return p.inner.Observe(ctx, request)
	}
	return provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{
			State:   provisioning.ExecutionStateFailed,
			Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureExecution, Reason: "ExecutionFailed", Message: "dependency anchor failed"},
		},
	}, nil
}

// m21Harness composes the durable worker with a catalog that declares a
// self-referencing dependency slot on the shared fake resource type.
type m21Harness struct {
	service  *application.Service
	store    *applicationfake.Store
	instance *worker.Worker
	provider *m21Provider
	resolver *applicationfake.Resolver
}

func newM21Harness(t *testing.T) *m21Harness {
	t.Helper()
	ref, err := application.NewProvisionerRef("m21-worker-provider")
	if err != nil {
		t.Fatal(err)
	}
	h := &m21Harness{store: applicationfake.NewStore(), provider: newM21Provider()}
	h.resolver = &applicationfake.Resolver{Providers: map[application.ProvisionerRef]provisioning.Provisioner{ref: h.provider}}
	typeValue, err := domain.NewResourceType(provisioningfake.ResourceType(), "worker test resource", []domain.Capability{
		domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete,
	})
	if err != nil {
		t.Fatal(err)
	}
	slots, err := resourcecontract.NewReferenceContract([]resourcecontract.ReferenceSlot{{
		Name:               "dependency",
		AllowedTargetTypes: []domain.ResourceTypeRef{provisioningfake.ResourceType()},
		MinItems:           0,
		MaxItems:           1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	catalog := applicationfake.Catalog{
		Types:      map[domain.ResourceTypeRef]domain.ResourceType{provisioningfake.ResourceType(): typeValue},
		References: map[domain.ResourceTypeRef]*resourcecontract.ReferenceContract{provisioningfake.ResourceType(): &slots},
	}
	h.service, err = application.NewService(catalog, &applicationfake.Selector{Ref: ref}, h.resolver, h.store, applicationfake.AllowAll{})
	if err != nil {
		t.Fatal(err)
	}
	h.instance, err = worker.NewWithCatalog(h.store, h.resolver, catalog)
	if err != nil {
		t.Fatal(err)
	}
	h.instance.Clock = func() time.Time { return testTime.Add(time.Minute) }
	return h
}

func (h *m21Harness) create(t *testing.T, id string, references map[string][]string) application.Result {
	t.Helper()
	command := application.CreateResourceCommand{
		Actor: applicationfake.Principal("tester"), ID: domain.ResourceID(id),
		Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: mustSpec(t, id), References: references,
		OperationID:    domain.OperationID("op-create-" + id),
		EventID:        domain.EventID("evt-create-" + id),
		RequestedAt:    testTime,
		IdempotencyKey: "key-create-" + id,
	}
	result, err := h.service.AdmitCreateResource(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func mustSpec(t *testing.T, name string) domain.ResourceSpec {
	t.Helper()
	spec, err := domain.NewResourceSpec(map[string]any{"size": uint64(3), "name": name})
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func (h *m21Harness) update(t *testing.T, id string, generation uint64, references map[string][]string, key string) application.Result {
	t.Helper()
	command := application.UpdateResourceCommand{
		Actor: applicationfake.Principal("tester"), ID: domain.ResourceID(id),
		ExpectedGeneration: generation, Spec: mustSpec(t, id+"-updated"),
		ReferencesPresent: true, References: references,
		OperationID: domain.OperationID("op-update-" + id + "-" + key), EventID: domain.EventID("evt-update-" + key),
		RequestedAt: testTime.Add(time.Minute), IdempotencyKey: key,
	}
	result, err := h.service.AdmitUpdateResource(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (h *m21Harness) operationState(t *testing.T, operationID domain.OperationID) domain.OperationState {
	t.Helper()
	record, err := h.store.GetOperation(context.Background(), operationID)
	if err != nil {
		t.Fatal(err)
	}
	return record.Operation.State()
}

func (h *m21Harness) conditionOf(t *testing.T, id string) (domain.ConditionStatus, string, bool) {
	t.Helper()
	record, err := h.store.GetResource(context.Background(), domain.ResourceID(id))
	if err != nil {
		t.Fatal(err)
	}
	for _, condition := range record.Status.Conditions() {
		if condition.Type() == lifecycle.ConditionDependenciesReady {
			return condition.Status(), condition.Reason(), true
		}
	}
	return "", "", false
}

func (h *m21Harness) edges(t *testing.T, table func(application.UnitOfWork) ([]application.ReferenceEdge, error)) []application.ReferenceEdge {
	t.Helper()
	var edges []application.ReferenceEdge
	err := h.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		var err error
		edges, err = table(tx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return edges
}

func desiredOf(id string) func(tx application.UnitOfWork) ([]application.ReferenceEdge, error) {
	return func(tx application.UnitOfWork) ([]application.ReferenceEdge, error) {
		return tx.References().DesiredReferences(context.Background(), domain.ResourceID(id))
	}
}

func appliedOf(id string) func(tx application.UnitOfWork) ([]application.ReferenceEdge, error) {
	return func(tx application.UnitOfWork) ([]application.ReferenceEdge, error) {
		return tx.References().AppliedReferences(context.Background(), domain.ResourceID(id))
	}
}

// TestM21GateBlocksUntilDependencyConvergesThenSubmitsOnce is deliberately
// ordering-agnostic: admission order of anchor and dependent work means either
// the gate passes immediately (anchor already Ready) or waits and is released
// by a versioned wake — in both interleavings the dependent Submits EXACTLY
// once and converges with applied == desired.
func TestM21GateBlocksUntilDependencyConvergesThenSubmitsOnce(t *testing.T) {
	h := newM21Harness(t)
	anchor := h.create(t, "anchor", nil)
	dependent := h.create(t, "dependent", map[string][]string{"dependency": {"anchor"}})

	drainLimit(t, h.instance, 64)

	if state := h.operationState(t, anchor.Operation.ID()); state != domain.OperationStateSucceeded {
		t.Fatalf("anchor state = %s", state)
	}
	if state := h.operationState(t, dependent.Operation.ID()); state != domain.OperationStateSucceeded {
		t.Fatalf("dependent state = %s", state)
	}
	if got := h.provider.inner.SubmissionCount(dependent.Operation.ID()); got != 1 {
		t.Fatalf("dependent submissions = %d, want exactly 1", got)
	}
	desired := h.edges(t, desiredOf("dependent"))
	applied := h.edges(t, appliedOf("dependent"))
	if len(desired) != 1 || len(applied) != 1 || desired[0].TargetID != "anchor" || applied[0].TargetID != "anchor" {
		t.Fatalf("applied/desired mismatch: desired=%+v applied=%+v", desired, applied)
	}
	if status, reason, found := h.conditionOf(t, "dependent"); !found || status != domain.ConditionStatusTrue || reason != lifecycle.ReasonDependenciesSatisfied {
		t.Fatalf("condition = (%s,%s,%t)", status, reason, found)
	}
}

// TestM21TerminalFailedDependencyFailsPreSubmission proves the closed-loop
// failure semantics: conclusively Failed dependency with no active recovery
// fails the dependent BEFORE any provider attempt; M13 retry re-evaluates live
// dependency truth instead of reusing cached readiness.
func TestM21TerminalFailedDependencyFailsPreSubmission(t *testing.T) {
	h := newM21Harness(t)
	anchor := h.create(t, "anchor", nil)
	h.provider.failFor[anchor.Operation.ID()] = struct{}{}
	drainLimit(t, h.instance, 16)
	if state := h.operationState(t, anchor.Operation.ID()); state != domain.OperationStateFailed {
		t.Fatalf("anchor state = %s, want Failed", state)
	}
	if status, reason, _ := h.conditionOf(t, "anchor"); status != "" && reason == lifecycle.ReasonWaitingForDependencies {
		t.Fatalf("anchor unexpectedly carries waiting condition")
	}

	dependent := h.create(t, "dependent", map[string][]string{"dependency": {"anchor"}})
	drainLimit(t, h.instance, 32)
	if state := h.operationState(t, dependent.Operation.ID()); state != domain.OperationStateFailed {
		t.Fatalf("dependent state = %s, want Failed pre-submission", state)
	}
	if got := h.provider.inner.SubmissionCount(dependent.Operation.ID()); got != 0 {
		t.Fatalf("pre-submission failure performed %d provider submissions", got)
	}
	if status, reason, found := h.conditionOf(t, "dependent"); !found || status != domain.ConditionStatusFalse || reason != lifecycle.ReasonDependencyFailed {
		t.Fatalf("condition = (%s,%s,%t), want False/DependencyFailed", status, reason, found)
	}
	execution, err := h.store.GetExecution(context.Background(), dependent.Operation.ID())
	if err != nil {
		t.Fatal(err)
	}
	if execution.State != application.AttemptFailed || execution.AcceptanceConfirmed {
		t.Fatalf("execution = (%s, accepted=%t), want AttemptFailed without acceptance", execution.State, execution.AcceptanceConfirmed)
	}
	waits, hasWaits := h.hasWaits(t, dependent.Operation.ID())
	if hasWaits && len(waits) > 0 {
		t.Fatalf("terminal gate failure left wait rows behind")
	}

	// M13 retry of the dependent re-evaluates current dependency truth.
	retryCmd := application.RetryOperationCommand{
		Actor: applicationfake.Principal("tester"), OperationID: dependent.Operation.ID(),
		ExpectedGeneration: 1, NewOperationID: domain.OperationID("op-retry-dependent"),
		EventID: "evt-retry", RequestedAt: testTime.Add(2 * time.Minute), IdempotencyKey: "key-retry",
	}
	retryResult, err := h.service.AdmitRetryOperation(context.Background(), retryCmd)
	if err != nil {
		t.Fatalf("retry admission failed: %v", err)
	}
	drainLimit(t, h.instance, 32)
	if state := h.operationState(t, retryResult.Operation.ID()); state != domain.OperationStateFailed {
		t.Fatalf("retried dependent state = %s, want Failed again", state)
	}
	if got := h.provider.inner.SubmissionCount(retryResult.Operation.ID()); got != 0 {
		t.Fatalf("retry reached the provider despite terminal dependency failure")
	}
}

func (h *m21Harness) hasWaits(t *testing.T, operationID domain.OperationID) ([]application.DependencyWait, bool) {
	t.Helper()
	var waits []application.DependencyWait
	err := h.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		var err error
		waits, _, err = tx.DependencyWaits().PageDependencyWaitersByTarget(context.Background(), "anchor", 0, 100)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	mine := waits[:0]
	for _, wait := range waits {
		if wait.OperationID == operationID {
			mine = append(mine, wait)
		}
	}
	return mine, len(mine) > 0
}

// TestM21UnionProtectionAcrossDependentUpdate pins the protective union: an
// applied-but-being-replaced target stays delete-blocked until the update's
// final convergence advances applied references exactly once.
func TestM21UnionProtectionAcrossDependentUpdate(t *testing.T) {
	h := newM21Harness(t)
	t1 := h.create(t, "union-t1", nil)
	t2 := h.create(t, "union-t2", nil)
	dependent := h.create(t, "union-user", map[string][]string{"dependency": {"union-t1"}})
	drainLimit(t, h.instance, 64)
	if state := h.operationState(t, dependent.Operation.ID()); state != domain.OperationStateSucceeded {
		t.Fatalf("dependent state = %s", state)
	}
	deleteT1 := func(op string, generation uint64) error {
		_, err := h.service.AdmitDeleteResource(context.Background(), application.DeleteResourceCommand{
			Actor: applicationfake.Principal("tester"), ID: "union-t1", ExpectedGeneration: generation,
			OperationID: domain.OperationID(op), EventID: domain.EventID("evt-" + op),
			RequestedAt: testTime.Add(time.Hour), IdempotencyKey: "key-" + op,
		})
		return err
	}
	if err := deleteT1("del-t1-a", 1); !errors.Is(err, application.ErrResourceInUse) {
		t.Fatalf("delete blocked-by-applied error = %v, want ErrResourceInUse", err)
	}

	h.update(t, "union-user", 1, map[string][]string{"dependency": {"union-t2"}}, "swap")
	// Desired is now T2 but applied still protects T1 until convergence.
	if err := deleteT1("del-t1-b", 1); !errors.Is(err, application.ErrResourceInUse) {
		t.Fatalf("union protection lost mid-update: %v", err)
	}
	drainLimit(t, h.instance, 64)
	applied := h.edges(t, appliedOf("union-user"))
	if len(applied) != 1 || applied[0].TargetID != "union-t2" {
		t.Fatalf("applied after convergence = %+v, want [union-t2]", applied)
	}
	if err := deleteT1("del-t1-c", 1); err != nil {
		t.Fatalf("released T1 delete failed after convergence: %v", err)
	}
	if _, err := h.service.AdmitDeleteResource(context.Background(), application.DeleteResourceCommand{
		Actor: applicationfake.Principal("tester"), ID: "union-t2", ExpectedGeneration: t2.Resource.Resource.Generation(),
		OperationID: "op-del-t2", EventID: "evt-del-t2", RequestedAt: testTime.Add(2 * time.Hour), IdempotencyKey: "key-del-t2",
	}); !errors.Is(err, application.ErrResourceInUse) {
		t.Fatalf("T2 should now be protected by its own applied edge: %v", err)
	}
	_ = t1
}

// TestM21WakeIdentityVersionedAndReusable pins Correction 2: versioned wake
// identities remain mintable forever across completed history, the finalizer
// handshake follows target version advancement, and concurrent wakes for one
// target coalesce behind the single active row.
func TestM21WakeIdentityVersionedAndReusable(t *testing.T) {
	h := newM21Harness(t)
	// Waiterless flows produce zero WakeDependents rows.
	h.create(t, "plain-a", nil)
	h.create(t, "plain-b", nil)
	drainLimit(t, h.instance, 32)
	if count := h.wakeCount(t); count != 0 {
		t.Fatalf("%d wake rows produced for waiterless resources", count)
	}

	record, err := h.store.GetResource(context.Background(), "plain-a")
	if err != nil {
		t.Fatal(err)
	}
	staleVersion := record.Version - 1
	if staleVersion == 0 {
		staleVersion = record.Version // handshake no-op case still exercises reuse
	}
	// A stale wake: its finalizer handshake must observe the current target
	// version and schedule a fresh follow-up wake instead of losing anything.
	stale := application.WakeDependentsMessage("plain-a", staleVersion)
	err = h.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		return tx.Outbox().EnqueueWakeDependents(context.Background(), stale)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Process EXACTLY the stale wake: one RunOnce claims and finalizes it,
	// enqueueing (but not yet processing) the handshake follow-up.
	worked, err := h.instance.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !worked {
		t.Fatal("stale wake was not claimable")
	}

	var followUpExists bool
	err = h.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		ctx := context.Background()
		current, err := tx.Resources().GetResource(ctx, "plain-a")
		if err != nil {
			return err
		}
		summary, err := tx.Outbox().SummarizeWorkByResource(ctx, "plain-a")
		if err != nil {
			return err
		}
		for _, message := range summary.Active {
			if message.Kind == application.OutboxWakeDependents && message.ExpectedVersion == current.Version {
				followUpExists = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !followUpExists {
		t.Fatal("wake finalizer handshake did not schedule a fresh current-version wake")
	}
	if !followUpExists {
		t.Fatal("wake finalizer handshake did not schedule a fresh current-version wake")
	}

	// Coalescing: another version arriving while one wake is active folds
	// silently behind it instead of failing or doubling rows.
	before := h.activeWakeCount(t, "plain-a")
	coalesce := application.WakeDependentsMessage("plain-a", record.Version+100)
	err = h.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		return tx.Outbox().EnqueueWakeDependents(context.Background(), coalesce)
	})
	if err != nil {
		t.Fatalf("coalesced enqueue errored: %v", err)
	}
	if after := h.activeWakeCount(t, "plain-a"); after != before {
		t.Fatalf("coalesced enqueue changed active wake count from %d to %d", before, after)
	}
}

func (h *m21Harness) activeWakeCount(t *testing.T, id string) int {
	t.Helper()
	count := 0
	err := h.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		summary, err := tx.Outbox().SummarizeWorkByResource(context.Background(), domain.ResourceID(id))
		if err != nil {
			return err
		}
		for _, message := range summary.Active {
			if message.Kind == application.OutboxWakeDependents {
				count++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func (h *m21Harness) wakeCount(t *testing.T) int {
	t.Helper()
	count := 0
	err := h.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		summary, err := tx.Outbox().SummarizeWorkByResource(context.Background(), "plain-a")
		if err != nil {
			return err
		}
		for _, message := range summary.Active {
			if message.Kind == application.OutboxWakeDependents {
				count++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return count
}

func drainLimit(t *testing.T, instance *worker.Worker, limit int) {
	t.Helper()
	for i := range limit {
		worked, err := instance.RunOnce(context.Background())
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if !worked {
			return
		}
	}
}
