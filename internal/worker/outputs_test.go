// SPDX-License-Identifier: Apache-2.0

package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
	"github.com/sithea-nou/liftr/internal/worker"

	applicationfake "github.com/sithea-nou/liftr/internal/application/fake"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
)

const (
	firstMapping  = "liftr-outputs-v1"
	renewMapping  = "liftr-outputs-v2"
	outputsExport = "liftrOutputs"
)

// outputsCatalog serves one contract that declares required hostname/port
// outputs so the worker must resolve the output dimension before completion.
type outputsCatalog struct {
	inner applicationfake.Catalog
}

func newOutputsCatalog(t *testing.T) *outputsCatalog {
	t.Helper()
	typeValue, err := domain.NewResourceType(provisioningfake.ResourceType(), "worker outputs resource",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		t.Fatal(err)
	}
	return &outputsCatalog{inner: applicationfake.Catalog{Types: map[domain.ResourceTypeRef]domain.ResourceType{
		provisioningfake.ResourceType(): typeValue,
	}}}
}

func (c *outputsCatalog) Get(_ context.Context, ref domain.ResourceTypeRef) (resourcecontract.Contract, error) {
	typeValue, ok := c.inner.Types[ref]
	if !ok {
		return nil, errors.New("unknown resource type")
	}
	fields, err := resourcecontract.NewOutputContract([]resourcecontract.OutputField{
		{Name: "hostname", JSONType: resourcecontract.OutputTypeString, RequiredWhenReady: true},
		{Name: "port", JSONType: resourcecontract.OutputTypeInteger, RequiredWhenReady: true},
	})
	if err != nil {
		return nil, err
	}
	return mappedContract{resourceType: typeValue, fields: fields}, nil
}

func (c *outputsCatalog) List(ctx context.Context) ([]resourcecontract.Contract, error) {
	contracts := make([]resourcecontract.Contract, 0, len(c.inner.Types))
	for ref := range c.inner.Types {
		contract, err := c.Get(ctx, ref)
		if err != nil {
			return nil, err
		}
		contracts = append(contracts, contract)
	}
	return contracts, nil
}

type mappedContract struct {
	resourceType domain.ResourceType
	fields       resourcecontract.OutputContract
}

func (c mappedContract) Ref() domain.ResourceTypeRef                      { return c.resourceType.Ref() }
func (c mappedContract) DisplayName() string                              { return c.Ref().Name }
func (c mappedContract) Description() string                              { return "" }
func (c mappedContract) Capabilities() []domain.Capability                { return c.resourceType.Capabilities() }
func (c mappedContract) Domain() domain.ResourceType                      { return c.resourceType }
func (c mappedContract) SpecSchema() json.RawMessage                      { return json.RawMessage(`{"type":"object"}`) }
func (c mappedContract) OutputContract() *resourcecontract.OutputContract { return &c.fields }

// ReferenceContract is nil by default in the output-mapping test contract.
func (c mappedContract) ReferenceContract() *resourcecontract.ReferenceContract { return nil }
func (mappedContract) ValidateSpec(domain.ResourceSpec) error                   { return nil }
func (mappedContract) ValidateUpdate(_, _ domain.ResourceSpec) error            { return nil }

// mappingProvider models a provisioner whose registered output mapping can
// change across deployments. It records every requested mapping identity and
// refuses observations whose persisted ref it cannot resolve — recovery never
// falls back to whatever happens to be registered now.
type mappingProvider struct {
	mu                  sync.Mutex
	mappingRef          string
	submissions         map[domain.OperationID]int
	requestedRefs       []string
	requestedSourceRefs []string
	observeCalls        map[domain.OperationID]int
	failUnknownRef      bool
	outputMapping       string
	invalidOutputs      bool
	recoveryFailed      bool
	recoverySource      string
	recoveryMapping     string
	observedBase        time.Time
	requestedOperations []domain.OperationID
	mappingProbe        func()
}

func newMappingProvider(mappingRef string, failUnknownRef bool) *mappingProvider {
	return &mappingProvider{mappingRef: mappingRef, submissions: map[domain.OperationID]int{},
		observeCalls: map[domain.OperationID]int{}, failUnknownRef: failUnknownRef, observedBase: testTime}
}

func (p *mappingProvider) Capabilities() []provisioning.ProvisionerCapability {
	return []provisioning.ProvisionerCapability{{ResourceType: provisioningfake.ResourceType(), Capability: domain.CapabilityCreate}}
}

// OutputMappingRef declares this deployment's mapping identity.
func (p *mappingProvider) OutputMappingRef(domain.ResourceTypeRef, domain.Capability) string {
	if p.mappingProbe != nil {
		p.mappingProbe()
	}
	return p.mappingRef
}

func (p *mappingProvider) SelectOutputRecoveryMapping(_ domain.ResourceTypeRef, capability domain.Capability, source string) (string, bool) {
	if capability == domain.CapabilityDelete || source == "" || source != p.recoverySource || p.recoveryMapping == "" || p.recoveryMapping == source {
		return "", false
	}
	return p.recoveryMapping, true
}

func (p *mappingProvider) handle(operationID domain.OperationID) provisioning.ExecutionObservation {
	handle, _ := provisioning.NewExecutionHandle("h-" + string(operationID))
	return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle},
		Resource:  readyFacts()}
}

func (p *mappingProvider) Submit(_ context.Context, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	p.mu.Lock()
	p.submissions[request.OperationID]++
	p.requestedRefs = append(p.requestedRefs, request.OutputMappingRef)
	p.mu.Unlock()
	observation := p.handle(request.OperationID)
	observation.Execution.State = provisioning.ExecutionStateAccepted
	return provisioning.Submission{Observation: observation}, nil
}

func (p *mappingProvider) Observe(_ context.Context, request provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	p.mu.Lock()
	p.observeCalls[request.OperationID]++
	call := p.observeCalls[request.OperationID]
	p.requestedRefs = append(p.requestedRefs, request.OutputMappingRef)
	p.requestedSourceRefs = append(p.requestedSourceRefs, request.OutputSourceMappingRef)
	p.requestedOperations = append(p.requestedOperations, request.OperationID)
	ref := request.OutputMappingRef
	failUnknown := p.failUnknownRef && ref != "" && ref != p.mappingRef
	p.mu.Unlock()

	if failUnknown {
		return provisioning.ExecutionObservation{}, provisioning.ObservationError{Failure: provisioning.ExecutionFailure{
			Kind: provisioning.FailureUnknown, Reason: "OutputMappingMissing",
			Message: "execution references an unresolvable output mapping"}}
	}
	observation := p.handle(request.OperationID)
	observation.Execution.State = provisioning.ExecutionStateSucceeded
	if p.recoveryFailed && request.OutputSourceMappingRef != "" {
		observation.Execution.State = provisioning.ExecutionStateFailed
		observation.Execution.Failure = &provisioning.ExecutionFailure{Kind: provisioning.FailureExecution,
			Reason: "ProviderSpecificFailure", Message: "private provider failure detail"}
		observation.ObservedAt = p.observedBase.Add(time.Duration(call) * time.Minute)
		return observation, nil
	}
	// Each observation advances the provider timeline so evidence freshness
	// stays honest; only the OUTPUT dimension may repeat timestamps.
	observation.ObservedAt = p.observedBase.Add(time.Duration(call) * time.Minute)
	switch {
	case call <= 2:
		observation.Execution.State = provisioning.ExecutionStateRunning
	case call == 3:
		observation.Outputs = &provisioning.OutputEvidence{State: provisioning.OutputsUnavailable}
	default:
		values := map[string]any{"hostname": "db.example", "port": int64(5432)}
		if p.invalidOutputs {
			values = map[string]any{"hostname": "db.example"}
		}
		observation.Outputs = &provisioning.OutputEvidence{State: provisioning.OutputsAvailable, Values: values, OutputMappingRef: p.outputMapping}
	}
	return observation, nil
}

func (p *mappingProvider) submissionCount(operationID domain.OperationID) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.submissions[operationID]
}

func outputsHarness(t *testing.T, provider *mappingProvider) (*application.Service, *applicationfake.Store, *applicationfake.Resolver, application.ProvisionerRef, *worker.Worker) {
	t.Helper()
	ref, err := application.NewProvisionerRef("mapping-provider")
	if err != nil {
		t.Fatal(err)
	}
	store := applicationfake.NewStore()
	selector := &applicationfake.Selector{Ref: ref}
	resolver := &applicationfake.Resolver{Providers: map[application.ProvisionerRef]provisioning.Provisioner{ref: provider}}
	catalog := newOutputsCatalog(t)
	service, err := application.NewService(catalog, selector, resolver, store, applicationfake.AllowAll{})
	if err != nil {
		t.Fatal(err)
	}
	instance, err := worker.NewWithCatalog(store, resolver, catalog)
	if err != nil {
		t.Fatal(err)
	}
	instance.RetryBase = 0
	instance.Clock = func() time.Time { return testTime.Add(time.Minute) }
	return service, store, resolver, ref, instance
}

func pumpOnce(t *testing.T, instance *worker.Worker) error {
	t.Helper()
	worked, err := instance.RunOnce(context.Background())
	if err != nil {
		return err
	}
	if !worked {
		return errDrained{}
	}
	return nil
}

type transactionProbeRunner struct {
	inner  application.TransactionRunner
	active atomic.Int32
}

func (r *transactionProbeRunner) Within(ctx context.Context, fn func(application.UnitOfWork) error) error {
	return r.inner.Within(ctx, func(tx application.UnitOfWork) error {
		r.active.Add(1)
		defer r.active.Add(-1)
		return fn(tx)
	})
}

func TestOutputMappingCallbackRunsOutsideDispatchTransaction(t *testing.T) {
	provider := newMappingProvider(firstMapping, false)
	service, store, _, _, instance := outputsHarness(t, provider)
	probe := &transactionProbeRunner{inner: store}
	instance.Transactions = probe
	var calls atomic.Int32
	var calledInTransaction atomic.Bool
	provider.mappingProbe = func() {
		calls.Add(1)
		if probe.active.Load() != 0 {
			calledInTransaction.Store(true)
		}
	}
	command := createCommand(t, "mapping-callback-db", "op-mapping-callback")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
			t.Fatalf("worker setup worked=%t error=%v", worked, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("OutputMappingRef calls=%d, want 1", calls.Load())
	}
	if calledInTransaction.Load() {
		t.Fatal("OutputMappingRef was called inside a database transaction")
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.OutputMappingRef != firstMapping {
		t.Fatalf("execution error=%v mapping=%q", err, execution.OutputMappingRef)
	}
}

type errDrained struct{}

func (errDrained) Error() string { return "drained" }

// TestInFlightExecutionSurvivesRestartWithOriginalMapping proves an ordinary
// accepted execution, before backend success or output deferral, keeps the
// mapping bound at dispatch across a deployment restart. Submit and every
// recovery observation carry the persisted source ref, never the new current.
func TestInFlightExecutionSurvivesRestartWithOriginalMapping(t *testing.T) {
	provider := newMappingProvider(firstMapping, false)
	service, store, resolver, ref, firstWorker := outputsHarness(t, provider)

	admitted, err := service.AdmitCreateResource(context.Background(), createCommand(t, "restart-db", "op-restart"))
	if err != nil {
		t.Fatal(err)
	}
	for range 16 {
		if provider.submissionCount(admitted.Operation.ID()) == 1 {
			break
		}
		if err := pumpOnce(t, firstWorker); err != nil {
			t.Fatal(err)
		}
	}
	if got := provider.submissionCount(admitted.Operation.ID()); got != 1 {
		t.Fatalf("pre-restart submissions = %d, want 1", got)
	}

	record, err := store.GetExecution(context.Background(), admitted.Operation.ID())
	if err != nil {
		t.Fatal(err)
	}
	if record.State != application.AttemptAccepted || record.OutputResolution == application.OutputResolutionPending ||
		record.OutputResolution == application.OutputResolutionPublished || record.OutputResolution == application.OutputResolutionRejected {
		t.Fatalf("pre-restart state=%s resolution=%s", record.State, record.OutputResolution)
	}
	if record.OutputMappingRef != firstMapping {
		t.Fatalf("persisted mapping = %q", record.OutputMappingRef)
	}
	if got := readRequestedRefs(provider); len(got) != 1 || got[0] != firstMapping {
		t.Fatalf("submit mapping refs = %v, want [%s]", got, firstMapping)
	}

	// Restart with a newer current mapping. Ordinary recovery must still carry
	// and publish through v1; selecting a compatible repair is reserved for an
	// explicitly admitted retry operation.
	newProvider := newMappingProvider(renewMapping, false)
	newProvider.outputMapping = firstMapping
	resolver.Providers[ref] = newProvider
	restarted, err := worker.NewWithCatalog(store, resolver, newOutputsCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	restarted.RetryBase = 0
	drain(t, restarted, 64)

	view := readView(t, store, "restart-db")
	if view.Resource.Status.State() != domain.ResourceStateReady || view.Outputs == nil {
		t.Fatalf("post-restart state=%s outputs=%#v", view.Resource.Status.State(), view.Outputs)
	}
	if got := newProvider.submissionCount(admitted.Operation.ID()); got != 0 {
		t.Fatalf("restart triggered %d backend submissions", got)
	}
	requestedRefs := readRequestedRefs(newProvider)
	if len(requestedRefs) == 0 {
		t.Fatal("restarted worker made no recovery observations")
	}
	for i, requested := range requestedRefs {
		if requested != firstMapping {
			t.Fatalf("restart request carried %q instead of persisted %q; current is %q", requested, firstMapping, renewMapping)
		}
		if newProvider.requestedSourceRefs[i] != "" {
			t.Fatalf("ordinary restart observation carried source mapping %q", newProvider.requestedSourceRefs[i])
		}
	}
	final, err := store.GetExecution(context.Background(), admitted.Operation.ID())
	if err != nil {
		t.Fatal(err)
	}
	if final.OutputMappingRef != firstMapping {
		t.Fatalf("post-restart persisted mapping = %q, want %q", final.OutputMappingRef, firstMapping)
	}
	var published application.ResourceOutputRecord
	err = store.Within(context.Background(), func(tx application.UnitOfWork) error {
		var found bool
		var loadErr error
		published, found, loadErr = tx.Outputs().LatestResourceOutputs(context.Background(), "restart-db")
		if loadErr != nil {
			return loadErr
		}
		if !found {
			return errors.New("published outputs not found")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if published.OutputMappingRef != firstMapping {
		t.Fatalf("published mapping provenance = %q, want persisted mapping %q", published.OutputMappingRef, firstMapping)
	}
}

func TestWorkerRejectsOutputEvidenceMappingThatContradictsFrozenExecution(t *testing.T) {
	provider := newMappingProvider(firstMapping, false)
	provider.outputMapping = renewMapping
	service, store, _, _, instance := outputsHarness(t, provider)
	admitted, err := service.AdmitCreateResource(context.Background(), createCommand(t, "mapping-conflict-db", "op-worker-mapping-conflict"))
	if err != nil {
		t.Fatal(err)
	}
	var got error
	for range 32 {
		_, got = instance.RunOnce(context.Background())
		if got != nil {
			break
		}
	}
	if !errors.Is(got, application.ErrInvalidApplicationCall) {
		t.Fatalf("mapping conflict error = %v", got)
	}
	execution, err := store.GetExecution(context.Background(), admitted.Operation.ID())
	if err != nil {
		t.Fatal(err)
	}
	if execution.OutputMappingRef != firstMapping {
		t.Fatalf("frozen mapping changed to %q", execution.OutputMappingRef)
	}
	view := readView(t, store, "mapping-conflict-db")
	if view.Outputs != nil {
		t.Fatal("contradictory worker output evidence was published")
	}
}

func TestExplicitRetryRecoversRejectedOutputsWithoutSubmitting(t *testing.T) {
	v1 := newMappingProvider(firstMapping, false)
	v1.outputMapping = firstMapping
	v1.invalidOutputs = true
	service, store, resolver, ref, firstWorker := outputsHarness(t, v1)

	admitted, err := service.AdmitCreateResource(context.Background(), createCommand(t, "recover-db", "op-output-v1"))
	if err != nil {
		t.Fatal(err)
	}
	drain(t, firstWorker, 64)
	source, err := store.GetOperation(context.Background(), admitted.Operation.ID())
	if err != nil {
		t.Fatal(err)
	}
	if source.Operation.State() != domain.OperationStateFailed {
		t.Fatalf("source state = %s", source.Operation.State())
	}
	failure, _ := source.Operation.Failure()
	if failure.Reason() != application.ReasonOutputPostconditionRejected {
		t.Fatalf("source failure = %q", failure.Reason())
	}
	sourceExecution, err := store.GetExecution(context.Background(), source.Operation.ID())
	if err != nil {
		t.Fatal(err)
	}
	if sourceExecution.State != application.AttemptSucceeded || sourceExecution.OutputResolution != application.OutputResolutionRejected || sourceExecution.CurrentAttempt == 0 {
		t.Fatalf("source execution = %+v", sourceExecution)
	}

	withoutRepair := newMappingProvider(renewMapping, false)
	resolver.Providers[ref] = withoutRepair
	before := store.RecordCounts()
	retryCommand := application.RetryOperationCommand{
		Actor: applicationfake.Principal("tester"), OperationID: source.Operation.ID(),
		ExpectedGeneration: source.Operation.TargetGeneration(), NewOperationID: "op-output-v2",
		EventID: "event-op-output-v2", RequestedAt: testTime.Add(10 * time.Minute), IdempotencyKey: "retry-output-key",
	}
	if _, err := service.AdmitRetryOperation(context.Background(), retryCommand); !errors.Is(err, application.ErrProvisionerNotFound) {
		t.Fatalf("retry without compatible mapping error = %v", err)
	}
	if after := store.RecordCounts(); after != before {
		t.Fatalf("rejected retry changed durable counts: before=%+v after=%+v", before, after)
	}
	// A historically ambiguous attempt is still safe when the execution has
	// durable, positively correlated terminal success and confirmed acceptance.
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		attempt, err := tx.SubmissionAttempts().GetSubmissionAttempt(context.Background(), sourceExecution.OperationID, sourceExecution.CurrentAttempt)
		if err != nil {
			return err
		}
		attempt.State = application.SubmissionAttemptUnknown
		return tx.SubmissionAttempts().SaveSubmissionAttempt(context.Background(), attempt, application.SubmissionAttemptAccepted)
	}); err != nil {
		t.Fatal(err)
	}

	v2 := newMappingProvider(renewMapping, false)
	v2.outputMapping = renewMapping
	v2.recoverySource = firstMapping
	v2.recoveryMapping = renewMapping
	v2.observedBase = testTime.Add(10 * time.Minute)
	resolver.Providers[ref] = v2
	retry, err := service.AdmitRetryOperation(context.Background(), retryCommand)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Execution == nil || retry.Execution.OutputMappingRef != renewMapping ||
		retry.Execution.RecoverySourceOperationID != source.Operation.ID() ||
		retry.Execution.RecoverySourceAttempt != sourceExecution.CurrentAttempt ||
		retry.Execution.CurrentAttempt != 0 || retry.Execution.State != application.AttemptSucceeded ||
		retry.Execution.OutputResolution != application.OutputResolutionPending {
		t.Fatalf("recovery execution = %+v", retry.Execution)
	}
	replay, err := service.AdmitRetryOperation(context.Background(), retryCommand)
	if err != nil || !replay.Replay || replay.Operation.ID() != retry.Operation.ID() {
		t.Fatalf("recovery replay = (%q, %t, %v)", replay.Operation.ID(), replay.Replay, err)
	}

	restarted, err := worker.NewWithCatalog(store, resolver, newOutputsCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	restarted.RetryBase = 0
	drain(t, restarted, 64)
	child, err := store.GetExecution(context.Background(), retry.Operation.ID())
	if err != nil {
		t.Fatal(err)
	}
	childOperation, err := store.GetOperation(context.Background(), retry.Operation.ID())
	if err != nil {
		t.Fatal(err)
	}
	if childOperation.Operation.State() != domain.OperationStateSucceeded || child.OutputResolution != application.OutputResolutionPublished {
		t.Fatalf("child operation=%s resolution=%s", childOperation.Operation.State(), child.OutputResolution)
	}
	if child.CurrentAttempt != 0 || v2.submissionCount(source.Operation.ID()) != 0 || v2.submissionCount(retry.Operation.ID()) != 0 {
		t.Fatalf("recovery submitted work: attempt=%d source submits=%d child submits=%d", child.CurrentAttempt, v2.submissionCount(source.Operation.ID()), v2.submissionCount(retry.Operation.ID()))
	}
	if counts := store.RecordCounts(); counts.Attempts != before.Attempts {
		t.Fatalf("recovery created a submission attempt: before=%d after=%d", before.Attempts, counts.Attempts)
	}
	for i, operationID := range v2.requestedOperations {
		if operationID != source.Operation.ID() || v2.requestedRefs[i] != renewMapping || v2.requestedSourceRefs[i] != firstMapping {
			t.Fatalf("recovery observation[%d] operation=%q mapping=%q source=%q", i, operationID, v2.requestedRefs[i], v2.requestedSourceRefs[i])
		}
	}
	view := readView(t, store, "recover-db")
	if view.Outputs == nil {
		t.Fatal("recovered outputs were not published")
	}
}

func TestRejectedOutputRetryRejectsMalformedHistoricalProvenanceAtomically(t *testing.T) {
	attemptState := func(state application.SubmissionAttemptState) func(context.Context, application.UnitOfWork, application.ProvisioningExecutionRecord) error {
		return func(ctx context.Context, tx application.UnitOfWork, execution application.ProvisioningExecutionRecord) error {
			attempt, err := tx.SubmissionAttempts().GetSubmissionAttempt(ctx, execution.OperationID, execution.CurrentAttempt)
			if err != nil {
				return err
			}
			attempt.State = state
			return tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, attempt, application.SubmissionAttemptAccepted)
		}
	}
	tests := []struct {
		name   string
		mutate func(context.Context, application.UnitOfWork, application.ProvisioningExecutionRecord) error
	}{
		{name: "rejection details disagree", mutate: func(ctx context.Context, tx application.UnitOfWork, execution application.ProvisioningExecutionRecord) error {
			execution.OutputFailureMessage = "tampered"
			return tx.Executions().SaveExecution(ctx, execution, execution.Version)
		}},
		{name: "attempt is pending", mutate: attemptState(application.SubmissionAttemptPending)},
		{name: "attempt is leased", mutate: attemptState(application.SubmissionAttemptLeased)},
		{name: "attempt was rejected", mutate: attemptState(application.SubmissionAttemptRejected)},
		{name: "attempt was not found", mutate: attemptState(application.SubmissionAttemptNotFound)},
		{name: "unknown attempt lacks durable positive correlation", mutate: func(ctx context.Context, tx application.UnitOfWork, execution application.ProvisioningExecutionRecord) error {
			attempt, err := tx.SubmissionAttempts().GetSubmissionAttempt(ctx, execution.OperationID, execution.CurrentAttempt)
			if err != nil {
				return err
			}
			attempt.State = application.SubmissionAttemptUnknown
			if err := tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, attempt, application.SubmissionAttemptAccepted); err != nil {
				return err
			}
			execution.AcceptanceConfirmed = false
			execution.Correlation = provisioning.RequestCorrelationUnknown
			return tx.Executions().SaveExecution(ctx, execution, execution.Version)
		}},
		{name: "exact source attempt is missing", mutate: func(ctx context.Context, tx application.UnitOfWork, execution application.ProvisioningExecutionRecord) error {
			execution.CurrentAttempt++
			return tx.Executions().SaveExecution(ctx, execution, execution.Version)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			v1 := newMappingProvider(firstMapping, false)
			v1.outputMapping = firstMapping
			v1.invalidOutputs = true
			service, store, resolver, ref, firstWorker := outputsHarness(t, v1)
			admitted, err := service.AdmitCreateResource(context.Background(), createCommand(t, "malformed-db", "op-malformed-source"))
			if err != nil {
				t.Fatal(err)
			}
			drain(t, firstWorker, 64)
			sourceExecution, err := store.GetExecution(context.Background(), admitted.Operation.ID())
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
				return test.mutate(context.Background(), tx, sourceExecution)
			}); err != nil {
				t.Fatal(err)
			}

			repair := newMappingProvider(renewMapping, false)
			repair.recoverySource = firstMapping
			repair.recoveryMapping = renewMapping
			resolver.Providers[ref] = repair
			before := store.RecordCounts()
			_, err = service.AdmitRetryOperation(context.Background(), application.RetryOperationCommand{
				Actor: applicationfake.Principal("tester"), OperationID: admitted.Operation.ID(), ExpectedGeneration: 1,
				NewOperationID: "op-malformed-child", EventID: "event-malformed-child",
				RequestedAt: testTime.Add(20 * time.Minute), IdempotencyKey: "malformed-retry",
			})
			if !errors.Is(err, application.ErrOperationNotRetryable) {
				t.Fatalf("malformed retry error = %v", err)
			}
			if after := store.RecordCounts(); after != before {
				t.Fatalf("malformed retry changed durable counts: before=%+v after=%+v", before, after)
			}
			if repair.submissionCount("op-malformed-child") != 0 {
				t.Fatal("malformed recovery submitted backend work")
			}
		})
	}
}

func TestOutputRecoveryTerminalFailureRemainsPendingAndRetriesObserveOnly(t *testing.T) {
	v1 := newMappingProvider(firstMapping, false)
	v1.outputMapping = firstMapping
	v1.invalidOutputs = true
	service, store, resolver, ref, firstWorker := outputsHarness(t, v1)
	admitted, err := service.AdmitCreateResource(context.Background(), createCommand(t, "conflict-db", "op-conflict-source"))
	if err != nil {
		t.Fatal(err)
	}
	drain(t, firstWorker, 64)
	sourceBefore, err := store.GetOperation(context.Background(), admitted.Operation.ID())
	if err != nil {
		t.Fatal(err)
	}
	sourceExecution, err := store.GetExecution(context.Background(), admitted.Operation.ID())
	if err != nil {
		t.Fatal(err)
	}

	repair := newMappingProvider(renewMapping, false)
	repair.recoverySource = firstMapping
	repair.recoveryMapping = renewMapping
	repair.recoveryFailed = true
	repair.observedBase = testTime.Add(20 * time.Minute)
	resolver.Providers[ref] = repair
	retry, err := service.AdmitRetryOperation(context.Background(), application.RetryOperationCommand{
		Actor: applicationfake.Principal("tester"), OperationID: admitted.Operation.ID(), ExpectedGeneration: 1,
		NewOperationID: "op-conflict-child", EventID: "event-conflict-child",
		RequestedAt: testTime.Add(20 * time.Minute), IdempotencyKey: "conflict-retry",
	})
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := worker.NewWithCatalog(store, resolver, newOutputsCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	restarted.RetryBase = 0
	for range 16 {
		if err := pumpOnce(t, restarted); err != nil {
			t.Fatal(err)
		}
		if len(repair.requestedOperations) != 0 {
			break
		}
	}

	child, err := store.GetOperation(context.Background(), retry.Operation.ID())
	if err != nil {
		t.Fatal(err)
	}
	childExecution, err := store.GetExecution(context.Background(), retry.Operation.ID())
	if err != nil {
		t.Fatal(err)
	}
	if child.Operation.IsTerminal() || childExecution.State != application.AttemptSucceeded || childExecution.OutputResolution != application.OutputResolutionPending {
		t.Fatalf("child operation=%s execution=%s resolution=%s", child.Operation.State(), childExecution.State, childExecution.OutputResolution)
	}
	sourceAfter, err := store.GetOperation(context.Background(), admitted.Operation.ID())
	if err != nil {
		t.Fatal(err)
	}
	if sourceAfter != sourceBefore {
		t.Fatalf("source operation changed: before=%+v after=%+v", sourceBefore, sourceAfter)
	}
	if repair.submissionCount(retry.Operation.ID()) != 0 || repair.submissionCount(admitted.Operation.ID()) != 0 {
		t.Fatal("contradictory recovery evidence submitted backend work")
	}
	if counts := store.RecordCounts(); counts.Attempts != int(sourceExecution.CurrentAttempt) {
		t.Fatalf("recovery created submission attempt: counts=%+v", counts)
	}
	repair.recoveryFailed = false
	drain(t, restarted, 64)
	child, err = store.GetOperation(context.Background(), retry.Operation.ID())
	if err != nil {
		t.Fatal(err)
	}
	if child.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("child did not recover after contradictory evidence: %s", child.Operation.State())
	}
}

func TestOutputRecoveryObservationUsesRetryReplayLockOrder(t *testing.T) {
	v1 := newMappingProvider(firstMapping, false)
	v1.outputMapping = firstMapping
	v1.invalidOutputs = true
	service, store, resolver, ref, firstWorker := outputsHarness(t, v1)
	admitted, err := service.AdmitCreateResource(context.Background(), createCommand(t, "lock-order-db", "op-lock-order-source"))
	if err != nil {
		t.Fatal(err)
	}
	drain(t, firstWorker, 64)

	repair := newMappingProvider(renewMapping, false)
	repair.recoverySource = firstMapping
	repair.recoveryMapping = renewMapping
	resolver.Providers[ref] = repair
	retry, err := service.AdmitRetryOperation(context.Background(), application.RetryOperationCommand{
		Actor: applicationfake.Principal("tester"), OperationID: admitted.Operation.ID(), ExpectedGeneration: 1,
		NewOperationID: "op-lock-order-child", EventID: "event-lock-order-child",
		RequestedAt: testTime.Add(20 * time.Minute), IdempotencyKey: "lock-order-retry",
	})
	if err != nil {
		t.Fatal(err)
	}

	traced := &lockTraceRunner{inner: store}
	restarted, err := worker.NewWithCatalog(traced, resolver, newOutputsCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	restarted.RetryBase = 0
	drain(t, restarted, 64)

	want := []string{
		"outbox:" + application.ObserveMessage(retry.Operation.ID(), 1, 0).ID,
		"lookup-execution:" + string(retry.Operation.ID()),
		"resource:" + string(admitted.Resource.Resource.ID()),
		"operation:" + string(admitted.Operation.ID()),
		"operation:" + string(retry.Operation.ID()),
		"execution:" + string(admitted.Operation.ID()),
		"execution:" + string(retry.Operation.ID()),
		fmt.Sprintf("attempt:%s:%d", admitted.Operation.ID(), 1),
	}
	matched := 0
	for _, transaction := range traced.snapshot() {
		if hasPrefix(transaction, want) {
			matched++
		}
	}
	if matched != 2 {
		t.Fatalf("canonical output-recovery lock sequence matched %d transactions, want prepare and record; trace=%v", matched, traced.snapshot())
	}
	if repair.submissionCount(admitted.Operation.ID()) != 0 || repair.submissionCount(retry.Operation.ID()) != 0 {
		t.Fatal("lock-order recovery submitted backend work")
	}
}

func hasPrefix(values, prefix []string) bool {
	if len(values) < len(prefix) {
		return false
	}
	for i := range prefix {
		if values[i] != prefix[i] {
			return false
		}
	}
	return true
}

type lockTraceRunner struct {
	inner application.TransactionRunner
	mu    sync.Mutex
	trace [][]string
}

func (r *lockTraceRunner) Within(ctx context.Context, fn func(application.UnitOfWork) error) error {
	var transaction []string
	err := r.inner.Within(ctx, func(tx application.UnitOfWork) error {
		return fn(&lockTraceUnitOfWork{UnitOfWork: tx, add: func(entry string) {
			transaction = append(transaction, entry)
		}})
	})
	r.mu.Lock()
	r.trace = append(r.trace, transaction)
	r.mu.Unlock()
	return err
}

func (r *lockTraceRunner) snapshot() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([][]string, len(r.trace))
	for i := range r.trace {
		result[i] = append([]string(nil), r.trace[i]...)
	}
	return result
}

type lockTraceUnitOfWork struct {
	application.UnitOfWork
	add func(string)
}

func (u *lockTraceUnitOfWork) Resources() application.ResourceRepository {
	return &lockTraceResources{ResourceRepository: u.UnitOfWork.Resources(), add: u.add}
}

func (u *lockTraceUnitOfWork) Operations() application.OperationRepository {
	return &lockTraceOperations{OperationRepository: u.UnitOfWork.Operations(), add: u.add}
}

func (u *lockTraceUnitOfWork) Executions() application.ExecutionRepository {
	return &lockTraceExecutions{ExecutionRepository: u.UnitOfWork.Executions(), add: u.add}
}

func (u *lockTraceUnitOfWork) SubmissionAttempts() application.SubmissionAttemptRepository {
	return &lockTraceAttempts{SubmissionAttemptRepository: u.UnitOfWork.SubmissionAttempts(), add: u.add}
}

func (u *lockTraceUnitOfWork) Outbox() application.OutboxRepository {
	return &lockTraceOutbox{OutboxRepository: u.UnitOfWork.Outbox(), add: u.add}
}

type lockTraceResources struct {
	application.ResourceRepository
	add func(string)
}

func (r *lockTraceResources) GetResource(ctx context.Context, id domain.ResourceID) (application.ResourceRecord, error) {
	r.add("resource:" + string(id))
	return r.ResourceRepository.GetResource(ctx, id)
}

type lockTraceOperations struct {
	application.OperationRepository
	add func(string)
}

func (r *lockTraceOperations) GetOperation(ctx context.Context, id domain.OperationID) (application.OperationRecord, error) {
	r.add("operation:" + string(id))
	return r.OperationRepository.GetOperation(ctx, id)
}

type lockTraceExecutions struct {
	application.ExecutionRepository
	add func(string)
}

func (r *lockTraceExecutions) LookupExecution(ctx context.Context, id domain.OperationID) (application.ProvisioningExecutionRecord, error) {
	r.add("lookup-execution:" + string(id))
	return r.ExecutionRepository.LookupExecution(ctx, id)
}

func (r *lockTraceExecutions) GetExecution(ctx context.Context, id domain.OperationID) (application.ProvisioningExecutionRecord, error) {
	r.add("execution:" + string(id))
	return r.ExecutionRepository.GetExecution(ctx, id)
}

type lockTraceAttempts struct {
	application.SubmissionAttemptRepository
	add func(string)
}

func (r *lockTraceAttempts) GetSubmissionAttempt(ctx context.Context, id domain.OperationID, attempt uint64) (application.SubmissionAttemptRecord, error) {
	r.add(fmt.Sprintf("attempt:%s:%d", id, attempt))
	return r.SubmissionAttemptRepository.GetSubmissionAttempt(ctx, id, attempt)
}

type lockTraceOutbox struct {
	application.OutboxRepository
	add func(string)
}

func (r *lockTraceOutbox) GetOutbox(ctx context.Context, id string) (application.OutboxMessage, error) {
	r.add("outbox:" + id)
	return r.OutboxRepository.GetOutbox(ctx, id)
}

func TestEagerOutputRecoveryTerminalFailureStaysPendingButSuccessfulInvalidOutputsFail(t *testing.T) {
	v1 := newMappingProvider(firstMapping, false)
	v1.outputMapping = firstMapping
	v1.invalidOutputs = true
	service, store, resolver, ref, firstWorker := outputsHarness(t, v1)
	admitted, err := service.AdmitCreateResource(context.Background(), createCommand(t, "eager-conflict-db", "op-eager-conflict-source"))
	if err != nil {
		t.Fatal(err)
	}
	drain(t, firstWorker, 64)
	sourceBefore, err := store.GetOperation(context.Background(), admitted.Operation.ID())
	if err != nil {
		t.Fatal(err)
	}
	repair := newMappingProvider(renewMapping, false)
	repair.recoverySource = firstMapping
	repair.recoveryMapping = renewMapping
	repair.recoveryFailed = true
	repair.observedBase = testTime.Add(30 * time.Minute)
	resolver.Providers[ref] = repair
	service.EnableEagerExecutionForTesting()
	retry, err := service.RetryOperation(context.Background(), application.RetryOperationCommand{
		Actor: applicationfake.Principal("tester"), OperationID: admitted.Operation.ID(), ExpectedGeneration: 1,
		NewOperationID: "op-eager-conflict-child", EventID: "event-eager-conflict-child",
		RequestedAt: testTime.Add(30 * time.Minute), IdempotencyKey: "eager-conflict-retry",
	})
	if err != nil {
		t.Fatal(err)
	}
	if retry.Operation.IsTerminal() || retry.Execution == nil || retry.Execution.OutputResolution != application.OutputResolutionPending {
		t.Fatalf("child state=%s execution=%+v", retry.Operation.State(), retry.Execution)
	}
	sourceAfter, err := store.GetOperation(context.Background(), admitted.Operation.ID())
	if err != nil {
		t.Fatal(err)
	}
	if sourceAfter != sourceBefore || repair.submissionCount(retry.Operation.ID()) != 0 {
		t.Fatal("eager recovery changed source or submitted backend work")
	}
	repair.recoveryFailed = false
	repair.invalidOutputs = true
	repair.outputMapping = renewMapping
	for range 4 {
		retry, err = service.ObserveOperation(context.Background(), application.ObserveOperationCommand{
			OperationID: retry.Operation.ID(), ObservedAt: testTime.Add(31 * time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
		if retry.Operation.IsTerminal() {
			break
		}
	}
	failure, ok := retry.Operation.Failure()
	if retry.Operation.State() != domain.OperationStateFailed || !ok || failure.Reason() != application.ReasonOutputPostconditionRejected {
		t.Fatalf("child state=%s failure=%+v", retry.Operation.State(), failure)
	}
}

func TestWorkerOutputRecoveryRejectsSourceChildSpecMismatchBeforeObserve(t *testing.T) {
	v1 := newMappingProvider(firstMapping, false)
	v1.outputMapping = firstMapping
	v1.invalidOutputs = true
	service, store, resolver, ref, firstWorker := outputsHarness(t, v1)
	admitted, err := service.AdmitCreateResource(context.Background(), createCommand(t, "spec-db", "op-spec-source"))
	if err != nil {
		t.Fatal(err)
	}
	drain(t, firstWorker, 64)
	repair := newMappingProvider(renewMapping, false)
	repair.recoverySource = firstMapping
	repair.recoveryMapping = renewMapping
	resolver.Providers[ref] = repair
	retry, err := service.AdmitRetryOperation(context.Background(), application.RetryOperationCommand{
		Actor: applicationfake.Principal("tester"), OperationID: admitted.Operation.ID(), ExpectedGeneration: 1,
		NewOperationID: "op-spec-child", EventID: "event-spec-child",
		RequestedAt: testTime.Add(20 * time.Minute), IdempotencyKey: "spec-retry",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		execution, err := tx.Executions().GetExecution(context.Background(), retry.Operation.ID())
		if err != nil {
			return err
		}
		execution.Spec, err = domain.NewResourceSpec(map[string]any{"tampered": true})
		if err != nil {
			return err
		}
		return tx.Executions().SaveExecution(context.Background(), execution, execution.Version)
	}); err != nil {
		t.Fatal(err)
	}
	restarted, err := worker.NewWithCatalog(store, resolver, newOutputsCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	restarted.RetryBase = 0
	var got error
	for range 16 {
		_, got = restarted.RunOnce(context.Background())
		if got != nil {
			break
		}
	}
	if !errors.Is(got, application.ErrInvalidApplicationCall) {
		t.Fatalf("spec mismatch error = %v", got)
	}
	if len(repair.requestedOperations) != 0 || repair.submissionCount(retry.Operation.ID()) != 0 {
		t.Fatal("spec-mismatched recovery reached provider")
	}
}

func TestEagerOutputRecoveryRejectsSourceChildSpecMismatchBeforeObserve(t *testing.T) {
	v1 := newMappingProvider(firstMapping, false)
	v1.outputMapping = firstMapping
	v1.invalidOutputs = true
	service, store, resolver, ref, firstWorker := outputsHarness(t, v1)
	admitted, err := service.AdmitCreateResource(context.Background(), createCommand(t, "eager-spec-db", "op-eager-spec-source"))
	if err != nil {
		t.Fatal(err)
	}
	drain(t, firstWorker, 64)
	repair := newMappingProvider(renewMapping, false)
	repair.recoverySource = firstMapping
	repair.recoveryMapping = renewMapping
	resolver.Providers[ref] = repair
	retry, err := service.AdmitRetryOperation(context.Background(), application.RetryOperationCommand{
		Actor: applicationfake.Principal("tester"), OperationID: admitted.Operation.ID(), ExpectedGeneration: 1,
		NewOperationID: "op-eager-spec-child", EventID: "event-eager-spec-child",
		RequestedAt: testTime.Add(40 * time.Minute), IdempotencyKey: "eager-spec-retry",
	})
	if err != nil {
		t.Fatal(err)
	}
	service.EnableEagerExecutionForTesting()
	changedAt := testTime.Add(41 * time.Minute)
	for _, phase := range []domain.OperationPhase{domain.OperationPhaseValidating, domain.OperationPhasePlanning, domain.OperationPhaseApplying} {
		if _, err := service.AdvanceOperation(context.Background(), application.AdvanceOperationCommand{
			OperationID: retry.Operation.ID(), Phase: phase,
			EventID: domain.EventID("event-eager-spec-" + string(phase)), ChangedAt: changedAt,
		}); err != nil {
			t.Fatal(err)
		}
		changedAt = changedAt.Add(time.Nanosecond)
	}
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		execution, err := tx.Executions().GetExecution(context.Background(), retry.Operation.ID())
		if err != nil {
			return err
		}
		execution.Spec, err = domain.NewResourceSpec(map[string]any{"tampered": true})
		if err != nil {
			return err
		}
		return tx.Executions().SaveExecution(context.Background(), execution, execution.Version)
	}); err != nil {
		t.Fatal(err)
	}
	_, err = service.ObserveOperation(context.Background(), application.ObserveOperationCommand{
		OperationID: retry.Operation.ID(), ObservedAt: changedAt.Add(time.Minute),
	})
	if !errors.Is(err, application.ErrInvalidApplicationCall) {
		t.Fatalf("eager spec mismatch error = %v", err)
	}
	if len(repair.requestedOperations) != 0 {
		t.Fatal("eager spec-mismatched recovery reached provider")
	}
}

func readRequestedRefs(provider *mappingProvider) []string {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return append([]string(nil), provider.requestedRefs...)
}

func readView(t *testing.T, store *applicationfake.Store, id string) application.ResourceView {
	t.Helper()
	var view application.ResourceView
	err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		record, err := tx.Resources().GetResource(context.Background(), domain.ResourceID(id))
		if err != nil {
			return err
		}
		view = application.ResourceView{Resource: record}
		if record.Status.State() != domain.ResourceStateDeleted {
			outputRecord, found, err := tx.Outputs().LatestResourceOutputs(context.Background(), domain.ResourceID(id))
			if err != nil {
				return err
			}
			if found {
				outputs := outputRecord.Values
				view.Outputs = &outputs
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return view
}

// TestMissingPersistedMappingNeverFallsBack pins corrections E/F: when a
// durable execution's persisted mapping identity cannot be resolved by the
// running deployment, extraction fails loudly, nothing is published, the
// operation stays active without false completion, and the backend is never
// re-executed.
func TestMissingPersistedMappingNeverFallsBack(t *testing.T) {
	provider := newMappingProvider(firstMapping, false)
	service, store, resolver, ref, firstWorker := outputsHarness(t, provider)

	admitted, err := service.AdmitCreateResource(context.Background(), createCommand(t, "missing-map", "op-missing"))
	if err != nil {
		t.Fatal(err)
	}

	// Drive until backend success is recorded with Pending output resolution.
	pending := func() bool {
		record, err := store.GetExecution(context.Background(), admitted.Operation.ID())
		return err == nil && record.OutputResolution == application.OutputResolutionPending
	}
	var lastErr error
	for range 32 {
		if pending() {
			break
		}
		lastErr = pumpOnce(t, firstWorker)
		if errors.Is(lastErr, errDrained{}) {
			t.Fatal("drained before reaching Pending output resolution")
		}
	}
	if !pending() {
		t.Fatal("execution never reached Pending output resolution")
	}

	// Deploy a newer build that only knows the v2 mapping. The in-flight
	// execution still references v1: recovery must refuse it loudly instead
	// of silently adopting v2 semantics.
	upgraded := newMappingProvider(renewMapping, true)
	resolver.Providers[ref] = upgraded
	instance, err := worker.NewWithCatalog(store, resolver, newOutputsCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	instance.RetryBase = time.Hour

	loudFailure := ""
	for range 8 {
		err := pumpOnce(t, instance)
		if err == nil {
			continue
		}
		if errors.Is(err, errDrained{}) {
			break
		}
		var observationErr provisioning.ObservationError
		if errors.As(err, &observationErr) && observationErr.Failure.Reason == "OutputMappingMissing" {
			loudFailure = observationErr.Failure.Reason
			break
		}
	}
	if loudFailure == "" {
		t.Fatal("unresolvable mapping did not fail loudly")
	}

	view := readView(t, store, "missing-map")
	if view.Outputs != nil {
		t.Fatal("outputs were published through an unresolvable mapping fallback")
	}
	if view.Resource.Status.State() == domain.ResourceStateReady {
		t.Fatal("reconciliation completed despite unresolvable output mapping")
	}
	if got := upgraded.submissionCount(admitted.Operation.ID()); got != 0 {
		t.Fatalf("unresolvable mapping triggered %d backend re-executions", got)
	}
	requestedRefs := readRequestedRefs(upgraded)
	for _, requested := range requestedRefs {
		if requested == renewMapping && requested != "" {
			continue
		}
	}
	// Every request the upgraded deployment served must carry the persisted
	// original identity — never the deployment's current one.
	for _, requested := range requestedRefs {
		if requested == "" {
			t.Fatal("a request reached the upgraded deployment without the persisted mapping identity")
		}
		if requested != firstMapping {
			t.Fatalf("request carried %q instead of the persisted %q", requested, firstMapping)
		}
	}
}
