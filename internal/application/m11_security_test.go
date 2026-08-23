// SPDX-License-Identifier: Apache-2.0

package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	appfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/auth"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
	"github.com/sithea-nou/liftr/internal/lifecycle"
	"github.com/sithea-nou/liftr/internal/provisioning"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
)

var securityTime = time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

func securitySpec(t *testing.T, value int64) domain.ResourceSpec {
	t.Helper()
	spec, err := domain.NewResourceSpec(map[string]any{"size": value})
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func securityCatalog(t *testing.T) application.ResourceTypeCatalog {
	t.Helper()
	typeValue, err := domain.NewResourceType(provisioningfake.ResourceType(), "security fixture",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		t.Fatal(err)
	}
	return appfake.Catalog{Types: map[domain.ResourceTypeRef]domain.ResourceType{provisioningfake.ResourceType(): typeValue}}
}

func securityService(t *testing.T, authorizer application.Authorizer) (*application.Service, *appfake.Store, *appfake.RecordingAuthorizer) {
	t.Helper()
	ref, err := application.NewProvisionerRef("security-provider")
	if err != nil {
		t.Fatal(err)
	}
	store := appfake.NewStore()
	recorder := &appfake.RecordingAuthorizer{AllowAll: appfake.AllowAll{}}
	selected := authorizer
	if selected == nil {
		selected = recorder
	}
	service, err := application.NewService(securityCatalog(t), &appfake.Selector{Ref: ref},
		&appfake.Resolver{Providers: map[application.ProvisionerRef]provisioning.Provisioner{
			ref: provisioningfake.New(provisioningfake.ModeSynchronous),
		}}, store, selected)
	if err != nil {
		t.Fatal(err)
	}
	return service, store, recorder
}

func memberOf(team string) identity.Principal {
	return appfake.Principal("user-"+team, appfake.Owner("team", team))
}

func TestUnauthorizedCreateIsDeniedBeforeAnyDurableEffect(t *testing.T) {
	deny := &appfake.DenyAll{}
	service, store, _ := securityService(t, deny)

	command := application.CreateResourceCommand{
		Actor:          memberOf("payments"),
		ID:             "resource-denied",
		Type:           provisioningfake.ResourceType(),
		Owner:          domain.OwnerRef{Kind: "team", ID: "payments"},
		Spec:           securitySpec(t, 1),
		OperationID:    "op-denied",
		EventID:        "evt-denied",
		RequestedAt:    securityTime,
		IdempotencyKey: "denied-key",
	}
	if _, err := service.AdmitCreateResource(context.Background(), command); !errors.Is(err, application.ErrNotAuthorized) {
		t.Fatalf("unauthorized create error = %v, want ErrNotAuthorized", err)
	}
	if records := store.RecordCounts(); records.Resources != 0 || records.Operations != 0 || records.Events != 0 || records.Idempotency != 0 {
		t.Fatalf("denied create left durable effects: %+v", records)
	}
}

func TestAuthorizationPrecedesIdempotencyReplayForCreate(t *testing.T) {
	// A principal admits a create under its owner with key K. After the
	// membership is revoked, replaying the identical request must be denied —
	// possession of K is not authorization.
	recorder := &appfake.RecordingAuthorizer{AllowAll: appfake.AllowAll{}}
	service, _, _ := securityService(t, recorder)
	ctx := context.Background()

	command := application.CreateResourceCommand{
		Actor:          memberOf("payments"),
		ID:             "resource-revoke",
		Type:           provisioningfake.ResourceType(),
		Owner:          domain.OwnerRef{Kind: "team", ID: "payments"},
		Spec:           securitySpec(t, 2),
		OperationID:    "op-revoke",
		EventID:        "evt-revoke",
		RequestedAt:    securityTime,
		IdempotencyKey: "revoke-key",
	}
	if _, err := service.AdmitCreateResource(ctx, command); err != nil {
		t.Fatal(err)
	}
	admissionInvocations := recorder.Invocations

	recorder.Denied = map[identity.Action]error{identity.ActionResourceCreate: errors.New("revoked")}
	replay := command
	replay.OperationID = "op-replay-attempt"
	replay.EventID = "evt-replay-attempt"
	if _, err := service.AdmitCreateResource(ctx, replay); !errors.Is(err, application.ErrNotAuthorized) {
		t.Fatalf("revoked replay error = %v, want ErrNotAuthorized", err)
	}
	if recorder.Invocations != admissionInvocations+1 {
		t.Fatalf("authorizer invocations = %d, want exactly one additional call", recorder.Invocations)
	}
}

func TestUpdateDenialHidesGenerationConflict(t *testing.T) {
	recorder := &appfake.RecordingAuthorizer{AllowAll: appfake.AllowAll{}}
	service, _, _ := securityService(t, recorder)
	ctx := context.Background()

	command := application.CreateResourceCommand{
		Actor:          memberOf("platform"),
		ID:             "resource-gen",
		Type:           provisioningfake.ResourceType(),
		Owner:          domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec:           securitySpec(t, 3),
		OperationID:    "op-gen-create",
		EventID:        "evt-gen-create",
		RequestedAt:    securityTime,
		IdempotencyKey: "gen-key",
	}
	if _, err := service.AdmitCreateResource(ctx, command); err != nil {
		t.Fatal(err)
	}

	// A stale generation AND a denial must answer the denial only; the caller
	// learns nothing about current generation or conflicts.
	recorder.Denied = map[identity.Action]error{identity.ActionResourceUpdate: errors.New("no membership")}
	stale := application.UpdateResourceCommand{
		Actor:              memberOf("platform"),
		ID:                 command.ID,
		ExpectedGeneration: 99,
		Spec:               securitySpec(t, 4),
		OperationID:        "op-stale-update",
		EventID:            "evt-stale-update",
		RequestedAt:        securityTime.Add(time.Minute),
		IdempotencyKey:     "stale-key",
	}
	_, denied := service.AdmitUpdateResource(ctx, stale)
	if !errors.Is(denied, application.ErrNotAuthorized) {
		t.Fatalf("denied update error = %v, want ErrNotAuthorized before generation evaluation", denied)
	}
	if errors.Is(denied, application.ErrConcurrencyConflict) {
		t.Fatal("denied update leaked generation conflict semantics")
	}

	// The authorized caller with the same stale generation sees the conflict.
	recorder.Denied = nil
	_, conflict := service.AdmitUpdateResource(ctx, stale)
	if !errors.Is(conflict, application.ErrConcurrencyConflict) {
		t.Fatalf("authorized stale update error = %v, want ErrConcurrencyConflict", conflict)
	}
}

func TestReadAuthorizationFollowsStoredOwner(t *testing.T) {
	// This pin uses the real owner-membership policy so structural
	// membership decides, not a permissive test fake.
	service, _, _ := securityService(t, auth.OwnerAuthorizer{})
	ctx := context.Background()

	command := application.CreateResourceCommand{
		Actor:          memberOf("payments"),
		ID:             "resource-read-authz",
		Type:           provisioningfake.ResourceType(),
		Owner:          domain.OwnerRef{Kind: "team", ID: "payments"},
		Spec:           securitySpec(t, 5),
		OperationID:    "op-read-authz",
		EventID:        "evt-read-authz",
		RequestedAt:    securityTime,
		IdempotencyKey: "read-authz-key",
	}
	result, err := service.AdmitCreateResource(ctx, command)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.GetResource(ctx, memberOf("platform"), command.ID); !errors.Is(err, application.ErrNotAuthorized) {
		t.Fatalf("non-member read error = %v, want ErrNotAuthorized", err)
	}
	record, err := service.GetResource(ctx, memberOf("payments"), command.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Resource.ID() != command.ID {
		t.Fatalf("member read returned %q", record.Resource.ID())
	}

	// Operation reads authorize through the owning Resource.
	if _, err := service.GetOperation(ctx, memberOf("platform"), result.Operation.ID()); !errors.Is(err, application.ErrNotAuthorized) {
		t.Fatalf("non-member operation read error = %v, want ErrNotAuthorized via owning Resource", err)
	}
	if _, err := service.GetOperation(ctx, memberOf("payments"), result.Operation.ID()); err != nil {
		t.Fatalf("member operation read failed: %v", err)
	}
}

func TestDiscoveryRequiresAuthenticationButIsGloballyReadable(t *testing.T) {
	service, _, _ := securityService(t, nil)
	ctx := context.Background()

	var anonymous identity.Principal
	if _, err := service.ListResourceTypes(ctx, anonymous); !errors.Is(err, application.ErrNotAuthorized) {
		t.Fatalf("anonymous discovery error = %v, want ErrNotAuthorized", err)
	}
	principal := memberOf("nobody")
	if contracts, err := service.ListResourceTypes(ctx, principal); err != nil || len(contracts) == 0 {
		t.Fatalf("authenticated discovery = %v, %v", contracts, err)
	}
}

func TestAdmittedWorkContinuesWithoutReauthorization(t *testing.T) {
	recorder := &appfake.RecordingAuthorizer{AllowAll: appfake.AllowAll{}}
	service, _, _ := securityService(t, recorder)
	service.EnableEagerExecutionForTesting()
	ctx := context.Background()

	command := application.CreateResourceCommand{
		Actor:          memberOf("payments"),
		ID:             "resource-worker",
		Type:           provisioningfake.ResourceType(),
		Owner:          domain.OwnerRef{Kind: "team", ID: "payments"},
		Spec:           securitySpec(t, 6),
		OperationID:    "op-worker",
		EventID:        "evt-worker",
		RequestedAt:    securityTime,
		IdempotencyKey: "worker-key",
	}
	result, err := service.CreateResource(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	if result.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("operation state = %s, want Succeeded", result.Operation.State())
	}
	admissionInvocations := recorder.Invocations

	// Revoke everything after admission; already-admitted work continues and
	// no further authorization calls occur.
	recorder.Denied = map[identity.Action]error{
		identity.ActionResourceCreate:   errors.New("revoked"),
		identity.ActionResourceRead:     errors.New("revoked"),
		identity.ActionResourceUpdate:   errors.New("revoked"),
		identity.ActionResourceDelete:   errors.New("revoked"),
		identity.ActionResourceTypeRead: errors.New("revoked"),
	}
	if _, err := service.GetResource(ctx, memberOf("payments"), command.ID); !errors.Is(err, application.ErrNotAuthorized) {
		t.Fatalf("post-admission read should still consult policy once: %v", err)
	}
	if recorder.Invocations != admissionInvocations+1 {
		t.Fatalf("worker execution consulted the authorizer post-admission: invocations = %d", recorder.Invocations)
	}
}

func TestPerPrincipalIdempotencyNamespaces(t *testing.T) {
	service, _, _ := securityService(t, nil)
	ctx := context.Background()

	first := application.CreateResourceCommand{
		Actor:          memberOf("payments"),
		ID:             "resource-principal-a",
		Type:           provisioningfake.ResourceType(),
		Owner:          domain.OwnerRef{Kind: "team", ID: "payments"},
		Spec:           securitySpec(t, 7),
		OperationID:    "op-principal-a",
		EventID:        "evt-principal-a",
		RequestedAt:    securityTime,
		IdempotencyKey: "shared-key",
	}
	resultA, err := service.AdmitCreateResource(ctx, first)
	if err != nil {
		t.Fatal(err)
	}

	// Principal B uses byte-equivalent content including A's owner and the
	// SAME key. Scoping means B never resolves A's record; B's admission is
	// independent (and here lands on an already-taken resource ID).
	second := first
	second.Actor = memberOf("platform")
	second.OperationID = "op-principal-b"
	second.EventID = "evt-principal-b"
	_, err = service.AdmitCreateResource(ctx, second)
	if !errors.Is(err, lifecycle.ErrOperationActive) && !errors.Is(err, application.ErrResourceNotFound) {
		// The exact failure mode of B's fresh admission is irrelevant to this
		// pin: what matters is that it is NOT a replay of A's result.
		t.Fatalf("principal B unexpectedly resolved within its own namespace: %v", err)
	}

	// A replays its own key and receives its own original Operation.
	replayed, err := service.AdmitCreateResource(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replay || replayed.Operation.ID() != resultA.Operation.ID() {
		t.Fatalf("A replay = %+v, want replay of original operation %q", replayed.Replay, resultA.Operation.ID())
	}
}

func TestLegacyControlPlaneScopeIsUnreachableByPrincipals(t *testing.T) {
	service, store, _ := securityService(t, nil)
	ctx := context.Background()

	// An anonymous-era record under the legacy scope must never resolve for
	// any authenticated principal namespace.
	err := store.Within(ctx, func(tx application.UnitOfWork) error {
		resource, resourceErr := domain.NewResource("legacy-resource", provisioningfake.ResourceType(),
			domain.OwnerRef{Kind: "team", ID: "legacy"}, securitySpec(t, 8), securityTime)
		if resourceErr != nil {
			return resourceErr
		}
		status, statusErr := domain.NewResourceStatus(resource.ID(), 0, domain.ResourceStateUnknown, nil, securityTime)
		if statusErr != nil {
			return statusErr
		}
		transition, transitionErr := (lifecycle.Engine{}).Request(resource, securityLifecycleType(t), status, nil, domain.CapabilityCreate, "legacy-op", "legacy-evt", securityTime)
		if transitionErr != nil {
			return transitionErr
		}
		if createErr := tx.Resources().CreateResource(ctx, application.ResourceRecord{Resource: resource, Status: transition.Status, ProvisionerRef: securityProvisionerRef(t), Version: 1}); createErr != nil {
			return createErr
		}
		if err := tx.Operations().CreateOperation(ctx, application.OperationRecord{Operation: transition.Operation, Version: 1}); err != nil {
			return err
		}
		return tx.Idempotency().PutIdempotency(ctx, application.IdempotencyRecord{
			Scope: "control-plane", Key: "legacy-key", Fingerprint: "legacy-fingerprint",
			CommandKind: "create", ResourceID: resource.ID(), OperationID: transition.Operation.ID(),
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	command := application.CreateResourceCommand{
		Actor:          memberOf("legacy"),
		ID:             "fresh-resource",
		Type:           provisioningfake.ResourceType(),
		Owner:          domain.OwnerRef{Kind: "team", ID: "legacy"},
		Spec:           securitySpec(t, 9),
		OperationID:    "op-fresh",
		EventID:        "evt-fresh",
		RequestedAt:    securityTime,
		IdempotencyKey: "legacy-key",
	}
	result, err := service.AdmitCreateResource(ctx, command)
	if err != nil {
		t.Fatalf("fresh admission collided with retired legacy scope: %v", err)
	}
	if result.Replay {
		t.Fatal("a principal resolved a record persisted under the retired control-plane scope")
	}
}

func TestAdmissionEventRecordsNormalizedActorOnly(t *testing.T) {
	service, store, _ := securityService(t, nil)
	ctx := context.Background()

	actor := memberOf("payments")
	command := application.CreateResourceCommand{
		Actor:          actor,
		ID:             "resource-audit",
		Type:           provisioningfake.ResourceType(),
		Owner:          domain.OwnerRef{Kind: "team", ID: "payments"},
		Spec:           securitySpec(t, 10),
		OperationID:    "op-audit",
		EventID:        "evt-audit",
		RequestedAt:    securityTime,
		IdempotencyKey: "audit-key",
	}
	if _, err := service.AdmitCreateResource(ctx, command); err != nil {
		t.Fatal(err)
	}
	event, err := store.GetEvent(context.Background(), "evt-audit")
	if err != nil {
		t.Fatalf("admission event was not persisted: %v", err)
	}
	recorded, present := event.Actor()
	if !present {
		t.Fatal("admission event carries no actor")
	}
	if recorded.ID != string(actor.ID) || recorded.Kind != string(identity.KindUser) {
		t.Fatalf("actor = %+v, want id=%q kind=%q", recorded, actor.ID, actor.Kind)
	}
	if len(recorded.ID) > len(string(actor.ID))+8 {
		t.Fatal("actor payload grew beyond the normalized fields")
	}
}

func securityLifecycleType(t *testing.T) domain.ResourceType {
	t.Helper()
	typeValue, err := domain.NewResourceType(provisioningfake.ResourceType(), "security fixture",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		t.Fatal(err)
	}
	return typeValue
}

func securityProvisionerRef(t *testing.T) application.ProvisionerRef {
	t.Helper()
	ref, err := application.NewProvisionerRef("security-provider")
	if err != nil {
		t.Fatal(err)
	}
	return ref
}
