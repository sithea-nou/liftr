// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/persistence/postgres"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
)

func outputSnapshot(t *testing.T, generation uint64, hostname string, publishedAt time.Time) domain.ResourceOutputs {
	t.Helper()
	snapshot, err := domain.NewResourceOutputs(generation, map[string]any{"hostname": hostname, "port": int64(5432)}, publishedAt)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// seedOutputResource creates a retained resource so output rows can satisfy
// their composite foreign keys.
func seedOutputResource(t *testing.T, ctx context.Context, store *postgres.Store, id string) application.ResourceRecord {
	t.Helper()
	service, _ := postgresService(t, store, provisioningfake.New(provisioningfake.ModeSynchronous))
	command := postgresCreateCommand(t, domain.ResourceID(id), domain.OperationID("op-seed-"+id), map[string]any{"n": int64(1)})
	if _, err := service.AdmitCreateResource(ctx, command); err != nil {
		t.Fatal(err)
	}
	record, err := func() (application.ResourceRecord, error) {
		var loaded application.ResourceRecord
		err := store.Within(ctx, func(tx application.UnitOfWork) error {
			var innerErr error
			loaded, innerErr = tx.Resources().GetResource(ctx, command.ID)
			return innerErr
		})
		return loaded, err
	}()
	if err != nil {
		t.Fatal(err)
	}
	return record
}

// seedOutputOperation creates a terminal operation row so output snapshots can
// satisfy their composite foreign key into operations.
func seedOutputOperation(t *testing.T, ctx context.Context, store *postgres.Store, id domain.OperationID, resourceID domain.ResourceID, capability domain.Capability, generation uint64) {
	t.Helper()
	operation, err := domain.NewOperation(id, resourceID, capability, generation, time.Date(2026, 8, 23, 8, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if err := operation.Start(time.Date(2026, 8, 23, 8, 0, 30, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := operation.AdvancePhase(domain.OperationPhasePlanning, time.Date(2026, 8, 23, 8, 0, 35, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := operation.AdvancePhase(domain.OperationPhaseApplying, time.Date(2026, 8, 23, 8, 0, 40, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if err := operation.Succeed(time.Date(2026, 8, 23, 8, 1, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	err = store.Within(ctx, func(tx application.UnitOfWork) error {
		return tx.Operations().CreateOperation(ctx, application.OperationRecord{Operation: operation, Version: 1})
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPostgresResourceOutputsRoundTripSurvivesReopen pins the durability and
// restart requirements: snapshots persist with provenance, survive a fresh
// connection, and the latest-generation lookup is deterministic.
func TestPostgresResourceOutputsRoundTripSurvivesReopen(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	seedOutputResource(t, ctx, store, "outputs-roundtrip")
	seedOutputOperation(t, ctx, store, "op-outputs-gen1", "outputs-roundtrip", domain.CapabilityCreate, 1)
	seedOutputOperation(t, ctx, store, "op-outputs-gen2", "outputs-roundtrip", domain.CapabilityUpdate, 2)

	publishedAt := time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)
	gen1 := outputSnapshot(t, 1, "old.example", publishedAt)
	gen2 := outputSnapshot(t, 2, "new.example", publishedAt.Add(time.Minute))
	digest1, err := application.ValuesDigest(gen1.Values())
	if err != nil {
		t.Fatal(err)
	}
	digest2, err := application.ValuesDigest(gen2.Values())
	if err != nil {
		t.Fatal(err)
	}
	insert := func(snapshot domain.ResourceOutputs, operation string, digest string, capability domain.Capability) error {
		return store.Within(ctx, func(tx application.UnitOfWork) error {
			return tx.Outputs().SaveResourceOutputs(ctx, application.ResourceOutputRecord{
				ResourceID: "outputs-roundtrip", ObservedGeneration: snapshot.ObservedGeneration(),
				OperationID: domain.OperationID(operation), Capability: capability,
				OutputMappingRef: "mapping-v1", OutputContractDigest: "contract-digest",
				Values: snapshot, ValuesDigest: digest,
			})
		})
	}
	if err := insert(gen1, "op-outputs-gen1", digest1, domain.CapabilityCreate); err != nil {
		t.Fatal(err)
	}
	if err := insert(gen2, "op-outputs-gen2", digest2, domain.CapabilityUpdate); err != nil {
		t.Fatal(err)
	}

	// Fresh pool simulates process restart.
	reopened, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	err = reopened.Within(ctx, func(tx application.UnitOfWork) error {
		latest, found, err := tx.Outputs().LatestResourceOutputs(ctx, "outputs-roundtrip")
		if err != nil || !found {
			t.Fatalf("latest outputs found=%t err=%v", found, err)
		}
		if latest.ObservedGeneration != 2 || latest.Values.Values()["hostname"] != "new.example" {
			t.Fatalf("latest = generation %d values %v", latest.ObservedGeneration, latest.Values.Values())
		}
		if latest.OperationID != "op-outputs-gen2" || latest.OutputMappingRef != "mapping-v1" ||
			latest.OutputContractDigest != "contract-digest" || latest.ValuesDigest != digest2 {
			t.Fatalf("provenance lost: %+v", latest)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestPostgresResourceOutputsImmutable pins the append-only trigger and the
// contradictory-evidence fail-closed behavior.
func TestPostgresResourceOutputsImmutable(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	seedOutputResource(t, ctx, store, "outputs-immutable")
	snapshot := outputSnapshot(t, 1, "db.example", time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC))
	digest, err := application.ValuesDigest(snapshot.Values())
	if err != nil {
		t.Fatal(err)
	}
	record := application.ResourceOutputRecord{
		ResourceID: "outputs-immutable", ObservedGeneration: 1,
		OperationID: "op-seed-outputs-immutable", Capability: domain.CapabilityCreate,
		OutputMappingRef: "m1", OutputContractDigest: "cd",
		Values: snapshot, ValuesDigest: digest,
	}
	err = store.Within(ctx, func(tx application.UnitOfWork) error {
		return tx.Outputs().SaveResourceOutputs(ctx, record)
	})
	if err != nil {
		t.Fatal(err)
	}

	expectPostgresFailure(t, func() error {
		_, err := pool.Exec(ctx, `UPDATE resource_outputs SET values_digest='changed' WHERE resource_id='outputs-immutable'`)
		return err
	})
	expectPostgresFailure(t, func() error {
		_, err := pool.Exec(ctx, `DELETE FROM resource_outputs WHERE resource_id='outputs-immutable'`)
		return err
	})

	conflicting := record
	// Same provenance key, tampered content digest: must hit the
	// fail-closed comparison rather than silently overwriting.
	conflicting.ValuesDigest = "different"
	err = store.Within(ctx, func(tx application.UnitOfWork) error {
		return tx.Outputs().SaveResourceOutputs(ctx, conflicting)
	})
	if err == nil {
		t.Fatal("contradictory evidence accepted")
	}
	if !isInvalidApplicationCall(t, err) {
		t.Fatalf("contradiction surfaced as %v, want ErrInvalidApplicationCall", err)
	}
}

// TestPostgresExecutionOutputColumnsRoundTrip pins the durable resolution
// state machine columns including rejection details.
func TestPostgresExecutionOutputColumnsRoundTrip(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	service, resolver := postgresService(t, store, provisioningfake.New(provisioningfake.ModeSynchronous))
	command := postgresCreateCommand(t, "outputs-execution", "op-exec-outputs", map[string]any{"n": int64(2)})
	admitted, err := service.AdmitCreateResource(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := getExecutionErr(store, admitted.Operation.ID())
	if err != nil {
		t.Fatal(err)
	}
	execution.State = application.AttemptSucceeded
	execution.OutputResolution = application.OutputResolutionRejected
	execution.OutputFailureReason = application.ReasonOutputPostconditionRejected
	execution.OutputFailureMessage = "required output field port is missing"
	if err := saveExecutionVersioned(t, store, execution); err != nil {
		t.Fatal(err)
	}
	reloaded, err := getExecutionErr(store, admitted.Operation.ID())
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.OutputResolution != application.OutputResolutionRejected ||
		reloaded.OutputFailureReason != application.ReasonOutputPostconditionRejected ||
		reloaded.OutputFailureMessage == "" {
		t.Fatalf("reloaded execution = %+v", reloaded)
	}
	if _, ok := resolver.Providers["unused"]; ok {
		t.Fatal("unexpected provider entry")
	}
}

func getExecutionErr(store *postgres.Store, id domain.OperationID) (application.ProvisioningExecutionRecord, error) {
	var record application.ProvisioningExecutionRecord
	err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		var err error
		record, err = tx.Executions().GetExecution(context.Background(), id)
		return err
	})
	return record, err
}

func saveExecutionVersioned(t *testing.T, store *postgres.Store, record application.ProvisioningExecutionRecord) error {
	t.Helper()
	return store.Within(context.Background(), func(tx application.UnitOfWork) error {
		current, err := tx.Executions().GetExecution(context.Background(), record.OperationID)
		if err != nil {
			return err
		}
		return tx.Executions().SaveExecution(context.Background(), record, current.Version)
	})
}

func isInvalidApplicationCall(t *testing.T, err error) bool {
	t.Helper()
	if err == nil {
		return false
	}
	return errorsIs(err, application.ErrInvalidApplicationCall)
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
