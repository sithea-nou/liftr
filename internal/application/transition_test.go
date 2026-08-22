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

	appfake "github.com/sithea-nou/liftr/internal/application/fake"
)

// syncSuccessProvider completes every submission synchronously so eager-mode
// fixtures reach terminal lifecycle states without any real backend. It is
// deliberately resource-type-agnostic; these tests never assert on its
// identity.
type syncSuccessProvider struct{}

func (syncSuccessProvider) Capabilities() []provisioning.ProvisionerCapability { return nil }

func (syncSuccessProvider) Submit(_ context.Context, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	handle, err := provisioning.NewExecutionHandle("transition-test-handle")
	if err != nil {
		return provisioning.Submission{}, err
	}
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle},
		Resource:    domain.ObservedFacts{Presence: domain.ResourcePresencePresent, Readiness: domain.ResourceReadinessReady, Drift: domain.ResourceDriftInSync},
	}}, nil
}

func (syncSuccessProvider) Observe(context.Context, provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return provisioning.ExecutionObservation{}, errors.New("observation not expected in transition tests")
}

// transitionFixture admits and completes a Widget resource, then configures
// the synthetic contract with a rejecting old→new rule, mirroring how concrete
// ResourceType contracts declare update-transition semantics.
func transitionFixture(t *testing.T) *admissionFixture {
	t.Helper()
	store := appfake.NewStore()
	catalog := newStrictCatalog("Widget")
	ref, err := application.NewProvisionerRef("transition-test-provider")
	if err != nil {
		t.Fatal(err)
	}
	selector := &appfake.Selector{Ref: ref}
	resolver := &appfake.Resolver{Providers: map[application.ProvisionerRef]provisioning.Provisioner{ref: syncSuccessProvider{}}}
	service, err := application.NewService(catalog, selector, resolver, store)
	if err != nil {
		t.Fatal(err)
	}
	service.EnableEagerExecutionForTesting()
	command := createCommand("r1", validSpec(map[string]any{"name": "gear", "size": int64(20)}))
	if _, err := service.CreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	record := readRecord(t, store, "r1")
	if record.Status.State() != domain.ResourceStateReady || record.Resource.Generation() != 1 {
		t.Fatalf("fixture create did not complete: state=%s generation=%d", record.Status.State(), record.Resource.Generation())
	}
	return &admissionFixture{service: service, store: store, catalog: catalog, selector: selector, resolver: resolver, ref: ref}
}

func updateCommand(id string, generation uint64, spec domain.ResourceSpec, key string) application.UpdateResourceCommand {
	return application.UpdateResourceCommand{
		ID:                 domain.ResourceID(id),
		ExpectedGeneration: generation,
		Spec:               spec,
		OperationID:        domain.OperationID("op-" + key),
		EventID:            domain.EventID("evt-" + key),
		RequestedAt:        time.Date(2026, 8, 22, 10, 5, 0, 0, time.UTC),
		IdempotencyKey:     "key-" + key,
	}
}

func readRecord(t *testing.T, store *appfake.Store, id domain.ResourceID) application.ResourceRecord {
	t.Helper()
	var record application.ResourceRecord
	err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		var err error
		record, err = tx.Resources().GetResource(context.Background(), id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// TestIllegalTransitionIsRejectedSynchronouslyWithZeroDurableEffect pins the
// M9 admission boundary: a schema-valid spec whose old→new transition is
// illegal under the contract is rejected before any durable mutation — no new
// Operation, Event, Execution, Idempotency record, or outbox work, an
// unchanged Resource generation and spec, and no new active work.
func TestIllegalTransitionIsRejectedSynchronouslyWithZeroDurableEffect(t *testing.T) {
	fixture := transitionFixture(t)
	widget := fixture.catalog.types[domain.ResourceTypeRef{Name: "Widget", Version: "v1"}]
	widget.transitionFunc = func(oldSpec, newSpec domain.ResourceSpec) error {
		if newSpec.Values()["size"].(int64) < oldSpec.Values()["size"].(int64) {
			return application.NewInvalidSpecError(widget.ref, []application.SpecViolation{{
				Path: "/size", Keyword: "transition", Message: "size cannot decrease",
			}})
		}
		return nil
	}

	before := fixture.store.RecordCounts()
	rejected := updateCommand("r1", 1, validSpec(map[string]any{"name": "gear", "size": int64(10)}), "shrink")
	_, err := fixture.service.AdmitUpdateResource(context.Background(), rejected)
	var invalid *application.InvalidSpecError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v, want *application.InvalidSpecError", err)
	}
	if len(invalid.Violations) != 1 || invalid.Violations[0].Path != "/size" || invalid.Violations[0].Keyword != "transition" {
		t.Fatalf("violations = %+v", invalid.Violations)
	}
	after := fixture.store.RecordCounts()
	if after != before {
		t.Fatalf("illegal transition persisted durable state: before=%+v after=%+v", before, after)
	}
	record := readRecord(t, fixture.store, "r1")
	if record.Resource.Generation() != 1 || record.Resource.Spec().Values()["size"] != int64(20) {
		t.Fatalf("desired state mutated by illegal transition: generation=%d spec=%+v",
			record.Resource.Generation(), record.Resource.Spec().Values())
	}
}

// TestLegalTransitionIsAdmitted pins that contract-legal transitions proceed
// through normal update admission to terminal success.
func TestLegalTransitionIsAdmitted(t *testing.T) {
	fixture := transitionFixture(t)
	widget := fixture.catalog.types[domain.ResourceTypeRef{Name: "Widget", Version: "v1"}]
	widget.transitionFunc = func(oldSpec, newSpec domain.ResourceSpec) error {
		if newSpec.Values()["size"].(int64) < oldSpec.Values()["size"].(int64) {
			return errors.New("size cannot decrease")
		}
		return nil
	}
	result, err := fixture.service.UpdateResource(context.Background(),
		updateCommand("r1", 1, validSpec(map[string]any{"name": "gear", "size": int64(30)}), "grow"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Replay {
		t.Fatal("fresh legal update admitted as replay")
	}
	record := readRecord(t, fixture.store, "r1")
	if record.Resource.Generation() != 2 || record.Resource.Spec().Values()["size"] != int64(30) ||
		record.Status.State() != domain.ResourceStateReady {
		t.Fatalf("legal transition not applied: generation=%d spec=%+v state=%s",
			record.Resource.Generation(), record.Resource.Spec().Values(), record.Status.State())
	}
}

// TestDeleteAdmissionSkipsTransitionValidation pins that delete admissions —
// which carry no spec — never consult ValidateUpdate.
func TestDeleteAdmissionSkipsTransitionValidation(t *testing.T) {
	fixture := transitionFixture(t)
	calls := 0
	widget := fixture.catalog.types[domain.ResourceTypeRef{Name: "Widget", Version: "v1"}]
	widget.transitionFunc = func(oldSpec, newSpec domain.ResourceSpec) error {
		calls++
		return nil
	}
	command := application.DeleteResourceCommand{
		ID:                 domain.ResourceID("r1"),
		ExpectedGeneration: 1,
		OperationID:        domain.OperationID("op-del"),
		EventID:            domain.EventID("evt-del"),
		RequestedAt:        time.Date(2026, 8, 22, 10, 6, 0, 0, time.UTC),
		IdempotencyKey:     "key-del",
	}
	if _, err := fixture.service.DeleteResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("delete admission consulted transition validation %d times", calls)
	}
}

// TestReplayPrecedesTransitionRevalidation pins the approved precedence: a
// replay of a previously admitted update resolves from its Idempotency-Key
// even when the same old→new transition would be rejected if submitted fresh
// today. Replay identity is immutable history, not re-evaluated legality.
func TestReplayPrecedesTransitionRevalidation(t *testing.T) {
	fixture := transitionFixture(t)
	widget := fixture.catalog.types[domain.ResourceTypeRef{Name: "Widget", Version: "v1"}]

	grow := updateCommand("r1", 1, validSpec(map[string]any{"name": "gear", "size": int64(30)}), "grow")
	if _, err := fixture.service.UpdateResource(context.Background(), grow); err != nil {
		t.Fatal(err)
	}

	// The contract now rejects every transition, including the one the
	// original request legally performed.
	widget.transitionFunc = func(oldSpec, newSpec domain.ResourceSpec) error {
		return application.NewInvalidSpecError(widget.ref, []application.SpecViolation{{
			Path: "", Keyword: "transition", Message: "all transitions are currently illegal",
		}})
	}

	replay, err := fixture.service.UpdateResource(context.Background(), grow)
	if err != nil {
		t.Fatalf("replay of previously admitted update was blocked by current transition rules: %v", err)
	}
	if !replay.Replay {
		t.Fatal("expected replay result for identical command content")
	}
	if replay.Operation.ID() != domain.OperationID("op-grow") {
		t.Fatalf("replay operation = %s, want op-grow", replay.Operation.ID())
	}

	// A fresh submission of genuinely illegal content under a new key is
	// still rejected by current rules.
	shrink := updateCommand("r1", 2, validSpec(map[string]any{"name": "gear", "size": int64(29)}), "fresh-shrink")
	if _, err := fixture.service.UpdateResource(context.Background(), shrink); !errors.Is(err, application.ErrInvalidResourceSpec) {
		t.Fatalf("fresh illegal transition error = %v, want ErrInvalidResourceSpec", err)
	}
}
