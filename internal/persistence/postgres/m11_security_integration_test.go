// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"testing"

	applicationfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/persistence/postgres"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
)

// TestIdempotencyScopesArePerPrincipal pins the M11 durable contract: two
// principals may use the same Idempotency-Key independently, and neither can
// resolve the other's record.
func TestIdempotencyScopesArePerPrincipal(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	service, _ := postgresService(t, store, provisioningfake.New(provisioningfake.ModeSynchronous))
	ctx := context.Background()

	principalA := applicationfake.Principal("principal-a")
	principalB := applicationfake.Principal("principal-b")

	commandA := postgresCreateCommand(t, "scoped-resource-a", "scoped-op-a", map[string]any{"size": int64(1)})
	commandA.Actor = principalA
	if _, admitErr := service.AdmitCreateResource(ctx, commandA); admitErr != nil {
		t.Fatal(admitErr)
	}

	// Principal B reuses the identical key with distinct content: it must
	// succeed as an independent admission instead of colliding with A's key
	// or resolving A's record.
	commandB := postgresCreateCommand(t, "scoped-resource-b", "scoped-op-b", map[string]any{"size": int64(2)})
	commandB.Actor = principalB
	resultB, err := service.AdmitCreateResource(ctx, commandB)
	if err != nil {
		t.Fatalf("principal B admission with A's key failed: %v", err)
	}
	if resultB.Replay {
		t.Fatal("principal B resolved a record from principal A's scope")
	}
	if resultB.Operation.ID() != "scoped-op-b" {
		t.Fatalf("principal B resolved operation %q", resultB.Operation.ID())
	}

	// Each principal still replays its own original admission.
	replayA, err := service.AdmitCreateResource(ctx, commandA)
	if err != nil || !replayA.Replay {
		t.Fatalf("principal A lost its own replay: %+v, %v", replayA.Replay, err)
	}
}

// TestEventsPersistNormalizedActor pins actor audit durability and hygiene:
// the admission event carries only the stable principal ID and kind in its
// data payload — never memberships, tokens, or raw claims.
func TestEventsPersistNormalizedActor(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	service, _ := postgresService(t, store, provisioningfake.New(provisioningfake.ModeSynchronous))
	ctx := context.Background()

	actor := applicationfake.Principal("audit-principal")
	command := postgresCreateCommand(t, "audited-resource", "audited-op", map[string]any{"size": int64(3)})
	command.Actor = actor
	if _, admitErr := service.AdmitCreateResource(ctx, command); admitErr != nil {
		t.Fatal(err)
	}

	var data map[string]any
	err = pool.QueryRow(ctx,
		`SELECT data FROM events WHERE id = $1`, "event-audited-op").Scan(&data)
	if err != nil {
		t.Fatalf("admission event lookup: %v", err)
	}
	actorData, ok := data["actor"].(map[string]any)
	if !ok {
		t.Fatalf("event data has no actor object: %v", data)
	}
	if actorData["id"] != string(actor.ID) || actorData["kind"] != "user" {
		t.Fatalf("actor payload = %v, want id=%q kind=user", actorData, actor.ID)
	}
	if len(actorData) != 2 {
		t.Fatalf("actor payload carries extra fields: %v (only ID and kind are auditable)", actorData)
	}
}
