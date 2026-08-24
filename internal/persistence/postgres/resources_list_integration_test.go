// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	applicationfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/persistence/postgres"
)

func inventorySeed(t *testing.T, resources application.ResourceRepository, operations application.OperationRepository,
	id string, owner domain.OwnerRef, state domain.ResourceState, createdAt time.Time, operationID domain.OperationID) {
	t.Helper()
	resource, err := domain.NewResource(domain.ResourceID(id),
		domain.ResourceTypeRef{Name: "Widget", Version: "v1"}, owner,
		inventorySpec(t), createdAt)
	if err != nil {
		t.Fatal(err)
	}
	status, err := domain.NewResourceStatus(resource.ID(), 1, state, nil, createdAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := resources.CreateResource(context.Background(), application.ResourceRecord{
		Resource: resource, Status: status, ProvisionerRef: inventoryProvisionerRef(t), Version: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if operationID == "" {
		return
	}
	operation, err := domain.NewOperation(operationID, resource.ID(), domain.CapabilityCreate, 1, createdAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := operation.Start(createdAt.Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := operations.CreateOperation(context.Background(), application.OperationRecord{Operation: operation, Version: 1}); err != nil {
		t.Fatal(err)
	}
}

func inventorySpec(t *testing.T) domain.ResourceSpec {
	t.Helper()
	spec, err := domain.NewResourceSpec(map[string]any{"size": int64(1)})
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func inventoryProvisionerRef(t *testing.T) application.ProvisionerRef {
	t.Helper()
	ref, err := application.NewProvisionerRef("inventory-test-provider")
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// listPage runs one trusted query inside a transaction, mirroring how the
// use case consumes the repository port.
func listPage(t *testing.T, store *postgres.Store, query application.ResourceListQuery) application.ResourceInventoryPage {
	t.Helper()
	var page application.ResourceInventoryPage
	err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		var err error
		page, err = tx.Resources().ListResources(context.Background(), query)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return page
}

func listIDs(items []application.ResourceInventoryItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, string(item.ID))
	}
	return ids
}

func joinedIDs(items []application.ResourceInventoryItem) string {
	return strings.Join(listIDs(items), ",")
}

func inventorySeeds() []struct {
	id          string
	owner       domain.OwnerRef
	state       domain.ResourceState
	at          time.Time
	operationID domain.OperationID
} {
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	payments := domain.OwnerRef{Kind: "team", ID: "payments"}
	platform := domain.OwnerRef{Kind: "team", ID: "platform"}
	return []struct {
		id          string
		owner       domain.OwnerRef
		state       domain.ResourceState
		at          time.Time
		operationID domain.OperationID
	}{
		{"inv-a", payments, domain.ResourceStateReady, base, "op-inv-a"},
		{"inv-b", payments, domain.ResourceStateReady, base, ""},
		{"inv-c", platform, domain.ResourceStateFailed, base.Add(time.Second), ""},
		{"inv-deleted", payments, domain.ResourceStateDeleted, base.Add(2 * time.Second), ""},
		{"inv-d", platform, domain.ResourceStatePending, base.Add(-time.Hour), ""},
	}
}

func seedBothStores(t *testing.T, store *postgres.Store, fakeStore *applicationfake.Store) {
	t.Helper()
	seeds := inventorySeeds()
	seedInto := func(resources application.ResourceRepository, operations application.OperationRepository) error {
		for _, seed := range seeds {
			inventorySeed(t, resources, operations, seed.id, seed.owner, seed.state, seed.at, seed.operationID)
		}
		return nil
	}
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		return seedInto(tx.Resources(), tx.Operations())
	}); err != nil {
		t.Fatal(err)
	}
	if err := fakeStore.Within(context.Background(), func(tx application.UnitOfWork) error {
		return seedInto(tx.Resources(), tx.Operations())
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresResourceInventoryOrderingPagingAndParity(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	fakeStore := applicationfake.NewStore()
	seedBothStores(t, store, fakeStore)

	query := application.ResourceListQuery{
		AllowedOwners:  []domain.OwnerRef{{Kind: "team", ID: "payments"}, {Kind: "team", ID: "platform"}},
		Limit:          10,
		IncludeDeleted: true,
	}
	pgPage := listPage(t, store, query)
	fakePage, err := fakeStore.Resources().ListResources(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}

	// Insertion sequence, not created_at_ns, defines inventory order: the
	// last-seeded row leads even though its timestamp is an hour older than
	// the others, and equal-timestamp pairs keep deterministic positions.
	want := "inv-d,inv-deleted,inv-c,inv-b,inv-a"
	if got := joinedIDs(pgPage.Items); got != want {
		t.Fatalf("postgres inventory order = %q, want %q", got, want)
	}
	if joinedIDs(fakePage.Items) != joinedIDs(pgPage.Items) {
		t.Fatalf("fake and postgres disagree:\n fake=%v\n pg=%v", listIDs(fakePage.Items), listIDs(pgPage.Items))
	}
	var itemWithLatest *application.ResourceInventoryItem
	for i := range pgPage.Items {
		if pgPage.Items[i].Sequence != fakePage.Items[i].Sequence ||
			pgPage.Items[i].Status.State != fakePage.Items[i].Status.State ||
			pgPage.Items[i].Generation != fakePage.Items[i].Generation ||
			pgPage.Items[i].Owner != fakePage.Items[i].Owner {
			t.Fatalf("item %d disagrees between stores:\n pg=%+v\n fake=%+v", i, pgPage.Items[i], fakePage.Items[i])
		}
		if pgPage.Items[i].ID == "inv-a" {
			itemWithLatest = &pgPage.Items[i]
		}
	}
	if itemWithLatest == nil || itemWithLatest.Latest == nil || itemWithLatest.Latest.ID != "op-inv-a" || itemWithLatest.Latest.State != domain.OperationStateRunning {
		t.Fatalf("latest operation projection missing or wrong: %+v", pgPage.Items)
	}

	traverse := func(store *postgres.Store, fakeStore *applicationfake.Store, query application.ResourceListQuery) (string, string) {
		collect := func(fetch func(application.ResourceListQuery) (application.ResourceInventoryPage, error)) string {
			var collected []string
			cursorQuery := query
			for {
				page, err := fetch(cursorQuery)
				if err != nil {
					t.Fatal(err)
				}
				collected = append(collected, listIDs(page.Items)...)
				if page.NextSequence == 0 {
					return strings.Join(collected, ",")
				}
				cursorQuery.AfterSequence = page.NextSequence
			}
		}
		pg := collect(func(q application.ResourceListQuery) (application.ResourceInventoryPage, error) {
			return listPage(t, store, q), nil
		})
		fake := collect(func(q application.ResourceListQuery) (application.ResourceInventoryPage, error) {
			page, err := fakeStore.Resources().ListResources(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			return page, nil
		})
		return pg, fake
	}

	firstQuery := query
	firstQuery.Limit = 2
	pgTraversal, fakeTraversal := traverse(store, fakeStore, firstQuery)
	wantTraversal := "inv-d,inv-deleted,inv-c,inv-b,inv-a"
	if pgTraversal != wantTraversal || fakeTraversal != wantTraversal {
		t.Fatalf("paging disagreement:\n pg=%q\n fake=%q\n want=%q", pgTraversal, fakeTraversal, wantTraversal)
	}

	// Concurrent insert after page one never duplicates or skips rows in the
	// open window; a fresh head request sees the new row at the top.
	windowQuery := query
	windowQuery.Limit = 2
	firstWindow := listPage(t, store, windowQuery)
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		inventorySeed(t, tx.Resources(), tx.Operations(), "inv-new",
			domain.OwnerRef{Kind: "team", ID: "payments"}, domain.ResourceStateReady,
			time.Date(2026, 8, 16, 13, 0, 0, 0, time.UTC), "")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	windowQuery.AfterSequence = firstWindow.NextSequence
	windowQuery.Limit = 10
	rest := listPage(t, store, windowQuery)
	traversal := joinedIDs(firstWindow.Items) + "," + joinedIDs(rest.Items)
	want = "inv-d,inv-deleted,inv-c,inv-b,inv-a"
	if traversal != want {
		t.Fatalf("traversal disturbed by concurrent insert: %q", traversal)
	}
	head := listPage(t, store, query)
	if joinedIDs(head.Items[:1]) != "inv-new" {
		t.Fatalf("fresh head request does not show the concurrent insert first: %v", listIDs(head.Items))
	}
}

func TestPostgresResourceInventoryOwnerScopeAndEmptyVisibility(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		inventorySeed(t, tx.Resources(), tx.Operations(), "scoped-payments", domain.OwnerRef{Kind: "team", ID: "payments"}, domain.ResourceStateReady, base, "")
		inventorySeed(t, tx.Resources(), tx.Operations(), "scoped-platform", domain.OwnerRef{Kind: "team", ID: "platform"}, domain.ResourceStateReady, base, "")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	paymentsOnly := application.ResourceListQuery{AllowedOwners: []domain.OwnerRef{{Kind: "team", ID: "payments"}}, Limit: 10}
	page := listPage(t, store, paymentsOnly)
	if got := joinedIDs(page.Items); got != "scoped-payments" {
		t.Fatalf("restricted scope returned %q", got)
	}

	emptyScope := application.ResourceListQuery{AllowedOwners: nil, Limit: 10}
	page = listPage(t, store, emptyScope)
	if len(page.Items) != 0 || page.NextSequence != 0 {
		t.Fatalf("empty visibility returned %#v", page)
	}

	unrestricted := application.ResourceListQuery{Unrestricted: true, Limit: 10, IncludeDeleted: true}
	page = listPage(t, store, unrestricted)
	if len(page.Items) != 2 {
		t.Fatalf("unrestricted scope returned %v", listIDs(page.Items))
	}

	narrowed := paymentsOnly
	ownerFilter := domain.OwnerRef{Kind: "team", ID: "security"}
	narrowed.OwnerFilter = &ownerFilter
	page = listPage(t, store, narrowed)
	if len(page.Items) != 0 {
		t.Fatalf("out-of-scope narrowing filter returned %v", listIDs(page.Items))
	}
}

func TestPostgresResourceIdentitySequenceAndOwnerAreImmutable(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		inventorySeed(t, tx.Resources(), tx.Operations(), "immutable-owner", domain.OwnerRef{Kind: "team", ID: "payments"}, domain.ResourceStateReady, base, "")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	expectPostgresFailure(t, func() error {
		_, err := pool.Exec(ctx, `UPDATE resources SET owner_id='security' WHERE id='immutable-owner'`)
		return err
	})
	expectPostgresFailure(t, func() error {
		_, err := pool.Exec(ctx, `UPDATE resources SET owner_kind='org' WHERE id='immutable-owner'`)
		return err
	})
	expectPostgresFailure(t, func() error {
		_, err := pool.Exec(ctx, `UPDATE resources SET resource_seq=resource_seq+100 WHERE id='immutable-owner'`)
		return err
	})

	// Ordinary application flows never mutate ownership: an optimistic save
	// carrying a changed owner violates the trigger like any other path.
	err = store.Within(ctx, func(tx application.UnitOfWork) error {
		record, err := tx.Resources().GetResource(ctx, "immutable-owner")
		if err != nil {
			return err
		}
		mutated, err := domain.RestoreResource(domain.ResourceSnapshot{
			ID: record.Resource.ID(), Type: record.Resource.Type(),
			Owner: domain.OwnerRef{Kind: "team", ID: "security"}, Generation: record.Resource.Generation(),
			Spec: record.Resource.Spec(), CreatedAt: record.Resource.CreatedAt(),
			UpdatedAt: record.Resource.UpdatedAt().Add(time.Minute),
		})
		if err != nil {
			return err
		}
		return tx.Resources().SaveResource(ctx, application.ResourceRecord{
			Resource: mutated, Status: record.Status, ProvisionerRef: record.ProvisionerRef,
		}, record.Version)
	})
	if err == nil {
		t.Fatal("application flow mutated ownership through SaveResource")
	}

	rows, err := pool.Query(ctx, `SELECT indexname FROM pg_indexes WHERE tablename='resources' AND indexname='resources_owner_sequence'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		found = true
	}
	if !found {
		t.Fatal("inventory owner-sequence index was not created")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
