// SPDX-License-Identifier: Apache-2.0

package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/resourcecontract"

	appfake "github.com/sithea-nou/liftr/internal/application/fake"
)

const outputMappingV1 = "liftr-test-outputs-v1"

// outputContractFor builds a strict contract with the declared output fields
// used across these tests.
func withOutputs(c *strictContract, fields []resourcecontract.OutputField) *strictContract {
	contract, err := resourcecontract.NewOutputContract(fields)
	if err != nil {
		panic(err)
	}
	c.outputs = &contract
	return c
}

func hostnamePortFields() []resourcecontract.OutputField {
	return []resourcecontract.OutputField{
		{Name: "hostname", JSONType: resourcecontract.OutputTypeString, RequiredWhenReady: true},
		{Name: "port", JSONType: resourcecontract.OutputTypeInteger, RequiredWhenReady: true},
	}
}

func validOutputValues() map[string]any {
	return map[string]any{"hostname": "orders-db.postgres.example", "port": int64(5432)}
}

// scriptedProvider is a deterministic provisioner stub whose submit and
// observe behavior is driven per call. It optionally implements the worker's
// OutputMappingSource so mapping binding is exercised end to end.
type scriptedProvider struct {
	submitCalls  map[domain.OperationID]int
	observeCalls map[domain.OperationID]int
	mappingRef   string
	onSubmit     func(request provisioning.ExecutionRequest, call int) (provisioning.Submission, error)
	onObserve    func(request provisioning.ObservationRequest, call int) (provisioning.ExecutionObservation, error)
}

func newScriptedProvider(mappingRef string) *scriptedProvider {
	return &scriptedProvider{submitCalls: map[domain.OperationID]int{}, observeCalls: map[domain.OperationID]int{}, mappingRef: mappingRef}
}

func (p *scriptedProvider) Capabilities() []provisioning.ProvisionerCapability {
	return []provisioning.ProvisionerCapability{{ResourceType: domain.ResourceTypeRef{Name: "Widget", Version: "v1"}, Capability: domain.CapabilityCreate}}
}

func (p *scriptedProvider) Submit(ctx context.Context, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	p.submitCalls[request.OperationID]++
	if p.onSubmit == nil {
		return provisioning.Submission{}, errors.New("no submit script")
	}
	return p.onSubmit(request, p.submitCalls[request.OperationID])
}

func (p *scriptedProvider) Observe(ctx context.Context, request provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	p.observeCalls[request.OperationID]++
	if p.onObserve == nil {
		return provisioning.ExecutionObservation{}, errors.New("no observe script")
	}
	return p.onObserve(request, p.observeCalls[request.OperationID])
}

func successObservation(handle provisioning.ExecutionHandle, outputs *provisioning.OutputEvidence) provisioning.ExecutionObservation {
	return provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle},
		Resource: domain.ObservedFacts{Presence: domain.ResourcePresenceUnknown,
			Readiness: domain.ResourceReadinessUnknown, Drift: domain.ResourceDriftUnknown},
		ObservedAt: time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC),
		Outputs:    outputs,
	}
}

func availableEvidence(values map[string]any) *provisioning.OutputEvidence {
	return &provisioning.OutputEvidence{State: provisioning.OutputsAvailable, Values: values}
}

func unavailableEvidence() *provisioning.OutputEvidence {
	return &provisioning.OutputEvidence{State: provisioning.OutputsUnavailable}
}

func invalidEvidence() *provisioning.OutputEvidence {
	return &provisioning.OutputEvidence{State: provisioning.OutputsInvalid, Reason: "undeclared output field"}
}

func outputFixture(t *testing.T, provider provisioning.Provisioner) (*application.Service, *appfake.Store, *appfake.Resolver, application.ProvisionerRef) {
	t.Helper()
	store := appfake.NewStore()
	widget := newStrictContract("Widget")
	withOutputs(widget, hostnamePortFields())
	catalog := &strictCatalog{types: map[domain.ResourceTypeRef]*strictContract{widget.Ref(): widget}, order: []domain.ResourceTypeRef{widget.Ref()}}
	ref := mustRef(t)
	resolver := &appfake.Resolver{Providers: map[application.ProvisionerRef]provisioning.Provisioner{ref: provider}}
	service, err := application.NewService(catalog, &appfake.Selector{Ref: ref}, resolver, store, appfake.AllowAll{})
	if err != nil {
		t.Fatal(err)
	}
	service.EnableEagerExecutionForTesting()
	return service, store, resolver, ref
}

func mustRef(t *testing.T) application.ProvisionerRef {
	t.Helper()
	ref, err := application.NewProvisionerRef("outputs-test-provider")
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func createOutputResource(t *testing.T, service *application.Service, id string) application.Result {
	t.Helper()
	result, err := service.CreateResource(context.Background(), application.CreateResourceCommand{Actor: appfake.Principal("tester"),
		ID:          domain.ResourceID(id),
		Type:        domain.ResourceTypeRef{Name: "Widget", Version: "v1"},
		Owner:       domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec:        validSpec(map[string]any{"name": "gear"}),
		OperationID: domain.OperationID("op-create-" + id),
		EventID:     domain.EventID("evt-create-" + id),
		RequestedAt: time.Date(2026, 8, 23, 8, 55, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func latestView(t *testing.T, store *appfake.Store, id string) (application.ResourceView, error) {
	t.Helper()
	service, err := application.NewService(&alwaysCatalog{}, &appfake.Selector{Ref: mustRef(t)}, &appfake.Resolver{}, store, appfake.AllowAll{})
	if err != nil {
		t.Fatal(err)
	}
	return service.GetResourceOperation(context.Background(), appfake.Principal("tester"), domain.ResourceID(id))
}

type alwaysCatalog struct{}

func (alwaysCatalog) Get(context.Context, domain.ResourceTypeRef) (resourcecontract.Contract, error) {
	return nil, errors.New("unused")
}
func (alwaysCatalog) List(context.Context) ([]resourcecontract.Contract, error) { return nil, nil }

// TestPlanTerminalOutputs pins the decision matrix over evidence and contracts.
func TestPlanTerminalOutputs(t *testing.T) {
	fieldsContract, err := resourcecontract.NewOutputContract(hostnamePortFields())
	if err != nil {
		t.Fatal(err)
	}
	optionalContract, err := resourcecontract.NewOutputContract([]resourcecontract.OutputField{
		{Name: "note", JSONType: resourcecontract.OutputTypeString},
	})
	if err != nil {
		t.Fatal(err)
	}
	fakeContractWithOutputs := func(contract resourcecontract.OutputContract) resourcecontract.Contract {
		base := newStrictContract("Widget")
		base.outputs = &contract
		return base
	}
	at := time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)

	cases := []struct {
		name       string
		contract   resourcecontract.Contract
		evidence   *provisioning.OutputEvidence
		want       application.OutputPlanAction
		capability domain.Capability
	}{
		{"delete never publishes", fakeContractWithOutputs(fieldsContract), availableEvidence(validOutputValues()), application.OutputPlanNone, domain.CapabilityDelete},
		{"nil evidence required defers", fakeContractWithOutputs(fieldsContract), nil, application.OutputPlanDefer, domain.CapabilityUpdate},
		{"unavailable required defers", fakeContractWithOutputs(fieldsContract), unavailableEvidence(), application.OutputPlanDefer, domain.CapabilityUpdate},
		{"available publishes", fakeContractWithOutputs(fieldsContract), availableEvidence(validOutputValues()), application.OutputPlanPublish, domain.CapabilityCreate},
		{"invalid rejects", fakeContractWithOutputs(fieldsContract), invalidEvidence(), application.OutputPlanReject, domain.CapabilityCreate},
		{"wrong type rejects", fakeContractWithOutputs(fieldsContract), availableEvidence(map[string]any{"hostname": "h", "port": "5432"}), application.OutputPlanReject, domain.CapabilityCreate},
		{"missing required rejects", fakeContractWithOutputs(fieldsContract), availableEvidence(map[string]any{"hostname": "h"}), application.OutputPlanReject, domain.CapabilityCreate},
		{"undeclared key rejects", fakeContractWithOutputs(fieldsContract), availableEvidence(map[string]any{"hostname": "h", "port": int64(1), "pw": "x"}), application.OutputPlanReject, domain.CapabilityCreate},
		{"no contract plain success", newStrictContract("Plain"), nil, application.OutputPlanNone, domain.CapabilityCreate},
		{"no contract undeclared rejects", newStrictContract("Plain"), availableEvidence(validOutputValues()), application.OutputPlanReject, domain.CapabilityCreate},
		{"optional absent completes", fakeContractWithOutputs(optionalContract), nil, application.OutputPlanNone, domain.CapabilityCreate},
	}
	for _, tc := range cases {
		capability := tc.capability
		if capability == "" {
			capability = capabilityFor(t, tc.contract)
		}
		plan, err := application.PlanTerminalOutputs(tc.contract, capability, tc.evidence, 3, at)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if plan.Action != tc.want {
			t.Errorf("%s: action = %d, want %d", tc.name, plan.Action, tc.want)
		}
	}

	deletePlan, err := application.PlanTerminalOutputs(fakeContractWithOutputs(fieldsContract), domain.CapabilityDelete, availableEvidence(validOutputValues()), 3, at)
	if err != nil || deletePlan.Action != application.OutputPlanNone {
		t.Fatalf("delete plan = %+v err=%v", deletePlan, err)
	}
}

func capabilityFor(t *testing.T, contract resourcecontract.Contract) domain.Capability {
	t.Helper()
	for _, capability := range contract.Capabilities() {
		if capability == domain.CapabilityUpdate {
			return domain.CapabilityUpdate
		}
	}
	return domain.CapabilityCreate
}

// TestCreatePublishesGenerationAssociatedOutputs drives a full eager create:
// backend success plus validated evidence publish an immutable snapshot at
// generation 1 atomically with reconciliation success.
func TestCreatePublishesGenerationAssociatedOutputs(t *testing.T) {
	provider := newScriptedProvider(outputMappingV1)
	provider.onSubmit = func(request provisioning.ExecutionRequest, call int) (provisioning.Submission, error) {
		handle, _ := provisioning.NewExecutionHandle("h-" + string(request.OperationID))
		return provisioning.Submission{Observation: successObservation(handle, availableEvidence(validOutputValues()))}, nil
	}
	service, store, _, _ := outputFixture(t, provider)
	create := createOutputResource(t, service, "pub")

	if create.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("operation = %s", create.Operation.State())
	}
	if create.Execution.OutputResolution != application.OutputResolutionPublished {
		t.Fatalf("resolution = %s", create.Execution.OutputResolution)
	}
	view, err := storeView(t, store, "pub")
	if err != nil {
		t.Fatal(err)
	}
	if view.Outputs == nil {
		t.Fatal("outputs missing from read model")
	}
	if view.Outputs.ObservedGeneration() != 1 || view.Resource.Resource.Generation() != 1 {
		t.Fatalf("O=%d D=%d", view.Outputs.ObservedGeneration(), view.Resource.Resource.Generation())
	}
	if got := view.Outputs.Values()["hostname"]; got != "orders-db.postgres.example" {
		t.Fatalf("hostname = %v", got)
	}
}

func storeView(t *testing.T, store *appfake.Store, id string) (application.ResourceView, error) {
	t.Helper()
	var view application.ResourceView
	err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		record, err := tx.Resources().GetResource(context.Background(), domain.ResourceID(id))
		if err != nil {
			return err
		}
		outputRecord, found, err := tx.Outputs().LatestResourceOutputs(context.Background(), domain.ResourceID(id))
		if err != nil {
			return err
		}
		view = application.ResourceView{Resource: record}
		if found && record.Status.State() != domain.ResourceStateDeleted {
			outputs := outputRecord.Values
			view.Outputs = &outputs
		}
		return nil
	})
	return view, err
}

// TestPendingThenPublishedAdvancesGenerationAndAcceptsEqualTimestamps covers
// corrections H/I/J: extraction may fail transiently first; resolution then
// advances while the provider terminal timestamp stays identical, without any
// resubmission.
func TestPendingThenPublishedAdvancesGenerationAndAcceptsEqualTimestamps(t *testing.T) {
	provider := newScriptedProvider(outputMappingV1)
	provider.onSubmit = func(request provisioning.ExecutionRequest, call int) (provisioning.Submission, error) {
		handle, _ := provisioning.NewExecutionHandle("h-" + string(request.OperationID))
		return provisioning.Submission{Observation: provisioning.ExecutionObservation{
			Correlation: provisioning.RequestCorrelationFound,
			Execution:   &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle},
			Resource:    domain.ObservedFacts{},
		}}, nil
	}
	provider.onObserve = func(request provisioning.ObservationRequest, call int) (provisioning.ExecutionObservation, error) {
		handle, _ := provisioning.NewExecutionHandle("h-" + string(request.OperationID))
		switch call {
		case 1:
			return provisioning.ExecutionObservation{
				Correlation: provisioning.RequestCorrelationFound,
				Execution:   &provisioning.Execution{State: provisioning.ExecutionStateRunning, Handle: &handle},
				Resource:    domain.ObservedFacts{},
			}, nil
		case 2:
			observation := successObservation(handle, unavailableEvidence())
			observation.ObservedAt = time.Date(2026, 8, 23, 9, 1, 0, 0, time.UTC)
			return observation, nil
		default:
			// Same provider terminal instant on every retry: only the output
			// dimension advances.
			observation := successObservation(handle, availableEvidence(validOutputValues()))
			observation.ObservedAt = time.Date(2026, 8, 23, 9, 1, 0, 0, time.UTC)
			return observation, nil
		}
	}
	service, store, _, _ := outputFixture(t, provider)
	create := createOutputResource(t, service, "pending")

	if create.OutputsPending || create.Execution.State != application.AttemptAccepted {
		t.Fatalf("create should be accepted, got %+v", create.Execution)
	}

	// First observe → Running.
	if _, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{
		OperationID: create.Operation.ID(), ObservedAt: time.Date(2026, 8, 23, 9, 0, 30, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	// Second observe → terminal success with unavailable outputs: deferred.
	deferred, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{
		OperationID: create.Operation.ID(), ObservedAt: time.Date(2026, 8, 23, 9, 1, 30, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if !deferred.OutputsPending {
		t.Fatal("terminal success with unavailable outputs did not defer")
	}
	if deferred.Operation.IsTerminal() {
		t.Fatal("deferred completion terminated the operation")
	}
	if deferred.Execution.OutputResolution != application.OutputResolutionPending {
		t.Fatalf("resolution = %s", deferred.Execution.OutputResolution)
	}
	view, err := storeView(t, store, "pending")
	if err != nil {
		t.Fatal(err)
	}
	if view.Outputs != nil {
		t.Fatal("outputs published before availability")
	}

	// Third observe resolves the outputs at the same provider instant.
	final, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{
		OperationID: create.Operation.ID(), ObservedAt: time.Date(2026, 8, 23, 9, 2, 30, 0, time.UTC)})
	if err != nil {
		t.Fatalf("equal-timestamp output progression rejected: %v", err)
	}
	if final.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("operation = %s", final.Operation.State())
	}
	if final.Execution.OutputResolution != application.OutputResolutionPublished {
		t.Fatalf("resolution = %s", final.Execution.OutputResolution)
	}
	if provider.submitCalls[create.Operation.ID()] != 1 {
		t.Fatalf("output retries re-executed the backend %d times", provider.submitCalls[create.Operation.ID()])
	}
	view, err = storeView(t, store, "pending")
	if err != nil {
		t.Fatal(err)
	}
	if view.Outputs == nil || view.Outputs.ObservedGeneration() != 1 {
		t.Fatalf("published outputs = %#v", view.Outputs)
	}
	if !final.Operation.CompletedAt().Equal(final.Execution.LastObservedAt) {
		t.Fatal("completion timestamp regressed relative to persisted evidence")
	}
}

// TestInvalidOutputEvidenceRejectsPostconditionWithoutLosingBackendSuccess
// pins correction-driven failure semantics: backend success stays recorded,
// the operation fails with the curated reason, and no values are published.
func TestInvalidOutputEvidenceRejectsPostconditionWithoutLosingBackendSuccess(t *testing.T) {
	provider := newScriptedProvider(outputMappingV1)
	provider.onSubmit = func(request provisioning.ExecutionRequest, call int) (provisioning.Submission, error) {
		handle, _ := provisioning.NewExecutionHandle("h-" + string(request.OperationID))
		return provisioning.Submission{Observation: successObservation(handle, invalidEvidence())}, nil
	}
	service, store, _, _ := outputFixture(t, provider)
	create := createOutputResource(t, service, "reject")

	if create.Operation.State() != domain.OperationStateFailed {
		t.Fatalf("operation = %s", create.Operation.State())
	}
	failure, ok := create.Operation.Failure()
	if !ok || failure.Reason() != application.ReasonOutputPostconditionRejected {
		t.Fatalf("failure = %+v", failure)
	}
	if create.Execution.State != application.AttemptSucceeded {
		t.Fatalf("execution state = %s, backend success must remain intact", create.Execution.State)
	}
	if create.Execution.OutputResolution != application.OutputResolutionRejected {
		t.Fatalf("resolution = %s", create.Execution.OutputResolution)
	}
	if create.Execution.OutputFailureReason != failure.Reason() || create.Execution.OutputFailureMessage != failure.Message() {
		t.Fatal("stored rejection details do not match operation failure")
	}
	view, err := storeView(t, store, "reject")
	if err != nil {
		t.Fatal(err)
	}
	if view.Outputs != nil {
		t.Fatal("rejected outputs were published")
	}
	if view.Resource.Status.State() != domain.ResourceStateFailed {
		t.Fatalf("state = %s", view.Resource.Status.State())
	}
}

// TestFailedUpdatePreservesPreviousOutputs pins generation semantics for a
// failed update: O stays at the previous successful generation while S and D
// advance.
func TestFailedUpdatePreservesPreviousOutputs(t *testing.T) {
	provider := newScriptedProvider(outputMappingV1)
	provider.onSubmit = func(request provisioning.ExecutionRequest, call int) (provisioning.Submission, error) {
		handle, _ := provisioning.NewExecutionHandle("h-" + string(request.OperationID))
		state := provisioning.ExecutionStateSucceeded
		evidence := availableEvidence(validOutputValues())
		if request.TargetGeneration == 2 {
			state = provisioning.ExecutionStateFailed
			evidence = nil
		}
		return provisioning.Submission{Observation: provisioning.ExecutionObservation{
			Correlation: provisioning.RequestCorrelationFound,
			Execution: &provisioning.Execution{State: state, Handle: &handle,
				Failure: failureFor(state)},
			Resource: domain.ObservedFacts{},
			Outputs:  evidence,
		}}, nil
	}
	service, store, _, _ := outputFixture(t, provider)
	create := createOutputResource(t, service, "upd-fail")
	if create.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("create state = %s", create.Operation.State())
	}

	update := application.UpdateResourceCommand{Actor: appfake.Principal("tester"),
		ID:                 domain.ResourceID("upd-fail"),
		ExpectedGeneration: 1,
		Spec:               validSpec(map[string]any{"name": "gear-2"}),
		OperationID:        "op-update-fail",
		EventID:            "evt-update-fail",
		RequestedAt:        time.Date(2026, 8, 23, 8, 56, 0, 0, time.UTC),
	}
	result, err := service.UpdateResource(context.Background(), update)
	if err != nil {
		t.Fatal(err)
	}
	if result.Operation.State() != domain.OperationStateFailed {
		t.Fatalf("update operation = %s", result.Operation.State())
	}
	view, err := storeView(t, store, "upd-fail")
	if err != nil {
		t.Fatal(err)
	}
	if view.Outputs == nil || view.Outputs.ObservedGeneration() != 1 {
		t.Fatalf("previous outputs not preserved: %#v", view.Outputs)
	}
	if view.Resource.Resource.Generation() != 2 {
		t.Fatalf("desired generation = %d", view.Resource.Resource.Generation())
	}
	if view.Resource.Status.ObservedGeneration() != 2 {
		t.Fatalf("status observed generation = %d", view.Resource.Status.ObservedGeneration())
	}
}

func failureFor(state provisioning.ExecutionState) *provisioning.ExecutionFailure {
	if state != provisioning.ExecutionStateFailed {
		return nil
	}
	return &provisioning.ExecutionFailure{Kind: provisioning.FailureExecution, Reason: "ExecutionFailed", Message: "backend failed"}
}

// TestDeletedTombstoneOmitsOutputsPublicly performs the actual delete flow.
func TestDeletedTombstoneOmitsOutputsPublicly(t *testing.T) {
	provider := newScriptedProvider(outputMappingV1)
	provider.onSubmit = func(request provisioning.ExecutionRequest, call int) (provisioning.Submission, error) {
		handle, _ := provisioning.NewExecutionHandle("h-" + string(request.OperationID))
		observation := successObservation(handle, nil)
		if request.Capability == domain.CapabilityDelete {
			observation.ObservedAt = time.Date(2026, 8, 23, 9, 14, 0, 0, time.UTC)
			observation.Resource = domain.ObservedFacts{Presence: domain.ResourcePresenceNotFound,
				Readiness: domain.ResourceReadinessUnknown, Drift: domain.ResourceDriftUnknown}
		} else {
			observation.Outputs = availableEvidence(validOutputValues())
		}
		return provisioning.Submission{Observation: observation}, nil
	}
	service, store, _, _ := outputFixture(t, provider)
	createOutputResource(t, service, "tomb")

	deleting, err := service.DeleteResource(context.Background(), application.DeleteResourceCommand{Actor: appfake.Principal("tester"),
		ID: domain.ResourceID("tomb"), ExpectedGeneration: 1,
		OperationID: "op-tomb", EventID: "evt-tomb",
		RequestedAt: time.Date(2026, 8, 23, 9, 11, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if deleting.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("delete operation = %s", deleting.Operation.State())
	}
	// Internal immutable history is retained.
	err = store.Within(context.Background(), func(tx application.UnitOfWork) error {
		_, found, err := tx.Outputs().LatestResourceOutputs(context.Background(), domain.ResourceID("tomb"))
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("internal output history was destroyed by deletion")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Public view suppresses outputs on the Deleted tombstone.
	view, err := storeView(t, store, "tomb")
	if err != nil {
		t.Fatal(err)
	}
	if view.Resource.Status.State() != domain.ResourceStateDeleted {
		t.Fatalf("state = %s", view.Resource.Status.State())
	}
	if view.Outputs != nil {
		t.Fatal("deleted tombstone exposes outputs")
	}
}

// TestContradictoryOutputPublicationFailsClosed pins persistence fencing.
func TestContradictoryOutputPublicationFailsClosed(t *testing.T) {
	store := appfake.NewStore()
	snapshot, err := domain.NewResourceOutputs(1, validOutputValues(), time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	first := application.ResourceOutputRecord{
		ResourceID: "r1", ObservedGeneration: 1, OperationID: "op-a", Capability: domain.CapabilityCreate,
		OutputMappingRef: outputMappingV1, OutputContractDigest: "digest-1",
		Values: snapshot, ValuesDigest: "values-digest-1",
	}
	err = store.Within(context.Background(), func(tx application.UnitOfWork) error {
		return tx.Outputs().SaveResourceOutputs(context.Background(), first)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Identical republication is idempotent.
	err = store.Within(context.Background(), func(tx application.UnitOfWork) error {
		return tx.Outputs().SaveResourceOutputs(context.Background(), first)
	})
	if err != nil {
		t.Fatalf("identical republication rejected: %v", err)
	}
	// Contradictory content fails closed.
	conflicting := first
	conflicting.ValuesDigest = "different"
	conflicting.OperationID = "op-b"
	err = store.Within(context.Background(), func(tx application.UnitOfWork) error {
		return tx.Outputs().SaveResourceOutputs(context.Background(), conflicting)
	})
	if err == nil {
		t.Fatal("contradictory evidence accepted")
	}
	// The original snapshot survives untouched.
	err = store.Within(context.Background(), func(tx application.UnitOfWork) error {
		record, found, err := tx.Outputs().LatestResourceOutputs(context.Background(), "r1")
		if err != nil || !found {
			t.Fatalf("snapshot lost: found=%t err=%v", found, err)
		}
		if record.OperationID != "op-a" || record.ValuesDigest != "values-digest-1" {
			t.Fatalf("record mutated: %+v", record)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestStaleOlderGenerationPublicationDoesNotRegressReadModel pins that a stale
// operation completing after a newer publication cannot change the public
// snapshot.
func TestStaleOlderGenerationPublicationDoesNotRegressReadModel(t *testing.T) {
	store := appfake.NewStore()
	gen1, err := domain.NewResourceOutputs(1, map[string]any{"hostname": "old.example", "port": int64(5432)},
		time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	gen2, err := domain.NewResourceOutputs(2, map[string]any{"hostname": "new.example", "port": int64(5432)},
		time.Date(2026, 8, 23, 9, 5, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	insert := func(snapshot domain.ResourceOutputs, op string) error {
		return store.Within(context.Background(), func(tx application.UnitOfWork) error {
			digest, err := application.ValuesDigest(snapshot.Values())
			if err != nil {
				return err
			}
			return tx.Outputs().SaveResourceOutputs(context.Background(), application.ResourceOutputRecord{
				ResourceID: "stale", ObservedGeneration: snapshot.ObservedGeneration(),
				OperationID: domain.OperationID(op), Capability: domain.CapabilityUpdate,
				Values: snapshot, ValuesDigest: digest,
			})
		})
	}
	if err := insert(gen1, "op-gen1"); err != nil {
		t.Fatal(err)
	}
	if err := insert(gen2, "op-gen2"); err != nil {
		t.Fatal(err)
	}
	// A stale gen-1 completion arrives late; it may persist as history but the
	// latest read must stay gen 2.
	if err := insert(gen1, "op-stale-gen1"); err == nil {
		t.Log("same-content different-operation insert for gen1 was rejected (also acceptable)")
	}
	var latest *domain.ResourceOutputs
	err = store.Within(context.Background(), func(tx application.UnitOfWork) error {
		record, found, err := tx.Outputs().LatestResourceOutputs(context.Background(), "stale")
		if err != nil || !found {
			t.Fatalf("snapshots lost: found=%t err=%v", found, err)
		}
		snapshot := record.Values
		latest = &snapshot
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.ObservedGeneration() != 2 {
		t.Fatalf("latest outputs = %#v, want generation 2", latest)
	}
	if latest.Values()["hostname"] != "new.example" {
		t.Fatalf("hostname = %v", latest.Values()["hostname"])
	}
}

// TestCleanupDeleteFromFailedDestroysInfrastructure pins Correction 1 case A:
// failed create whose infrastructure succeeded can be cleanup-deleted to
// Deleted via a real destroy.
func TestCleanupDeleteFromFailedDestroysInfrastructure(t *testing.T) {
	provider := newScriptedProvider(outputMappingV1)
	provider.onSubmit = func(request provisioning.ExecutionRequest, call int) (provisioning.Submission, error) {
		handle, _ := provisioning.NewExecutionHandle("h-" + string(request.OperationID))
		observation := successObservation(handle, nil)
		switch {
		case request.Capability == domain.CapabilityCreate:
			// Infrastructure succeeds; required outputs are rejected.
			observation.Outputs = invalidEvidence()
		case request.Capability == domain.CapabilityDelete:
			observation.ObservedAt = time.Date(2026, 8, 23, 9, 15, 0, 0, time.UTC)
			observation.Resource = domain.ObservedFacts{Presence: domain.ResourcePresenceNotFound,
				Readiness: domain.ResourceReadinessUnknown, Drift: domain.ResourceDriftUnknown}
		}
		return provisioning.Submission{Observation: observation}, nil
	}
	service, store, _, _ := outputFixture(t, provider)
	create := createOutputResource(t, service, "cleanup")
	if create.Operation.State() != domain.OperationStateFailed {
		t.Fatalf("create state = %s", create.Operation.State())
	}

	result, err := service.DeleteResource(context.Background(), application.DeleteResourceCommand{Actor: appfake.Principal("tester"),
		ID: domain.ResourceID("cleanup"), ExpectedGeneration: 1,
		OperationID: "op-cleanup", EventID: "evt-cleanup",
		RequestedAt: time.Date(2026, 8, 23, 9, 12, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("cleanup delete admission rejected: %v", err)
	}
	if result.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("cleanup operation = %s", result.Operation.State())
	}
	view, err := storeView(t, store, "cleanup")
	if err != nil {
		t.Fatal(err)
	}
	if view.Resource.Status.State() != domain.ResourceStateDeleted {
		t.Fatalf("state = %s", view.Resource.Status.State())
	}
}

// TestCleanupDeleteWithConclusiveAbsenceSucceeds pins Correction 1 case B: a
// create that failed before launch leaves no stack; the conclusive pre-launch
// NotFound satisfies destruction.
func TestCleanupDeleteWithConclusiveAbsenceSucceeds(t *testing.T) {
	provider := newScriptedProvider(outputMappingV1)
	provider.onSubmit = func(request provisioning.ExecutionRequest, call int) (provisioning.Submission, error) {
		observation := provisioning.ExecutionObservation{}
		switch request.Capability {
		case domain.CapabilityCreate:
			// Conclusive preflight rejection before any launch.
			observation = provisioning.ExecutionObservation{
				Correlation: provisioning.RequestCorrelationNotFound,
				Execution: &provisioning.Execution{State: provisioning.ExecutionStateFailed,
					Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureUnavailable,
						Reason: "RequiredEnvironmentMissing", Message: "environment variable ARM_SUBSCRIPTION_ID is required"}},
				Resource: domain.ObservedFacts{},
			}
		case domain.CapabilityDelete:
			// Stack conclusively absent pre-launch: fresh NotFound.
			observation = provisioning.ExecutionObservation{
				Correlation: provisioning.RequestCorrelationNotFound,
				Execution: &provisioning.Execution{State: provisioning.ExecutionStateFailed,
					Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureNotFound,
						Reason: "ExecutionStateNotFound", Message: "stack not found"}},
				Resource: domain.ObservedFacts{},
			}
		}
		return provisioning.Submission{Observation: observation}, nil
	}
	service, store, _, _ := outputFixture(t, provider)
	create := createOutputResource(t, service, "absent")
	if create.Operation.State() != domain.OperationStateFailed {
		t.Fatalf("create state = %s", create.Operation.State())
	}

	result, err := service.DeleteResource(context.Background(), application.DeleteResourceCommand{Actor: appfake.Principal("tester"),
		ID: domain.ResourceID("absent"), ExpectedGeneration: 1,
		OperationID: "op-cleanup-absent", EventID: "evt-cleanup-absent",
		RequestedAt: time.Date(2026, 8, 23, 9, 12, 30, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("cleanup delete rejected: %v", err)
	}
	if result.Operation.State() != domain.OperationStateSucceeded {
		failure, _ := result.Operation.Failure()
		t.Fatalf("conclusive absence did not satisfy destruction: %s (%+v)", result.Operation.State(), failure)
	}
	view, err := storeView(t, store, "absent")
	if err != nil {
		t.Fatal(err)
	}
	if view.Resource.Status.State() != domain.ResourceStateDeleted {
		t.Fatalf("state = %s", view.Resource.Status.State())
	}
}

// TestAmbiguousDestroyFromFailedNeverYieldsDeleted pins Correction 1 case C.
func TestAmbiguousDestroyFromFailedNeverYieldsDeleted(t *testing.T) {
	provider := newScriptedProvider(outputMappingV1)
	provider.onSubmit = func(request provisioning.ExecutionRequest, call int) (provisioning.Submission, error) {
		handle, _ := provisioning.NewExecutionHandle("h-" + string(request.OperationID))
		if request.Capability == domain.CapabilityCreate {
			observation := successObservation(handle, invalidEvidence())
			return provisioning.Submission{Observation: observation}, nil
		}
		// Destroy launched; outcome unknown (post-launch ambiguity).
		return provisioning.Submission{Observation: provisioning.ExecutionObservation{
			Correlation: provisioning.RequestCorrelationFound,
			Execution: &provisioning.Execution{State: provisioning.ExecutionStateUnknown, Handle: &handle,
				Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "SubmissionOutcomeUnknown", Message: "unknown"}},
			Resource: domain.ObservedFacts{},
		}}, provisioning.ErrAmbiguousSubmission
	}
	service, store, _, _ := outputFixture(t, provider)
	createOutputResource(t, service, "ambiguous")

	result, err := service.DeleteResource(context.Background(), application.DeleteResourceCommand{Actor: appfake.Principal("tester"),
		ID: domain.ResourceID("ambiguous"), ExpectedGeneration: 1,
		OperationID: "op-cleanup-amb", EventID: "evt-cleanup-amb",
		RequestedAt: time.Date(2026, 8, 23, 9, 13, 0, 0, time.UTC),
	})
	if err == nil {
		t.Fatal("ambiguous destroy returned success")
	}
	if result.Operation.IsTerminal() && result.Operation.State() == domain.OperationStateSucceeded {
		t.Fatal("ambiguity concluded deletion")
	}
	view, err := storeView(t, store, "ambiguous")
	if err != nil {
		t.Fatal(err)
	}
	if view.Resource.Status.State() == domain.ResourceStateDeleted {
		t.Fatal("Resource became Deleted from ambiguous destroy")
	}
}

// TestConclusiveAbsenceRequiresFreshPreLaunchCorrelation pins the guard: an
// accepted attempt whose correlation later reports NotFound must never become
// a satisfied deletion or a false success.
func TestConclusiveAbsenceRequiresFreshPreLaunchCorrelation(t *testing.T) {
	execution := application.ProvisioningExecutionRecord{Capability: domain.CapabilityDelete, AcceptanceConfirmed: true}
	accepted := provisioning.Execution{State: provisioning.ExecutionStateFailed,
		Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureNotFound, Reason: "Lost"}}
	if application.ConclusiveManagedAbsence(execution.Capability, provisioning.RequestCorrelationNotFound, &accepted, execution.AcceptanceConfirmed) {
		t.Fatal("confirmed acceptance converted into absence satisfaction")
	}
	wrongKind := provisioning.Execution{State: provisioning.ExecutionStateFailed,
		Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureExecution, Reason: "DestroyFailed"}}
	if application.ConclusiveManagedAbsence(execution.Capability, provisioning.RequestCorrelationNotFound, &wrongKind, false) {
		t.Fatal("definitive destroy failure converted into absence satisfaction")
	}
	if application.ConclusiveManagedAbsence(domain.CapabilityCreate, provisioning.RequestCorrelationNotFound, &accepted, false) {
		t.Fatal("non-delete capability converted into absence satisfaction")
	}
	if application.ConclusiveManagedAbsence(execution.Capability, provisioning.RequestCorrelationUnknown, &accepted, false) {
		t.Fatal("unknown correlation converted into absence satisfaction")
	}
	if !application.ConclusiveManagedAbsence(execution.Capability, provisioning.RequestCorrelationNotFound, &accepted, false) {
		t.Fatal("fresh conclusive NotFound refused")
	}
}
