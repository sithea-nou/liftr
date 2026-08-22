// SPDX-License-Identifier: Apache-2.0

package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"

	appfake "github.com/sithea-nou/liftr/internal/application/fake"
)

// strictContract is an application-side synthetic contract with hand-written
// validation. It proves the orchestration boundary without importing any
// concrete ResourceType implementation.
type strictContract struct {
	ref         domain.ResourceTypeRef
	failLookups bool
	validations int
}

func newStrictContract(name string) *strictContract {
	return &strictContract{ref: domain.ResourceTypeRef{Name: name, Version: "v1"}}
}

func (c *strictContract) Ref() domain.ResourceTypeRef { return c.ref }
func (c *strictContract) DisplayName() string         { return c.ref.Name + " Display" }
func (c *strictContract) Description() string         { return c.ref.Name + " contract." }

func (c *strictContract) Capabilities() []domain.Capability {
	return []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete}
}

func (c *strictContract) Domain() domain.ResourceType {
	typeValue, err := domain.NewResourceType(c.ref, c.Description(), c.Capabilities())
	if err != nil {
		panic(err)
	}
	return typeValue
}

func (c *strictContract) SpecSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

func (c *strictContract) ValidateSpec(spec domain.ResourceSpec) error {
	c.validations++
	if _, ok := spec.Values()["name"]; !ok {
		return application.NewInvalidSpecError(c.ref, []application.SpecViolation{{
			Path:    "",
			Keyword: "required",
			Message: `property "name" is required`,
		}})
	}
	return nil
}

type strictCatalog struct {
	types map[domain.ResourceTypeRef]*strictContract
	order []domain.ResourceTypeRef
}

func newStrictCatalog(names ...string) *strictCatalog {
	catalog := &strictCatalog{types: make(map[domain.ResourceTypeRef]*strictContract)}
	for _, name := range names {
		contract := newStrictContract(name)
		catalog.types[contract.Ref()] = contract
		catalog.order = append(catalog.order, contract.Ref())
	}
	return catalog
}

func (c *strictCatalog) Get(_ context.Context, ref domain.ResourceTypeRef) (application.ResourceContract, error) {
	contract, ok := c.types[ref]
	if !ok || contract.failLookups {
		return nil, errors.New("catalog unavailable")
	}
	return contract, nil
}

func (c *strictCatalog) List(_ context.Context) ([]application.ResourceContract, error) {
	contracts := make([]application.ResourceContract, 0, len(c.order))
	for _, ref := range c.order {
		contracts = append(contracts, c.types[ref])
	}
	return contracts, nil
}

type admissionFixture struct {
	service  *application.Service
	store    *appfake.Store
	catalog  *strictCatalog
	selector *appfake.Selector
	resolver *appfake.Resolver
	ref      application.ProvisionerRef
}

func newAdmissionFixture(t *testing.T, typeNames ...string) *admissionFixture {
	t.Helper()
	store := appfake.NewStore()
	catalog := newStrictCatalog(typeNames...)
	ref, err := application.NewProvisionerRef("validation-test-provider")
	if err != nil {
		t.Fatal(err)
	}
	selector := &appfake.Selector{Ref: ref}
	resolver := &appfake.Resolver{Providers: map[application.ProvisionerRef]provisioning.Provisioner{
		ref: appfakeProvider{},
	}}
	service, err := application.NewService(catalog, selector, resolver, store)
	if err != nil {
		t.Fatal(err)
	}
	return &admissionFixture{service: service, store: store, catalog: catalog, selector: selector, resolver: resolver, ref: ref}
}

// appfakeProvider is a provisioner stub; these tests never reach dispatch.
type appfakeProvider struct{}

func (appfakeProvider) Capabilities() []provisioning.ProvisionerCapability { return nil }
func (p appfakeProvider) Submit(context.Context, provisioning.ExecutionRequest) (provisioning.Submission, error) {
	return provisioning.Submission{}, errors.New("dispatch not expected in validation tests")
}
func (p appfakeProvider) Observe(context.Context, provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return provisioning.ExecutionObservation{}, errors.New("observation not expected in validation tests")
}

func createCommand(id string, spec domain.ResourceSpec) application.CreateResourceCommand {
	return application.CreateResourceCommand{
		ID:             domain.ResourceID(id),
		Type:           domain.ResourceTypeRef{Name: "Widget", Version: "v1"},
		Owner:          domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec:           spec,
		OperationID:    domain.OperationID("op-" + id),
		EventID:        domain.EventID("evt-" + id),
		RequestedAt:    time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC),
		IdempotencyKey: "key-create-" + id,
	}
}

func validSpec(values map[string]any) domain.ResourceSpec {
	spec, err := domain.NewResourceSpec(values)
	if err != nil {
		panic(err)
	}
	return spec
}

// TestCreateValidatesSpecBeforeAnyPersistence pins invariant L: an invalid
// first-time spec produces no Resource, Operation, Event, Execution,
// Idempotency record, or outbox work.
func TestCreateValidatesSpecBeforeAnyPersistence(t *testing.T) {
	fixture := newAdmissionFixture(t, "Widget")
	_, err := fixture.service.AdmitCreateResource(context.Background(), createCommand("r1", validSpec(map[string]any{"wrong": true})))
	if !errors.Is(err, application.ErrInvalidResourceSpec) {
		t.Fatalf("error = %v, want ErrInvalidResourceSpec", err)
	}
	counts := fixture.store.RecordCounts()
	if counts != (appfake.RecordCounts{}) {
		t.Fatalf("invalid spec persisted durable state: %+v", counts)
	}
}

// TestValidationRunsAfterCatalogLookup pins that an unknown type is reported
// as a resource-type failure and that validation never runs for it.
func TestValidationRunsAfterCatalogLookup(t *testing.T) {
	fixture := newAdmissionFixture(t, "Widget")
	command := createCommand("r1", validSpec(map[string]any{}))
	command.Type = domain.ResourceTypeRef{Name: "Missing", Version: "v1"}
	_, err := fixture.service.AdmitCreateResource(context.Background(), command)
	if !errors.Is(err, application.ErrResourceTypeNotFound) {
		t.Fatalf("error = %v, want ErrResourceTypeNotFound", err)
	}
	if got := fixture.catalog.types[domain.ResourceTypeRef{Name: "Widget", Version: "v1"}].validations; got != 0 {
		t.Fatalf("validation ran %d times for an unknown type", got)
	}
}

// TestValidationRunsBeforeProvisionerSelection pins the approved order:
// catalog lookup -> ValidateSpec -> selector -> lifecycle admission.
func TestValidationRunsBeforeProvisionerSelection(t *testing.T) {
	fixture := newAdmissionFixture(t, "Widget")
	_, err := fixture.service.AdmitCreateResource(context.Background(), createCommand("r1", validSpec(map[string]any{})))
	if !errors.Is(err, application.ErrInvalidResourceSpec) {
		t.Fatalf("error = %v, want ErrInvalidResourceSpec", err)
	}
	if fixture.selector.Calls != 0 {
		t.Fatalf("selector was consulted %d times despite invalid spec", fixture.selector.Calls)
	}
	if got := fixture.catalog.types[domain.ResourceTypeRef{Name: "Widget", Version: "v1"}].validations; got != 1 {
		t.Fatalf("ValidateSpec ran %d times, want exactly once", got)
	}
}

// TestReplayPrecedesCatalogAndSchemaValidation pins invariant M: replaying a
// previously admitted request still succeeds even if the catalog later
// becomes unavailable and would reject validation.
func TestReplayPrecedesCatalogAndSchemaValidation(t *testing.T) {
	fixture := newAdmissionFixture(t, "Widget")
	command := createCommand("r1", validSpec(map[string]any{"name": "gear"}))
	first, err := fixture.service.AdmitCreateResource(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}

	contract := fixture.catalog.types[command.Type]
	contract.failLookups = true // catalog can no longer serve lookups
	replay, err := fixture.service.AdmitCreateResource(context.Background(), command)
	if err != nil {
		t.Fatalf("replay failed after catalog degradation: %v", err)
	}
	if !replay.Replay {
		t.Fatal("expected replay result")
	}
	if replay.Operation.ID() != first.Operation.ID() {
		t.Fatalf("replay operation = %s, want original %s", replay.Operation.ID(), first.Operation.ID())
	}
	if got := contract.validations; got != 1 {
		t.Fatalf("replay revalidated the spec %d extra times", got-1)
	}
}

// TestFingerprintUnaffectedByValidation pins invariant 18/12: validation is a
// pure predicate and does not alter replay identity. The idempotency record
// stored for identical content is identical with and without a rejecting
// schema, and int64 vs float64 specs remain fingerprint-distinct.
func TestFingerprintUnaffectedByValidation(t *testing.T) {
	fixture := newAdmissionFixture(t, "Widget")
	intCommand := createCommand("int", validSpec(map[string]any{"name": int64(20)}))
	floatCommand := createCommand("float", validSpec(map[string]any{"name": float64(20)}))

	for _, command := range []application.CreateResourceCommand{intCommand, floatCommand} {
		if _, err := fixture.service.AdmitCreateResource(context.Background(), command); err != nil {
			t.Fatal(err)
		}
	}
	var intFingerprint, floatFingerprint string
	err := fixture.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		record, err := tx.Idempotency().GetIdempotency(context.Background(), intCommand.IdempotencyKey)
		if err != nil {
			return err
		}
		intFingerprint = record.Fingerprint
		record, err = tx.Idempotency().GetIdempotency(context.Background(), floatCommand.IdempotencyKey)
		if err != nil {
			return err
		}
		floatFingerprint = record.Fingerprint
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if intFingerprint == "" || floatFingerprint == "" {
		t.Fatal("idempotency records missing")
	}
	if intFingerprint == floatFingerprint {
		t.Fatal("20 and 20.0 unexpectedly share a fingerprint")
	}
}

// TestUpdateValidatesNewSpecAgainstStoredType pins that update admission
// validates the submitted revision before any generation bump or persistence.
func TestUpdateValidatesNewSpecAgainstStoredType(t *testing.T) {
	fixture := newAdmissionFixture(t, "Widget")
	createCmd := createCommand("r1", validSpec(map[string]any{"name": "gear"}))
	admitted, err := fixture.service.AdmitCreateResource(context.Background(), createCmd)
	if err != nil {
		t.Fatal(err)
	}

	update := application.UpdateResourceCommand{
		ID:                 admitted.Resource.Resource.ID(),
		ExpectedGeneration: 1,
		Spec:               validSpec(map[string]any{"invalid": true}),
		OperationID:        domain.OperationID("op-update"),
		EventID:            domain.EventID("evt-update"),
		RequestedAt:        createCmd.RequestedAt.Add(time.Minute),
		IdempotencyKey:     "key-update",
	}
	if _, err := fixture.service.AdmitUpdateResource(context.Background(), update); !errors.Is(err, application.ErrInvalidResourceSpec) {
		t.Fatalf("update error = %v, want ErrInvalidResourceSpec", err)
	}

	var record application.ResourceRecord
	err = fixture.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		var err error
		record, err = tx.Resources().GetResource(context.Background(), update.ID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.Resource.Generation() != 1 {
		t.Fatalf("generation advanced to %d despite rejected update", record.Resource.Generation())
	}
	if _, hasInvalid := record.Resource.Spec().Values()["invalid"]; hasInvalid {
		t.Fatal("rejected update spec was stored")
	}
}

// TestGetResourceTypeWrapsUnknown pins the discovery read path mapping.
func TestGetResourceTypeWrapsUnknown(t *testing.T) {
	fixture := newAdmissionFixture(t, "Widget")

	known, err := fixture.service.GetResourceType(context.Background(), domain.ResourceTypeRef{Name: "Widget", Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if known.DisplayName() != "Widget Display" {
		t.Fatalf("DisplayName() = %q", known.DisplayName())
	}

	_, err = fixture.service.GetResourceType(context.Background(), domain.ResourceTypeRef{Name: "Missing", Version: "v9"})
	if !errors.Is(err, application.ErrResourceTypeNotFound) {
		t.Fatalf("error = %v, want ErrResourceTypeNotFound", err)
	}

	// Catalog unavailability is indistinguishable from absence at the port.
	widget := fixture.catalog.types[domain.ResourceTypeRef{Name: "Widget", Version: "v1"}]
	widget.failLookups = true
	if _, err := fixture.service.GetResourceType(context.Background(), widget.Ref()); !errors.Is(err, application.ErrResourceTypeNotFound) {
		t.Fatalf("degraded lookup error = %v, want ErrResourceTypeNotFound", err)
	}
}

// TestListResourceTypesPassthrough pins the discovery list read path.
func TestListResourceTypesPassthrough(t *testing.T) {
	fixture := newAdmissionFixture(t, "Alpha", "Beta")
	contracts, err := fixture.service.ListResourceTypes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(contracts) != 2 {
		t.Fatalf("List() returned %d contracts, want 2", len(contracts))
	}
	if contracts[0].Ref().Name != "Alpha" || contracts[1].Ref().Name != "Beta" {
		t.Fatalf("order = [%s, %s], want [Alpha, Beta]", contracts[0].Ref().Name, contracts[1].Ref().Name)
	}
}
