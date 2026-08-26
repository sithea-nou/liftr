// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	applicationfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
	"github.com/sithea-nou/liftr/internal/worker"
)

func applicationfakePrincipal() identity.Principal { return applicationfake.Principal("m21") }
func timeNowUTC() time.Time                        { return time.Now().UTC() }

func workerNewWithCatalog(w *m21World) (*worker.Worker, error) {
	return worker.NewWithCatalog(w.store, w.resolver, w.catalog)
}

func countDependencyWaits(ctx context.Context, tx application.UnitOfWork, target domain.ResourceID) (int, error) {
	return application.CountDependencyWaitsForTarget(ctx, tx, target)
}

// TestM21PostgresLargeFanoutExecution drives the full wake machinery on REAL
// PostgreSQL with a shared target blocked by many dependents:
//
//   - the anchor's success transaction enqueues AT MOST ONE WakeDependents;
//   - the wake worker pages waiters in bounded LIMIT/keyset batches;
//   - Drives are deduplicated and every current waiter converges;
//   - no dependent receives more than exactly one Provisioner.Submit;
//   - a mid-fanout worker restart (fresh lease identity) is harmless;
//   - wait rows are fully cleaned once every dependent passes its gate.
//
// Determinism: after admission, the ANCHOR's outbox rows are parked far in the
// future so the first drain registers every dependent's wait against the
// Pending anchor; the anchor rows are then released, making the anchor's own
// success transaction — including its conditional wake enqueue — the exact
// production code path.
func TestM21PostgresLargeFanoutExecution(t *testing.T) {
	w := newM21World(t)
	defer w.cleanup()
	ctx := context.Background()

	const dependents = 250

	w.create(t, "fanout-anchor", nil)

	parkAnchorWork(t, w, "fanout-anchor", true)
	for i := range dependents {
		id := fanoutDependentID(i)
		command := application.CreateResourceCommand{
			Actor: applicationfakePrincipal(), ID: domain.ResourceID(id),
			Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
			Spec:           m21Spec(t, id),
			References:     map[string][]string{"dependency": {"fanout-anchor"}},
			OperationID:    domain.OperationID("op-fan-" + id),
			EventID:        domain.EventID("evt-fan-" + id),
			RequestedAt:    timeNowUTC(),
			IdempotencyKey: "key-fan-" + string(id),
		}
		if _, err := w.service.AdmitCreateResource(ctx, command); err != nil {
			t.Fatalf("admit %s: %v", id, err)
		}
	}
	// Each dependent needs its full Requested->Validating->Planning->Applying
	// drive chain before the gate registers its wait: bound generously.
	for range 2000 {
		worked, err := w.instance.RunOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
	}
	if waits := w.waitsFor(t, "fanout-anchor"); waits != dependents {
		t.Fatalf("registered waits = %d, want %d", waits, dependents)
	}

	parkAnchorWork(t, w, "fanout-anchor", false)

	// Mid-fanout restart: run part of the drain with one worker instance
	// (lease identity A), then finish with a brand-new instance (identity B).
	for range 40 {
		worked, err := w.instance.RunOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
	}
	fresh, err := workerNewWithCatalog(w)
	if err != nil {
		t.Fatal(err)
	}
	for range 4000 {
		worked, err := fresh.RunOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			break
		}
		takenOver, err := w.instance.RunOnce(ctx) // old lease owner must be inert/stale-safe
		if err != nil && !isStaleConflict(err) {
			t.Fatal(err)
		}
		_ = takenOver
	}

	if state := w.state(t, "fanout-anchor"); state != domain.ResourceStateReady {
		t.Fatalf("anchor state = %s", state)
	}
	for i := range dependents {
		id := fanoutDependentID(i)
		if state := w.state(t, id); state != domain.ResourceStateReady {
			t.Fatalf("dependent %s state = %s", id, state)
		}
		applied := w.edges(t, true, id)
		if len(applied) != 1 || applied[0].TargetID != "fanout-anchor" {
			t.Fatalf("dependent %s applied = %+v", id, applied)
		}
	}
	if waits := w.waitsFor(t, "fanout-anchor"); waits != 0 {
		t.Fatalf("%d stale wait rows survived full convergence", waits)
	}

	// Exactly ONE wake was ever created for the anchor, and it is terminal.
	var wakes, activeWakes int
	wakeErr := w.store.Within(ctx, func(tx application.UnitOfWork) error {
		summary, err := tx.Outbox().SummarizeWorkByResource(ctx, "fanout-anchor")
		if err != nil {
			return err
		}
		activeWakes = len(summary.Active)
		wakes = summary.Counts[application.OutboxCompleted]
		return nil
	})
	if wakeErr != nil {
		t.Fatal(wakeErr)
	}
	if activeWakes != 0 || wakes != 1 {
		t.Fatalf("wake rows completed=%d active=%d, want exactly 1 completed, 0 active", wakes, activeWakes)
	}
}

func fanoutDependentID(i int) string { return fmt.Sprintf("fan-dep-%04d", i) }

func isStaleConflict(err error) bool { return errors.Is(err, application.ErrConcurrencyConflict) }
