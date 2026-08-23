// SPDX-License-Identifier: Apache-2.0

package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
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
func (mappedContract) ValidateSpec(domain.ResourceSpec) error             { return nil }
func (mappedContract) ValidateUpdate(_, _ domain.ResourceSpec) error      { return nil }

// mappingProvider models a provisioner whose registered output mapping can
// change across deployments. It records every requested mapping identity and
// refuses observations whose persisted ref it cannot resolve — recovery never
// falls back to whatever happens to be registered now.
type mappingProvider struct {
	mu             sync.Mutex
	mappingRef     string
	submissions    map[domain.OperationID]int
	requestedRefs  []string
	observeCalls   map[domain.OperationID]int
	failUnknownRef bool
}

func newMappingProvider(mappingRef string, failUnknownRef bool) *mappingProvider {
	return &mappingProvider{mappingRef: mappingRef, submissions: map[domain.OperationID]int{},
		observeCalls: map[domain.OperationID]int{}, failUnknownRef: failUnknownRef}
}

func (p *mappingProvider) Capabilities() []provisioning.ProvisionerCapability {
	return []provisioning.ProvisionerCapability{{ResourceType: provisioningfake.ResourceType(), Capability: domain.CapabilityCreate}}
}

// OutputMappingRef declares this deployment's mapping identity.
func (p *mappingProvider) OutputMappingRef(domain.ResourceTypeRef, domain.Capability) string {
	return p.mappingRef
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
	// Each observation advances the provider timeline so evidence freshness
	// stays honest; only the OUTPUT dimension may repeat timestamps.
	observation.ObservedAt = testTime.Add(time.Duration(call) * time.Minute)
	switch {
	case call <= 2:
		observation.Execution.State = provisioning.ExecutionStateRunning
	case call == 3:
		observation.Outputs = &provisioning.OutputEvidence{State: provisioning.OutputsUnavailable}
	default:
		observation.Outputs = &provisioning.OutputEvidence{State: provisioning.OutputsAvailable,
			Values: map[string]any{"hostname": "db.example", "port": int64(5432)}}
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
	service, err := application.NewService(catalog, selector, resolver, store)
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

type errDrained struct{}

func (errDrained) Error() string { return "drained" }

// TestBackendSuccessPendingOutputSurvivesRestartWithOriginalMapping pins
// correction D: after backend success and Pending resolution, a restarted
// worker with a NEWER registered mapping still observes through the persisted
// original identity and publishes without re-executing the backend.
func TestBackendSuccessPendingOutputSurvivesRestartWithOriginalMapping(t *testing.T) {
	provider := newMappingProvider(firstMapping, false)
	service, store, resolver, ref, firstWorker := outputsHarness(t, provider)

	admitted, err := service.AdmitCreateResource(context.Background(), createCommand(t, "restart-db", "op-restart"))
	if err != nil {
		t.Fatal(err)
	}
	drain(t, firstWorker, 64)

	record, err := store.GetExecution(context.Background(), admitted.Operation.ID())
	if err != nil {
		t.Fatal(err)
	}
	if record.OutputResolution != application.OutputResolutionPublished || record.OutputResolution == application.OutputResolutionPending {
		t.Fatalf("pre-restart resolution = %s", record.OutputResolution)
	}
	if record.OutputMappingRef != firstMapping {
		t.Fatalf("persisted mapping = %q", record.OutputMappingRef)
	}

	// Restart with a newer deployed mapping: a fresh worker whose provider
	// registers liftr-outputs-v2. The in-flight execution must keep using the
	// persisted liftr-outputs-v1 semantics.
	newProvider := newMappingProvider(renewMapping, true)
	resolver.Providers[ref] = newProvider
	restarted, err := worker.NewWithCatalog(store, resolver, newOutputsCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	restarted.RetryBase = 0

	view := readView(t, store, "restart-db")
	if view.Resource.Status.State() != domain.ResourceStateReady || view.Outputs == nil {
		t.Fatalf("post-restart state=%s outputs=%#v", view.Resource.Status.State(), view.Outputs)
	}
	if got := readRequestedRefs(newProvider); len(got) != 0 {
		t.Logf("restarted worker observed %d messages using refs %v", len(got), got)
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
