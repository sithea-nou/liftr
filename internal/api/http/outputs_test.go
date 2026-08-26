// SPDX-License-Identifier: Apache-2.0

package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/sithea-nou/liftr/internal/application"
	applicationfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
	"github.com/sithea-nou/liftr/internal/resourcecontract"

	resourcetypes "github.com/sithea-nou/liftr/internal/resourcetypes"
	pgregistry "github.com/sithea-nou/liftr/internal/resourcetypes/postgresqldatabase"
)

// outputContractCatalog serves FakeResource/v1 with required hostname/port
// outputs so transport behavior can be exercised end to end.
type outputContractCatalog struct {
	inner  applicationfake.Catalog
	fields resourcecontract.OutputContract
}

func newOutputContractCatalog(t *testing.T) *outputContractCatalog {
	t.Helper()
	typeValue, err := domain.NewResourceType(provisioningfake.ResourceType(), "transport outputs resource",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		t.Fatal(err)
	}
	fields, err := resourcecontract.NewOutputContract([]resourcecontract.OutputField{
		{Name: "hostname", JSONType: resourcecontract.OutputTypeString, RequiredWhenReady: true},
		{Name: "port", JSONType: resourcecontract.OutputTypeInteger, RequiredWhenReady: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &outputContractCatalog{
		inner:  applicationfake.Catalog{Types: map[domain.ResourceTypeRef]domain.ResourceType{provisioningfake.ResourceType(): typeValue}},
		fields: fields,
	}
}

type outputContract struct {
	inner  domain.ResourceType
	fields resourcecontract.OutputContract
}

func (c outputContract) Ref() domain.ResourceTypeRef                      { return c.inner.Ref() }
func (c outputContract) DisplayName() string                              { return c.Ref().Name }
func (c outputContract) Description() string                              { return "" }
func (c outputContract) Capabilities() []domain.Capability                { return c.inner.Capabilities() }
func (c outputContract) Domain() domain.ResourceType                      { return c.inner }
func (c outputContract) SpecSchema() json.RawMessage                      { return json.RawMessage(`{"type":"object"}`) }
func (c outputContract) OutputContract() *resourcecontract.OutputContract { return &c.fields }
func (c outputContract) ReferenceContract() *resourcecontract.ReferenceContract {
	return nil
}
func (outputContract) ValidateSpec(domain.ResourceSpec) error        { return nil }
func (outputContract) ValidateUpdate(_, _ domain.ResourceSpec) error { return nil }

func (c *outputContractCatalog) Get(_ context.Context, ref domain.ResourceTypeRef) (resourcecontract.Contract, error) {
	typeValue, ok := c.inner.Types[ref]
	if !ok {
		return nil, errors.New("unknown")
	}
	return outputContract{inner: typeValue, fields: c.fields}, nil
}

func (c *outputContractCatalog) List(ctx context.Context) ([]resourcecontract.Contract, error) {
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

// evidenceProvider succeeds immediately with validated output values.
type evidenceProvider struct{}

func (*evidenceProvider) Capabilities() []provisioning.ProvisionerCapability {
	return []provisioning.ProvisionerCapability{{ResourceType: provisioningfake.ResourceType(), Capability: domain.CapabilityCreate}}
}

func (*evidenceProvider) Submit(_ context.Context, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	handle, _ := provisioning.NewExecutionHandle("h-" + string(request.OperationID))
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle},
		Resource:    domain.ObservedFacts{},
		Outputs: &provisioning.OutputEvidence{State: provisioning.OutputsAvailable, Values: map[string]any{
			"hostname": "orders-db.postgres.example", "port": int64(5432),
		}},
	}}, nil
}

func (*evidenceProvider) Observe(_ context.Context, request provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	handle, _ := provisioning.NewExecutionHandle("h-" + string(request.OperationID))
	return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle},
		Resource:  domain.ObservedFacts{}}, nil
}

// TestResourceGetEmbedsGenerationAssociatedOutputs drives create through the
// durable worker and asserts the public envelope.
func TestResourceGetEmbedsGenerationAssociatedOutputs(t *testing.T) {
	store := applicationfake.NewStore()
	catalog := newOutputContractCatalog(t)
	fixture := newFixtureWithParts(t, store, catalog)
	ref, err := application.NewProvisionerRef("transport-test-provider")
	if err != nil {
		t.Fatal(err)
	}
	fixture.resolver.Providers[ref] = &evidenceProvider{}
	fixture.catalog = catalog

	createResponse := fixture.createResource(t, "orders-db", map[string]any{"size": int64(5)})
	expectStatus(t, createResponse, http.StatusCreated)
	body := decodeBody(t, createResponse)
	if _, present := body["outputs"]; present {
		t.Fatalf("admission response already carries outputs: %v", body["outputs"])
	}
	fixture.drainWorker(t)

	response := fixture.request(t, http.MethodGet, "/v1/resources/orders-db", nil, nil)
	expectStatus(t, response, http.StatusOK)
	document := decodeBody(t, response)
	outputs, ok := document["outputs"].(map[string]any)
	if !ok {
		t.Fatalf("outputs missing or not an object: %v", document["outputs"])
	}
	generation, ok := outputs["observedGeneration"].(float64)
	if !ok || generation != 1 {
		t.Fatalf("observedGeneration = %v", outputs["observedGeneration"])
	}
	values, ok := outputs["values"].(map[string]any)
	if !ok {
		t.Fatalf("values missing: %v", outputs)
	}
	if values["hostname"] != "orders-db.postgres.example" || values["port"] != float64(5432) {
		t.Fatalf("values = %v", values)
	}
	if len(values) != 2 {
		t.Fatalf("undeclared values leaked: %v", values)
	}

	// Mutation replay bodies carry the current published snapshot too.
	replay := fixture.createResource(t, "orders-db", map[string]any{"size": int64(5)})
	replayBody := decodeBody(t, replay)
	if _, ok := replayBody["outputs"]; !ok {
		t.Fatal("replay body omits current outputs")
	}
}

// TestDiscoveryDeclaresVersionedOutputContracts pins Correction G at the
// transport: v1 detail has no outputContract, v2 declares hostname/port, and
// list summaries never embed either.
func TestDiscoveryDeclaresVersionedOutputContracts(t *testing.T) {
	v1, err := pgregistry.Contract()
	if err != nil {
		t.Fatal(err)
	}
	v2, err := pgregistry.ContractV2()
	if err != nil {
		t.Fatal(err)
	}
	registry, err := resourcetypes.NewRegistry(v1, v2)
	if err != nil {
		t.Fatal(err)
	}
	fixture := newFixtureWithCatalog(t, registry)

	listResponse := fixture.request(t, http.MethodGet, "/v1/resource-types", nil, nil)
	expectStatus(t, listResponse, http.StatusOK)
	list := decodeBody(t, listResponse)
	items, _ := list["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("list items = %d", len(items))
	}
	for _, rawItem := range items {
		item := rawItem.(map[string]any)
		if _, present := item["outputContract"]; present {
			t.Fatal("summary exposes outputContract")
		}
		if _, present := item["specSchema"]; present {
			t.Fatal("summary exposes specSchema")
		}
	}

	v1Detail := decodeBody(t, fixture.request(t, http.MethodGet, "/v1/resource-types/PostgreSQLDatabase/v1", nil, nil))
	if detail, present := v1Detail["outputContract"]; present || detail == nil && false {
		t.Fatalf("v1 detail exposes outputContract: %v", v1Detail["outputContract"])
	}
	if _, present := v1Detail["specSchema"]; !present {
		t.Fatal("v1 detail lost specSchema")
	}

	v2Detail := decodeBody(t, fixture.request(t, http.MethodGet, "/v1/resource-types/PostgreSQLDatabase/v2", nil, nil))
	rawContract, present := v2Detail["outputContract"]
	if !present {
		t.Fatal("v2 detail missing outputContract")
	}
	contractMap := rawContract.(map[string]any)
	fields, _ := contractMap["fields"].([]any)
	if len(fields) != 2 {
		t.Fatalf("v2 output fields = %d", len(fields))
	}
	first := fields[0].(map[string]any)
	second := fields[1].(map[string]any)
	if first["name"] != "hostname" || first["jsonType"] != "string" || first["requiredWhenReady"] != true {
		t.Fatalf("hostname declaration = %v", first)
	}
	if second["name"] != "port" || second["jsonType"] != "integer" || second["requiredWhenReady"] != true {
		t.Fatalf("port declaration = %v", second)
	}
}
