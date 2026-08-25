// SPDX-License-Identifier: Apache-2.0

package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/policy"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
)

func TestCreateQuotaDenialRollsBackAndDoesNotClaimIdempotency(t *testing.T) {
	service, store, _, _ := newService(t, mustProvisionerRef(t, "provider-policy-quota"), provisioningfake.New(provisioningfake.ModeSynchronous))
	service.AdmissionPolicy = parseAdmissionPolicy(t, `{
		"apiVersion":"liftr.dev/admission-policy/v1",
		"rules":[{"id":"one-per-owner","kind":"resource-count-quota","limit":1}]
	}`)

	first := policyCreateCommand(t, "quota-first", "quota-first-operation", "quota-first-event", "first-key")
	if _, err := service.AdmitCreateResource(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	before := store.RecordCounts()
	second := policyCreateCommand(t, "quota-second", "quota-second-operation", "quota-second-event", "denied-key")
	if _, err := service.AdmitCreateResource(context.Background(), second); !errors.Is(err, application.ErrQuotaExceeded) {
		t.Fatalf("quota error = %v", err)
	}
	if after := store.RecordCounts(); after != before {
		t.Fatalf("denied admission persisted records: before=%+v after=%+v", before, after)
	}

	service.AdmissionPolicy = application.NoRestrictionsAdmissionPolicy{}
	if _, err := service.AdmitCreateResource(context.Background(), second); err != nil {
		t.Fatalf("same idempotency key after policy change = %v", err)
	}
}

func TestSuccessfulReplayBypassesChangedPolicy(t *testing.T) {
	service, store, _, _ := newService(t, mustProvisionerRef(t, "provider-policy-replay"), provisioningfake.New(provisioningfake.ModeSynchronous))
	command := policyCreateCommand(t, "replay-resource", "replay-operation", "replay-event", "replay-key")
	first, err := service.AdmitCreateResource(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	event, err := store.GetEvent(context.Background(), command.EventID)
	if err != nil {
		t.Fatal(err)
	}
	admission, present := event.Admission()
	if !present || admission.PolicyRevision != string(application.NoRestrictionsAdmissionPolicy{}.Revision()) {
		t.Fatalf("event admission = %+v present=%t", admission, present)
	}

	service.AdmissionPolicy = parseAdmissionPolicy(t, `{
		"apiVersion":"liftr.dev/admission-policy/v1",
		"rules":[{"id":"deny-create","kind":"capability-deny","resourceType":{"name":"FakeResource","version":"v1"},"capabilities":["create"]}]
	}`)
	replay, err := service.AdmitCreateResource(context.Background(), command)
	if err != nil {
		t.Fatalf("replay under changed policy = %v", err)
	}
	if !replay.Replay || replay.Operation.ID() != first.Operation.ID() {
		t.Fatalf("replay = %+v", replay)
	}
}

func TestUpdateCapabilityPolicyDenialPersistsNothing(t *testing.T) {
	service, store, _, _ := newService(t, mustProvisionerRef(t, "provider-policy-update"), provisioningfake.New(provisioningfake.ModeSynchronous))
	created, err := service.CreateResource(context.Background(), policyCreateCommand(t, "update-resource", "update-create-operation", "update-create-event", "update-create-key"))
	if err != nil {
		t.Fatal(err)
	}
	before := store.RecordCounts()
	service.AdmissionPolicy = parseAdmissionPolicy(t, `{
		"apiVersion":"liftr.dev/admission-policy/v1",
		"rules":[{"id":"deny-update","kind":"capability-deny","resourceType":{"name":"FakeResource","version":"v1"},"capabilities":["update"]}]
	}`)
	_, err = service.AdmitUpdateResource(context.Background(), application.UpdateResourceCommand{
		Actor: fake.Principal("tester"), ID: created.Resource.Resource.ID(),
		ExpectedGeneration: created.Resource.Resource.Generation(), Spec: testSpec(t),
		OperationID: "update-denied-operation", EventID: "update-denied-event",
		RequestedAt: applicationTime.Add(time.Minute), IdempotencyKey: "update-denied-key",
	})
	if !errors.Is(err, application.ErrPolicyDenied) {
		t.Fatalf("update policy error = %v", err)
	}
	if after := store.RecordCounts(); after != before {
		t.Fatalf("denied update persisted records: before=%+v after=%+v", before, after)
	}
}

func TestSuccessfulUpdateEventRecordsPolicyRevision(t *testing.T) {
	service, store, _, _ := newService(t, mustProvisionerRef(t, "provider-policy-update-provenance"), provisioningfake.New(provisioningfake.ModeSynchronous))
	created, err := service.CreateResource(context.Background(), policyCreateCommand(t, "update-provenance-resource", "update-provenance-create-operation", "update-provenance-create-event", "update-provenance-create-key"))
	if err != nil {
		t.Fatal(err)
	}
	compiled := parseAdmissionPolicy(t, `{"apiVersion":"liftr.dev/admission-policy/v1","rules":[]}`)
	service.AdmissionPolicy = compiled
	command := application.UpdateResourceCommand{
		Actor: fake.Principal("tester"), ID: created.Resource.Resource.ID(),
		ExpectedGeneration: created.Resource.Resource.Generation(), Spec: testSpec(t),
		OperationID: "update-provenance-operation", EventID: "update-provenance-event",
		RequestedAt: applicationTime.Add(time.Minute), IdempotencyKey: "update-provenance-key",
	}
	if _, err := service.UpdateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	event, err := store.GetEvent(context.Background(), command.EventID)
	if err != nil {
		t.Fatal(err)
	}
	admission, present := event.Admission()
	if !present || admission.PolicyRevision != string(compiled.Revision()) {
		t.Fatalf("event admission=%+v present=%t", admission, present)
	}
}

func TestFakeQuotaFactsFailClosedForMissingStatus(t *testing.T) {
	service, store, _, _ := newService(t, mustProvisionerRef(t, "provider-policy-corruption"), provisioningfake.New(provisioningfake.ModeSynchronous))
	service.AdmissionPolicy = parseAdmissionPolicy(t, `{
		"apiVersion":"liftr.dev/admission-policy/v1",
		"rules":[{"id":"bounded-owner","kind":"resource-count-quota","limit":100}]
	}`)
	corrupt, err := domain.NewResource("corrupt-resource", provisioningfake.ResourceType(), domain.OwnerRef{Kind: "team", ID: "platform"}, testSpec(t), applicationTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateResource(context.Background(), application.ResourceRecord{Resource: corrupt, Version: 1}); err != nil {
		t.Fatal(err)
	}
	command := policyCreateCommand(t, "after-corruption", "after-corruption-operation", "after-corruption-event", "after-corruption-key")
	if _, err := service.AdmitCreateResource(context.Background(), command); !errors.Is(err, application.ErrQuotaInvariant) {
		t.Fatalf("corrupt fake quota admission error = %v", err)
	}
}

func parseAdmissionPolicy(t *testing.T, raw string) *policy.Policy {
	t.Helper()
	compiled, err := policy.Parse([]byte(raw), []domain.ResourceTypeRef{provisioningfake.ResourceType()})
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func policyCreateCommand(t *testing.T, id domain.ResourceID, operationID domain.OperationID, eventID domain.EventID, key string) application.CreateResourceCommand {
	t.Helper()
	return application.CreateResourceCommand{
		Actor: fake.Principal("tester"), ID: id, Type: provisioningfake.ResourceType(),
		Owner: domain.OwnerRef{Kind: "team", ID: "platform"}, Spec: testSpec(t),
		OperationID: operationID, EventID: eventID, RequestedAt: applicationTime, IdempotencyKey: key,
	}
}
