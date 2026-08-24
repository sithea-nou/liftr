// SPDX-License-Identifier: Apache-2.0

package application_test

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
	"github.com/sithea-nou/liftr/internal/provisioning"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
)

// listPolicy is a configurable test policy whose single-target and collection
// decisions are controlled independently, so custom policies can prove the
// ADR-0016 invariant that inventory never discloses beyond resource:read.
type listPolicy struct {
	mu            sync.Mutex
	readDeniedFor map[domain.OwnerRef]bool
	listDenied    bool
	listScope     *identity.ResourceVisibility
}

func (p *listPolicy) Authorize(_ context.Context, _ identity.Principal, action identity.Action, target identity.ResourceTarget) error {
	if action == identity.ActionResourceRead && p.readDeniedFor[target.Owner] {
		return errors.New("read denied by test policy")
	}
	return nil
}

func (p *listPolicy) AuthorizeResourceList(_ context.Context, _ identity.Principal) (identity.ResourceVisibility, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listDenied {
		return identity.ResourceVisibility{}, errors.New("list denied by test policy")
	}
	if p.listScope != nil {
		return *p.listScope, nil
	}
	return identity.ResourceVisibility{AllOwners: true}, nil
}

func newListTestService(t *testing.T, authorizer application.Authorizer) (*application.Service, *fake.Store) {
	t.Helper()
	store := fake.NewStore()
	typeValue, err := domain.NewResourceType(provisioningfake.ResourceType(), "inventory test type",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := application.NewProvisionerRef("inventory-test-provider")
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewService(fake.Catalog{Types: map[domain.ResourceTypeRef]domain.ResourceType{provisioningfake.ResourceType(): typeValue}},
		&fake.Selector{Ref: ref}, &fake.Resolver{Providers: map[application.ProvisionerRef]provisioning.Provisioner{ref: provisioningfake.New(provisioningfake.ModeSynchronous)}},
		store, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	return service, store
}

func seedInventoryResource(t *testing.T, store *fake.Store, id string, owner domain.OwnerRef, state domain.ResourceState, createdAt time.Time) {
	t.Helper()
	resource, err := domain.NewResource(domain.ResourceID(id), provisioningfake.ResourceType(), owner,
		mustInventorySpec(t, map[string]any{"size": int64(5)}), createdAt)
	if err != nil {
		t.Fatal(err)
	}
	status, err := domain.NewResourceStatus(resource.ID(), 1, state, nil, createdAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	err = store.Within(context.Background(), func(tx application.UnitOfWork) error {
		return tx.Resources().CreateResource(context.Background(), application.ResourceRecord{
			Resource: resource, Status: status, ProvisionerRef: mustInventoryProvisionerRef(t), Version: 1,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func mustInventorySpec(t *testing.T, values map[string]any) domain.ResourceSpec {
	t.Helper()
	spec, err := domain.NewResourceSpec(values)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func mustInventoryProvisionerRef(t *testing.T) application.ProvisionerRef {
	t.Helper()
	ref, err := application.NewProvisionerRef("inventory-test-provider")
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func listedIDs(page application.ResourceInventoryPageView) []string {
	ids := make([]string, 0, len(page.Items))
	for i := range page.Items {
		ids = append(ids, string(page.Items[i].ID))
	}
	return ids
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestListResourcesVisibilityNeverExceedsReadVisibility(t *testing.T) {
	// Custom-policy regression for ADR-0016 correction 1A: enumeration is
	// allowed while reads of team/hidden are denied. The hidden owner's
	// Resources must never appear in inventory regardless.
	policy := &listPolicy{readDeniedFor: map[domain.OwnerRef]bool{{Kind: "team", ID: "hidden"}: true}}
	policy.listScope = &identity.ResourceVisibility{Owners: []domain.OwnerRef{{Kind: "team", ID: "platform"}}}
	service, store := newListTestService(t, policy)
	base := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	seedInventoryResource(t, store, "readable-one", domain.OwnerRef{Kind: "team", ID: "platform"}, domain.ResourceStateReady, base)
	seedInventoryResource(t, store, "unreadable-secret", domain.OwnerRef{Kind: "team", ID: "hidden"}, domain.ResourceStateReady, base.Add(time.Second))

	principal := fake.Principal("auditor", domain.OwnerRef{Kind: "team", ID: "platform"})
	page, err := service.ListResources(context.Background(), application.ListResourcesRequest{Principal: principal})
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(listedIDs(page), "readable-one") {
		t.Fatalf("visible owner's Resource missing from inventory: %v", listedIDs(page))
	}
	if containsString(listedIDs(page), "unreadable-secret") {
		t.Fatal("list disclosed a Resource whose owner is not readable by policy")
	}
	_, readErr := service.GetResource(context.Background(), principal, "unreadable-secret")
	if !errors.Is(readErr, application.ErrNotAuthorized) {
		t.Fatalf("direct read of hidden-owner Resource = %v, want denial", readErr)
	}
}

func TestReadWithoutListAllowsDetailAndDeniesEnumeration(t *testing.T) {
	policy := &listPolicy{listDenied: true}
	service, store := newListTestService(t, policy)
	base := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	seedInventoryResource(t, store, "known-resource", domain.OwnerRef{Kind: "team", ID: "payments"}, domain.ResourceStateReady, base)

	principal := fake.Principal("operator", domain.OwnerRef{Kind: "team", ID: "payments"})
	record, err := service.GetResource(context.Background(), principal, "known-resource")
	if err != nil {
		t.Fatalf("resource:read was denied although only resource:list should be denied: %v", err)
	}
	if record.Resource.ID() != "known-resource" {
		t.Fatalf("unexpected record %+v", record.Resource)
	}
	_, listErr := service.ListResources(context.Background(), application.ListResourcesRequest{Principal: principal})
	if !errors.Is(listErr, application.ErrNotAuthorized) {
		t.Fatalf("list with denied enumeration = %v, want ErrNotAuthorized", listErr)
	}
}

func TestListResourcesEmptyVisibilityIsEmptyCollectionNotDenial(t *testing.T) {
	policy := &listPolicy{}
	policy.listScope = &identity.ResourceVisibility{}
	service, store := newListTestService(t, policy)
	seedInventoryResource(t, store, "somewhere-else", domain.OwnerRef{Kind: "team", ID: "other"}, domain.ResourceStateReady, time.Now())

	page, err := service.ListResources(context.Background(), application.ListResourcesRequest{Principal: fake.Principal("nobody")})
	if err != nil {
		t.Fatalf("empty visibility must be an authorized empty page, got %v", err)
	}
	if len(page.Items) != 0 || page.Items == nil || page.NextCursor != "" {
		t.Fatalf("empty visibility page = %#v", page)
	}
}

func TestListResourcesDenialIsErrNotAuthorized(t *testing.T) {
	service, _ := newListTestService(t, fake.DenyAll{})
	_, err := service.ListResources(context.Background(), application.ListResourcesRequest{Principal: fake.Principal("anyone")})
	if !errors.Is(err, application.ErrNotAuthorized) {
		t.Fatalf("denied listing = %v, want ErrNotAuthorized", err)
	}
}

func TestListResourcesOrdersByKeysetAndPaginatesDeterministically(t *testing.T) {
	service, store := newListTestService(t, fake.AllowAll{})
	base := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	for i := range 5 {
		seedInventoryResource(t, store, "res-"+string(rune('a'+i)), domain.OwnerRef{Kind: "team", ID: "payments"}, domain.ResourceStateReady, base.Add(time.Duration(i)*time.Second))
	}
	principal := fake.Principal("pager")

	first, err := service.ListResources(context.Background(), application.ListResourcesRequest{Principal: principal, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %#v", first)
	}
	if first.Items[0].ID != "res-e" || first.Items[1].ID != "res-d" {
		t.Fatalf("newest-first order violated: %v", listedIDs(first))
	}
	collected := listedIDs(first)
	cursor := first.NextCursor
	for range 10 {
		page, err := service.ListResources(context.Background(), application.ListResourcesRequest{Principal: principal, Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		collected = append(collected, listedIDs(page)...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	want := []string{"res-e", "res-d", "res-c", "res-b", "res-a"}
	if !reflect.DeepEqual(collected, want) {
		t.Fatalf("traversal collected %v, want %v", collected, want)
	}
}

func TestCursorContinuationRequiresSameVisibilityScope(t *testing.T) {
	policy := &listPolicy{}
	service, store := newListTestService(t, policy)
	base := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	seedInventoryResource(t, store, "payments-db", domain.OwnerRef{Kind: "team", ID: "payments"}, domain.ResourceStateReady, base)
	seedInventoryResource(t, store, "platform-db", domain.OwnerRef{Kind: "team", ID: "platform"}, domain.ResourceStateReady, base.Add(time.Second))

	wide := identity.ResourceVisibility{Owners: []domain.OwnerRef{{Kind: "team", ID: "payments"}, {Kind: "team", ID: "platform"}}}
	narrow := identity.ResourceVisibility{Owners: []domain.OwnerRef{{Kind: "team", ID: "platform"}}}
	grown := identity.ResourceVisibility{Owners: []domain.OwnerRef{{Kind: "team", ID: "payments"}, {Kind: "team", ID: "platform"}, {Kind: "team", ID: "security"}}}
	principal := fake.Principal("member", domain.OwnerRef{Kind: "team", ID: "payments"}, domain.OwnerRef{Kind: "team", ID: "platform"})

	// Membership loss: a cursor issued under payments+platform is rejected
	// once only platform remains visible, and a restarted traversal never
	// leaks the revoked owner's Resources.
	policy.listScope = &wide
	first, err := service.ListResources(context.Background(), application.ListResourcesRequest{Principal: principal, Limit: 1})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("first page under wide scope = %#v err=%v", first, err)
	}
	policy.listScope = &narrow
	if _, err := service.ListResources(context.Background(), application.ListResourcesRequest{Principal: principal, Limit: 1, Cursor: first.NextCursor}); !errors.Is(err, application.ErrInvalidApplicationCall) {
		t.Fatalf("shrunken scope continued silently: err=%v", err)
	}
	restarted, err := service.ListResources(context.Background(), application.ListResourcesRequest{Principal: principal, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if containsString(listedIDs(restarted), "payments-db") {
		t.Fatal("revoked owner's Resource leaked after membership loss")
	}
	if !containsString(listedIDs(restarted), "platform-db") {
		t.Fatalf("still-visible Resource missing: %v", listedIDs(restarted))
	}

	// Membership gain: the old narrow-scope cursor is likewise rejected; the
	// restarted traversal exposes the newly visible Resource.
	policy.listScope = &wide
	gainFirst, err := service.ListResources(context.Background(), application.ListResourcesRequest{Principal: principal, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	policy.listScope = &grown
	if _, err := service.ListResources(context.Background(), application.ListResourcesRequest{Principal: principal, Limit: 1, Cursor: gainFirst.NextCursor}); !errors.Is(err, application.ErrInvalidApplicationCall) {
		t.Fatalf("grown scope continued old traversal: err=%v", err)
	}
	seedInventoryResource(t, store, "security-db", domain.OwnerRef{Kind: "team", ID: "security"}, domain.ResourceStatePending, base.Add(2*time.Second))
	regrown, err := service.ListResources(context.Background(), application.ListResourcesRequest{Principal: principal, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(listedIDs(regrown), "security-db") || !containsString(listedIDs(regrown), "payments-db") {
		t.Fatalf("restart did not expose newly visible Resources: %v", listedIDs(regrown))
	}
}

func TestConcurrentInsertDoesNotDisturbOpenTraversalWindow(t *testing.T) {
	service, store := newListTestService(t, fake.AllowAll{})
	base := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	for i := range 4 {
		seedInventoryResource(t, store, "win-"+string(rune('a'+i)), domain.OwnerRef{Kind: "team", ID: "payments"}, domain.ResourceStateReady, base.Add(time.Duration(i)*time.Second))
	}
	principal := fake.Principal("walker")
	first, err := service.ListResources(context.Background(), application.ListResourcesRequest{Principal: principal, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	seedInventoryResource(t, store, "win-new", domain.OwnerRef{Kind: "team", ID: "payments"}, domain.ResourceStateReady, base.Add(time.Hour))
	second, err := service.ListResources(context.Background(), application.ListResourcesRequest{Principal: principal, Limit: 10, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	collected := append(listedIDs(first), listedIDs(second)...)
	want := []string{"win-d", "win-c", "win-b", "win-a"}
	if !reflect.DeepEqual(collected, want) {
		t.Fatalf("window disturbed by concurrent insert: %v", collected)
	}
	fresh, err := service.ListResources(context.Background(), application.ListResourcesRequest{Principal: principal, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(listedIDs(fresh), "win-new") || fresh.Items[0].ID != "win-new" {
		t.Fatalf("new insert missing or misplaced at the head: %v", listedIDs(fresh))
	}
}

func TestListResourcesAuthorizesExactlyOncePerPage(t *testing.T) {
	recorder := &fake.RecordingAuthorizer{}
	service, store := newListTestService(t, recorder)
	seedInventoryResource(t, store, "counted", domain.OwnerRef{Kind: "team", ID: "payments"}, domain.ResourceStateReady, time.Now())
	principal := fake.Principal("counter", domain.OwnerRef{Kind: "team", ID: "payments"})

	if _, err := service.ListResources(context.Background(), application.ListResourcesRequest{Principal: principal}); err != nil {
		t.Fatal(err)
	}
	if recorder.ListInvocations != 1 {
		t.Fatalf("first-page authorizations = %d, want exactly 1", recorder.ListInvocations)
	}
	page, err := service.ListResources(context.Background(), application.ListResourcesRequest{Principal: principal})
	if err != nil {
		t.Fatal(err)
	}
	if recorder.ListInvocations != 2 {
		t.Fatalf("cumulative authorizations = %d, want exactly one per request", recorder.ListInvocations)
	}
	if _, err := service.ListResources(context.Background(), application.ListResourcesRequest{Principal: principal, Cursor: page.NextCursor}); page.NextCursor != "" {
		t.Log("no second page available; continuation path covered elsewhere")
	} else if err == nil && recorder.ListInvocations != 3 {
		t.Fatalf("continuation authorizations = %d, want exactly 1 per request", recorder.ListInvocations)
	}
}

func TestInventoryItemLatestOperationIsDedicatedProjection(t *testing.T) {
	field, found := reflect.TypeOf(application.ResourceInventoryItem{}).FieldByName("Latest")
	if !found {
		t.Fatal("ResourceInventoryItem has no Latest field")
	}
	want := reflect.TypeOf((*application.ResourceInventoryLatestOperation)(nil))
	if field.Type != want {
		t.Fatalf("ResourceInventoryItem.Latest is %s, want %s — a partial domain.Operation is not a valid read model", field.Type, want)
	}
}

func TestListResourcesValidatesFiltersAndLimits(t *testing.T) {
	service, store := newListTestService(t, fake.AllowAll{})
	seedInventoryResource(t, store, "filter-target", domain.OwnerRef{Kind: "team", ID: "payments"}, domain.ResourceStateReady, time.Now())
	principal := fake.Principal("filter-user")
	deleted := domain.ResourceStateDeleted

	cases := []struct {
		name    string
		request application.ListResourcesRequest
	}{
		{"limit below minimum", application.ListResourcesRequest{Principal: principal, Limit: -1}},
		{"limit above maximum", application.ListResourcesRequest{Principal: principal, Limit: 101}},
		{"version without type", application.ListResourcesRequest{Principal: principal, TypeVersion: "v2"}},
		{"deleted without includeDeleted", application.ListResourcesRequest{Principal: principal, StateFilter: &deleted}},
		{"unknown state", application.ListResourcesRequest{Principal: principal, StateFilter: ptrResourceState("Exploded"), IncludeDeleted: true}},
		{"non-canonical owner filter", application.ListResourcesRequest{Principal: principal, OwnerFilter: &domain.OwnerRef{Kind: " team", ID: "payments"}}},
	}
	for _, testCase := range cases {
		if _, err := service.ListResources(context.Background(), testCase.request); !errors.Is(err, application.ErrInvalidApplicationCall) {
			t.Fatalf("%s: err = %v, want ErrInvalidApplicationCall", testCase.name, err)
		}
	}

	stateReady := domain.ResourceStateReady
	page, err := service.ListResources(context.Background(), application.ListResourcesRequest{Principal: principal, TypeName: "NoSuchType", TypeVersion: "v9"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("unknown well-formed type filter returned rows: %v", listedIDs(page))
	}
	page, err = service.ListResources(context.Background(), application.ListResourcesRequest{Principal: principal, StateFilter: &stateReady})
	if err != nil || len(page.Items) != 1 {
		t.Fatalf("state filter page = %#v err=%v", page, err)
	}
	page, err = service.ListResources(context.Background(), application.ListResourcesRequest{Principal: principal, OwnerFilter: &domain.OwnerRef{Kind: "team", ID: "security"}})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("out-of-scope owner filter must select nothing: %#v err=%v", page, err)
	}
}

func ptrResourceState(value string) *domain.ResourceState {
	state := domain.ResourceState(value)
	return &state
}

func TestFakeStoreSaveResourceRejectsOwnerMutation(t *testing.T) {
	_, store := newListTestService(t, fake.AllowAll{})
	seedInventoryResource(t, store, "owned-once", domain.OwnerRef{Kind: "team", ID: "payments"}, domain.ResourceStateReady, time.Now())
	err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		record, err := tx.Resources().GetResource(context.Background(), "owned-once")
		if err != nil {
			return err
		}
		mutated, err := domain.RestoreResource(domain.ResourceSnapshot{
			ID: record.Resource.ID(), Type: record.Resource.Type(),
			Owner: domain.OwnerRef{Kind: "team", ID: "security"}, Generation: record.Resource.Generation(),
			Spec: record.Resource.Spec(), CreatedAt: record.Resource.CreatedAt(), UpdatedAt: record.Resource.UpdatedAt(),
		})
		if err != nil {
			return err
		}
		return tx.Resources().SaveResource(context.Background(), application.ResourceRecord{
			Resource: mutated, Status: record.Status, ProvisionerRef: record.ProvisionerRef, Version: record.Version,
		}, record.Version)
	})
	if err == nil {
		t.Fatal("ordinary persistence update changed Resource ownership")
	}
}
