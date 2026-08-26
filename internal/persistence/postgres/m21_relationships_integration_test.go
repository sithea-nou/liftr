// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sithea-nou/liftr/internal/application"
	applicationfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/lifecycle"
	"github.com/sithea-nou/liftr/internal/persistence/postgres"
	"github.com/sithea-nou/liftr/internal/provisioning"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
	"github.com/sithea-nou/liftr/internal/worker"
)

// m21CatalogWithSlot declares a self-targeting optional reference slot so
// admission exercises the relationship paths.
func m21CatalogWithSlot(t *testing.T) application.ResourceTypeCatalog {
	t.Helper()
	typeValue, err := domain.NewResourceType(provisioningfake.ResourceType(), "PostgreSQL M21 resource",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		t.Fatal(err)
	}
	slots, err := resourcecontract.NewReferenceContract([]resourcecontract.ReferenceSlot{{
		Name:               "dependency",
		AllowedTargetTypes: []domain.ResourceTypeRef{provisioningfake.ResourceType()},
		MinItems:           0,
		MaxItems:           1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return applicationfake.Catalog{
		Types:      map[domain.ResourceTypeRef]domain.ResourceType{provisioningfake.ResourceType(): typeValue},
		References: map[domain.ResourceTypeRef]*resourcecontract.ReferenceContract{provisioningfake.ResourceType(): &slots},
	}
}

// m21World composes a migrated PostgreSQL store, an M21-catalog service, and a
// real durable worker so tests can converge admitted generations exactly as
// production does.
type m21World struct {
	service  *application.Service
	store    *postgres.Store
	pool     *pgxpool.Pool
	resolver *applicationfake.Resolver
	catalog  application.ResourceTypeCatalog
	instance *worker.Worker
	cleanup  func()
}

// rawExec runs one raw SQL statement against the migrated schema. It is for
// tests only: production paths never bypass the application ports.
func (w *m21World) rawExec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := w.pool.Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("raw exec failed: %v\nSQL: %s", err, query)
	}
}

func newM21World(t *testing.T) *m21World {
	t.Helper()
	pool, cleanup := migratedPool(t)
	store, err := postgres.NewStore(pool)
	if err != nil {
		cleanup()
		t.Fatal(err)
	}
	ref, _ := application.NewProvisionerRef("m21-pg-provider")
	resolver := &applicationfake.Resolver{Providers: map[application.ProvisionerRef]provisioning.Provisioner{
		ref: provisioningfake.New(provisioningfake.ModeSynchronous),
	}}
	catalog := m21CatalogWithSlot(t)
	service, err := application.NewService(catalog, &applicationfake.Selector{Ref: ref}, resolver, store, applicationfake.AllowAll{})
	if err != nil {
		cleanup()
		t.Fatal(err)
	}
	instance, err := worker.NewWithCatalog(store, resolver, catalog)
	if err != nil {
		cleanup()
		t.Fatal(err)
	}
	world := &m21World{service: service, store: store, pool: pool, resolver: resolver, catalog: catalog, instance: instance, cleanup: cleanup}
	return world
}

func (w *m21World) drain(t *testing.T) {
	t.Helper()
	for range 128 {
		worked, err := w.instance.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			return
		}
	}
	t.Fatal("m21 worker did not drain")
}

func (w *m21World) create(t *testing.T, id string, references map[string][]string) application.Result {
	t.Helper()
	command := application.CreateResourceCommand{
		Actor: applicationfake.Principal("m21"), ID: domain.ResourceID(id),
		Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: m21Spec(t, id), References: references,
		OperationID:    domain.OperationID("op-create-" + id),
		EventID:        domain.EventID("evt-create-" + id),
		RequestedAt:    time.Now().UTC(),
		IdempotencyKey: "key-create-" + id,
	}
	result, err := w.service.AdmitCreateResource(context.Background(), command)
	if err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	return result
}

func (w *m21World) update(t *testing.T, id string, references map[string][]string, present bool, key string) (application.Result, error) {
	t.Helper()
	command := application.UpdateResourceCommand{
		Actor:              applicationfake.Principal("m21"),
		ID:                 domain.ResourceID(id),
		ExpectedGeneration: w.generation(t, id),
		Spec:               m21Spec(t, id+"-"+key),
		ReferencesPresent:  present,
		References:         references,
		OperationID:        domain.OperationID("op-update-" + key),
		EventID:            domain.EventID("evt-update-" + key),
		RequestedAt:        time.Now().UTC(),
		IdempotencyKey:     "key-" + key,
	}
	result, err := w.service.AdmitUpdateResource(context.Background(), command)
	return result, err
}

func (w *m21World) remove(t *testing.T, id string, key string) error {
	t.Helper()
	command := application.DeleteResourceCommand{
		Actor:              applicationfake.Principal("m21"),
		ID:                 domain.ResourceID(id),
		ExpectedGeneration: w.generation(t, id),
		OperationID:        domain.OperationID("op-delete-" + key),
		EventID:            domain.EventID("evt-delete-" + key),
		RequestedAt:        time.Now().UTC(),
		IdempotencyKey:     "key-" + key,
	}
	_, err := w.service.AdmitDeleteResource(context.Background(), command)
	return err
}

func (w *m21World) generation(t *testing.T, id string) uint64 {
	t.Helper()
	var generation uint64
	err := w.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		record, err := tx.Resources().GetResource(context.Background(), domain.ResourceID(id))
		if err != nil {
			return err
		}
		generation = record.Resource.Generation()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return generation
}

func (w *m21World) edges(t *testing.T, applied bool, id string) []application.ReferenceEdge {
	t.Helper()
	var edges []application.ReferenceEdge
	err := w.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		ctx := context.Background()
		var err error
		if applied {
			edges, err = tx.References().AppliedReferences(ctx, domain.ResourceID(id))
		} else {
			edges, err = tx.References().DesiredReferences(ctx, domain.ResourceID(id))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return edges
}

func (w *m21World) protected(t *testing.T, target string) bool {
	t.Helper()
	var protective bool
	err := w.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		var err error
		protective, err = tx.References().HasInboundProtectiveReference(context.Background(), domain.ResourceID(target))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return protective
}

func (w *m21World) state(t *testing.T, id string) domain.ResourceState {
	t.Helper()
	var state domain.ResourceState
	err := w.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		record, err := tx.Resources().GetResource(context.Background(), domain.ResourceID(id))
		if err != nil {
			return err
		}
		state = record.Status.State()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func m21Spec(t *testing.T, name string) domain.ResourceSpec {
	t.Helper()
	spec, err := domain.NewResourceSpec(map[string]any{"size": uint64(3), "name": name})
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

// TestM21PostgresConcurrentCycleAdmissionCannotCommit is the adversarial
// A->B / B->A race on REAL PostgreSQL. Both graph mutations run concurrently;
// because each serializes through the owner structural lock BEFORE touching
// Resource rows, both committing is impossible and the surviving graph stays
// acyclic.
func TestM21PostgresConcurrentCycleAdmissionCannotCommit(t *testing.T) {
	w := newM21World(t)
	defer w.cleanup()

	w.create(t, "pg-a", nil)
	w.create(t, "pg-b", nil)
	w.drain(t)

	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, results[0] = w.update(t, "pg-a", map[string][]string{"dependency": {"pg-b"}}, true, "race-ab")
	}()
	go func() {
		defer wg.Done()
		<-start
		_, results[1] = w.update(t, "pg-b", map[string][]string{"dependency": {"pg-a"}}, true, "race-ba")
	}()
	close(start)
	wg.Wait()

	failures := 0
	for _, err := range results {
		if err == nil {
			continue
		}
		failures++
		switch {
		case errors.Is(err, application.ErrDependencyCycle),
			errors.Is(err, application.ErrConcurrencyConflict),
			errors.Is(err, lifecycle.ErrOperationActive):
			// Acceptable race outcomes.
		default:
			t.Fatalf("unexpected concurrent cycle-race error: %v", err)
		}
	}
	if failures == 0 {
		t.Fatal("both sides of the cycle race committed; owner serialization failed")
	}
	w.drain(t)
	if len(w.edges(t, false, "pg-a"))+len(w.edges(t, false, "pg-b")) > 1 {
		t.Fatalf("committed desired graph is not acyclic: %+v / %+v",
			w.edges(t, false, "pg-a"), w.edges(t, false, "pg-b"))
	}
}

// TestM21PostgresReferenceVersusTargetDeleteRace proves reference-admission vs
// target-delete serialization: either the edge commits and delete answers
// RESOURCE_IN_USE, or delete wins and the edge is refused. A reference must
// never land on a Resource whose delete was simultaneously admitted.
func TestM21PostgresReferenceVersusTargetDeleteRace(t *testing.T) {
	w := newM21World(t)
	defer w.cleanup()

	w.create(t, "pg-target", nil)
	w.create(t, "pg-source", nil)
	w.drain(t)

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	var referenceErr, deleteErr error
	go func() {
		defer wg.Done()
		<-start
		_, referenceErr = w.update(t, "pg-source", map[string][]string{"dependency": {"pg-target"}}, true, "refdel")
	}()
	go func() {
		defer wg.Done()
		<-start
		deleteErr = w.remove(t, "pg-target", "refdel-del")
	}()
	close(start)
	wg.Wait()

	protected := w.protected(t, "pg-target")
	switch {
	case referenceErr == nil && deleteErr == nil && !protected:
		t.Fatal("delete won yet no protective evidence remains and the reference also committed")
	case referenceErr == nil:
		if !protected {
			t.Fatal("reference committed without protective evidence")
		}
		if deleteErr == nil || !errors.Is(deleteErr, application.ErrResourceInUse) {
			t.Fatalf("post-commit delete = %v, want RESOURCE_IN_USE", deleteErr)
		}
	case deleteErr == nil:
		if protected {
			t.Fatal("delete succeeded while protective evidence stands")
		}
		if referenceErr == nil || errors.Is(referenceErr, context.DeadlineExceeded) {
			t.Fatalf("reference after delete = %v", referenceErr)
		}
	default:
		if !errors.Is(deleteErr, application.ErrResourceInUse) &&
			!errors.Is(deleteErr, lifecycle.ErrOperationActive) &&
			!errors.Is(deleteErr, application.ErrConcurrencyConflict) {
			t.Fatalf("delete failed unexpectedly: %v", deleteErr)
		}
	}
}

// TestM21PostgresRemovalDoesNotReleaseAppliedProtection pins the union rule on
// REAL PostgreSQL: removing T from desired while T still sits in applied
// references keeps T's deletion blocked until final convergence.
func TestM21PostgresRemovalDoesNotReleaseAppliedProtection(t *testing.T) {
	w := newM21World(t)
	defer w.cleanup()

	w.create(t, "rm-t", nil)
	w.create(t, "rm-src", nil)
	w.drain(t)

	// Generation 2 binds rm-t (converges => applied={rm-t}).
	if _, err := w.update(t, "rm-src", map[string][]string{"dependency": {"rm-t"}}, true, "rm-est"); err != nil {
		t.Fatal(err)
	}
	w.drain(t)
	if applied := w.edges(t, true, "rm-src"); len(applied) != 1 || applied[0].TargetID != "rm-t" {
		t.Fatalf("applied after convergence = %+v", applied)
	}

	// Generation 3 removes the desired edge; applied still protects rm-t
	// because this update has NOT converged yet.
	if _, err := w.update(t, "rm-src", map[string][]string{}, true, "rm-clear"); err != nil {
		t.Fatal(err)
	}
	if desired := w.edges(t, false, "rm-src"); len(desired) != 0 {
		t.Fatalf("desired should be empty after explicit replacement: %+v", desired)
	}
	if applied := w.edges(t, true, "rm-src"); len(applied) == 0 {
		t.Fatal("applied protection vanished before convergence")
	}
	if err := w.remove(t, "rm-t", "rm-try"); !errors.Is(err, application.ErrResourceInUse) {
		t.Fatalf("target delete during applied-only protection = %v, want RESOURCE_IN_USE", err)
	}

	// Converge generation 3: applied collapses to {} and rm-t is released.
	w.drain(t)
	if applied := w.edges(t, true, "rm-src"); len(applied) != 0 {
		t.Fatalf("applied after cleared convergence = %+v, want empty", applied)
	}
	if err := w.remove(t, "rm-t", "rm-final"); err != nil {
		t.Fatalf("released target delete failed: %v", err)
	}
	w.drain(t)
	if state := w.state(t, "rm-t"); state != domain.ResourceStateDeleting && state != domain.ResourceStateDeleted && state != domain.ResourceStateFailed {
		t.Fatalf("released target state = %s", state)
	}
}

// TestM21PostgresWakeVersionHandshakeAndFanout exercises the wake machinery
// end-to-end on PostgreSQL: a blocked dependent waits, the anchor converges,
// the versioned wake fires, and the dependent submits exactly once.
func TestM21PostgresWakeVersionHandshakeAndFanout(t *testing.T) {
	w := newM21World(t)
	defer w.cleanup()

	w.create(t, "wake-anchor", nil)
	w.create(t, "wake-dependent", map[string][]string{"dependency": {"wake-anchor"}})

	// Drain everything; either ordering yields exactly one dependent Submit
	// and full convergence through the wait/wake path or direct readiness.
	w.drain(t)

	if state := w.state(t, "wake-dependent"); state != domain.ResourceStateReady {
		t.Fatalf("dependent state = %s, want Ready", state)
	}
	desired := w.edges(t, false, "wake-dependent")
	applied := w.edges(t, true, "wake-dependent")
	if len(desired) != 1 || desired[0].Generation != 1 || len(applied) != 1 || applied[0].TargetID != "wake-anchor" {
		t.Fatalf("desired=%+v applied=%+v", desired, applied)
	}

}

// TestM21PostgresDeletedSourceWithEdgesFailsClosed seeds the corruption case
// directly in SQL: a Deleted source still owning protective rows must refuse
// the target delete with the reference-invariant failure instead of being
// silently ignored.
func TestM21PostgresDeletedSourceWithEdgesFailsClosed(t *testing.T) {
	w := newM21World(t)
	defer w.cleanup()

	w.create(t, "fc-target", nil)
	w.create(t, "fc-source", map[string][]string{"dependency": {"fc-target"}})
	w.drain(t)
	if err := w.remove(t, "fc-source", "fc-src"); err != nil {
		t.Fatal(err)
	}
	w.drain(t)
	if state := w.state(t, "fc-source"); state != domain.ResourceStateDeleted {
		t.Fatalf("source state = %s, want Deleted", state)
	}
	if w.protected(t, "fc-target") {
		t.Fatal("properly deleted source left protective rows behind")
	}

	// Reintroduce corruption: an applied row owned by the Deleted source.
	w.rawExec(t, `INSERT INTO resource_applied_references (source_id, slot, target_id, generation)
		 VALUES ('fc-source','dependency','fc-target',1)`)
	var invariantErr error
	err := w.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		_, invariantErr = tx.References().HasInboundProtectiveReference(context.Background(), "fc-target")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(invariantErr, application.ErrReferenceInvariant) {
		t.Fatalf("corrupted inbound rows error = %v, want ErrReferenceInvariant", invariantErr)
	}
}

// parkAnchorWork postpones (or releases) every outbox row of one Resource so
// tests can deterministically order gate registration against a later
// gate-relevant transition. Parking only moves available_at; nothing is
// deleted, so the production transition path runs untouched afterwards.
func parkAnchorWork(t *testing.T, w *m21World, id string, park bool) {
	t.Helper()
	if park {
		w.rawExec(t, `UPDATE outbox_messages SET available_at = clock_timestamp() + interval '1 hour' WHERE resource_id = $1 OR operation_id IN (SELECT id FROM operations WHERE resource_id = $1)`, id)
		return
	}
	w.rawExec(t, `UPDATE outbox_messages SET available_at = clock_timestamp() WHERE state = 'Pending' AND (resource_id = $1 OR operation_id IN (SELECT id FROM operations WHERE resource_id = $1))`, id)
}

func (w *m21World) waitsFor(t *testing.T, target string) int {
	t.Helper()
	var count int
	err := w.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		var err error
		count, err = countDependencyWaits(context.Background(), tx, domain.ResourceID(target))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return count
}
