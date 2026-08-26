// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sithea-nou/liftr/internal/application"
	applicationfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/persistence/postgres"
	"github.com/sithea-nou/liftr/internal/provisioning"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
	"github.com/sithea-nou/liftr/internal/worker"
)

var schemaSequence atomic.Uint64

func TestPostgresMigrationEmptyIdempotentAndChecksummed(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("second migration: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE liftr_schema_migrations SET checksum='changed'"); err != nil {
		t.Fatal(err)
	}
	if err := postgres.VerifySchema(ctx, pool); err == nil {
		t.Fatal("schema verification accepted a changed checksum")
	}
	if err := postgres.Migrate(ctx, pool); err == nil {
		t.Fatal("modified applied migration was accepted")
	}
}

func TestPostgresMigrationAdvisoryLockSerializesMigrators(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	var wait sync.WaitGroup
	errorsFound := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsFound <- postgres.Migrate(ctx, pool)
		}()
	}
	wait.Wait()
	close(errorsFound)
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestPostgresMigrationRejectsUnknownAppliedVersion(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO liftr_schema_migrations(version,name,checksum) VALUES (999,'unknown.sql','unknown')"); err != nil {
		t.Fatal(err)
	}
	if err := postgres.Migrate(ctx, pool); err == nil {
		t.Fatal("unknown applied migration version was accepted")
	}
}

func TestPostgresOutputRecoveryMigrationUpgradesExistingExecution(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE liftr_schema_migrations (
		version bigint PRIMARY KEY, name text NOT NULL, checksum text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	names := []string{
		"000001_initial.sql", "000002_operations_id_c_collation.sql", "000003_resource_outputs.sql",
		"000004_provider_evidence_time.sql", "000005_operation_history.sql",
	}
	for version, name := range names {
		raw, err := os.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(raw)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		digest := sha256.Sum256(raw)
		if _, err := pool.Exec(ctx, `INSERT INTO liftr_schema_migrations(version,name,checksum) VALUES ($1,$2,$3)`,
			version+1, name, hex.EncodeToString(digest[:])); err != nil {
			t.Fatal(err)
		}
	}
	statements := []string{
		`INSERT INTO resources(id,type_name,type_version,owner_kind,owner_id,generation,spec_codec_version,spec,record_version,created_at_ns,updated_at_ns)
		 VALUES ('upgrade-resource','Widget','v1','team','platform',1,1,'{}',1,1,1)`,
		`INSERT INTO resource_statuses(resource_id,observed_generation,state,updated_at_ns) VALUES ('upgrade-resource',1,'Failed',2)`,
		`INSERT INTO provisioner_bindings(resource_id,provisioner_ref) VALUES ('upgrade-resource','provider')`,
		`INSERT INTO operations(id,resource_id,capability,target_generation,state,phase,requested_at_ns,started_at_ns,phase_changed_at_ns,completed_at_ns,failure_reason,failure_message,record_version)
		 VALUES ('upgrade-operation','upgrade-resource','create',1,'Failed','Applying',1,1,2,2,'OutputPostconditionRejected','invalid outputs',1)`,
		`INSERT INTO provisioning_executions(operation_id,resource_id,provisioner_ref,resource_type_name,resource_type_version,capability,target_generation,
		 spec_codec_version,submitted_spec,state,acceptance_confirmed,correlation_status,latest_observation,last_observed_at_ns,current_attempt_number,next_observation_sequence,
		 record_version,output_mapping_ref,output_resolution,output_failure_reason,output_failure_message,last_provider_observed_at_ns)
		 VALUES ('upgrade-operation','upgrade-resource','provider','Widget','v1','create',1,1,'{}','Succeeded',true,'Found',
		 '{"correlation":"Found","execution":{"state":"Succeeded"},"resource":{},"observedAtNs":2}',2,1,1,1,'mapping-v1','Rejected','OutputPostconditionRejected','invalid outputs',2)`,
		`INSERT INTO provisioning_submission_attempts(operation_id,attempt_number,state,dispatch_message_id,resolved_at)
		 VALUES ('upgrade-operation',1,'Accepted','dispatch:upgrade-operation:1',clock_timestamp())`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var sourceOperation, sourceAttempt *string
	var mapping string
	if err := pool.QueryRow(ctx, `SELECT recovery_source_operation_id,recovery_source_attempt::text,output_mapping_ref
		FROM provisioning_executions WHERE operation_id='upgrade-operation'`).Scan(&sourceOperation, &sourceAttempt, &mapping); err != nil {
		t.Fatal(err)
	}
	if sourceOperation != nil || sourceAttempt != nil || mapping != "mapping-v1" {
		t.Fatalf("upgraded execution source=%v attempt=%v mapping=%q", sourceOperation, sourceAttempt, mapping)
	}
}

func TestPostgresOperationHistorySequenceOrdering(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	service, _ := postgresService(t, store, provisioningfake.New(provisioningfake.ModeSynchronous))

	command := postgresCreateCommand(t, "resource-latest", "operation-latest", map[string]any{"n": int64(1)})
	admitted, err := service.AdmitCreateResource(ctx, command)
	if err != nil {
		t.Fatal(err)
	}

	// Insertion sequence, not client request time, defines history order.
	requestedAt := command.RequestedAt.Add(-time.Hour)
	equalOlder := mustEqualTimestampOperation(t, "op-a-equal-older", command.ID, domain.CapabilityUpdate, requestedAt)
	equalNewer := mustEqualTimestampOperation(t, "op-b-equal-newer", command.ID, domain.CapabilityDelete, requestedAt)
	history := []domain.Operation{equalOlder, equalNewer}
	for number := 3; number <= 11; number++ {
		history = append(history, mustEqualTimestampOperation(t,
			domain.OperationID(fmt.Sprintf("op-%02d-equal", number)), command.ID, domain.CapabilityUpdate, requestedAt))
	}
	newest := history[len(history)-1]

	err = store.Within(ctx, func(tx application.UnitOfWork) error {
		for _, operation := range history {
			if err := tx.Operations().CreateOperation(ctx, application.OperationRecord{Operation: operation, Version: 1}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	latestFromPostgres := func(t *testing.T) domain.OperationID {
		t.Helper()
		var latestID domain.OperationID
		err := store.Within(ctx, func(tx application.UnitOfWork) error {
			latest, found, err := tx.Operations().LatestForResource(ctx, command.ID)
			if err != nil {
				return err
			}
			if !found {
				return fmt.Errorf("no operations found for resource %q", command.ID)
			}
			latestID = latest.Operation.ID()
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return latestID
	}

	for attempt := 0; attempt < 25; attempt++ {
		if got := latestFromPostgres(t); got != newest.ID() {
			t.Fatalf("attempt %d: latest selected %q, want last inserted %q", attempt, got, newest.ID())
		}
	}
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		first, err := tx.Operations().PageForResource(ctx, command.ID, 0, 2)
		if err != nil {
			return err
		}
		if len(first.Records) != 2 || first.Records[0].Operation.ID() != newest.ID() || first.Records[1].Operation.ID() != history[len(history)-2].ID() || first.NextSequence == 0 {
			return fmt.Errorf("unexpected first operation page: %#v", first)
		}
		second, err := tx.Operations().PageForResource(ctx, command.ID, first.NextSequence, 2)
		if err != nil {
			return err
		}
		if len(second.Records) != 2 || second.Records[0].Operation.ID() != history[len(history)-3].ID() || second.Records[1].Operation.ID() != history[len(history)-4].ID() || second.NextSequence == 0 {
			return fmt.Errorf("unexpected second operation page: %#v", second)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The fake repository must agree with PostgreSQL on identical insertion
	// order, including request timestamps that run backwards.
	fakeStore := applicationfake.NewStore()
	err = fakeStore.Within(ctx, func(tx application.UnitOfWork) error {
		create := admitted.Operation
		if err := tx.Operations().CreateOperation(ctx, application.OperationRecord{Operation: create, Version: 1}); err != nil {
			return err
		}
		for _, operation := range history {
			if err := tx.Operations().CreateOperation(ctx, application.OperationRecord{Operation: operation, Version: 1}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	pgLatest := latestFromPostgres(t)
	fakeLatest, fakeFound, err := fakeStore.LatestForResource(ctx, command.ID)
	if err != nil || !fakeFound {
		t.Fatalf("fake latest found=%t err=%v", fakeFound, err)
	}
	if pgLatest != fakeLatest.Operation.ID() || pgLatest != newest.ID() {
		t.Fatalf("ordering disagreement: postgres selected %q, fake selected %q, want tiebreak winner %q",
			pgLatest, fakeLatest.Operation.ID(), newest.ID())
	}
}

func TestPostgresOutputRecoveryProvenanceRoundTripsAndIsImmutable(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	service, resolver := postgresService(t, store, provisioningfake.New(provisioningfake.ModeSynchronous))
	command := postgresCreateCommand(t, "resource-recovery-provenance", "operation-recovery-source", map[string]any{"n": int64(1)})
	admitted, err := service.AdmitCreateResource(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := worker.New(store, resolver)
	if err != nil {
		t.Fatal(err)
	}
	drainWorker(t, instance)
	source := getExecution(t, store, admitted.Operation.ID())
	if source.CurrentAttempt == 0 {
		t.Fatal("source execution has no submission attempt")
	}
	resource := getResource(t, store, command.ID)
	childOperation, err := domain.NewOperation("operation-recovery-child", command.ID, domain.CapabilityUpdate,
		resource.Resource.Generation(), command.RequestedAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	child := application.ProvisioningExecutionRecord{
		OperationID: childOperation.ID(), ProvisionerRef: source.ProvisionerRef, ResourceID: source.ResourceID,
		ResourceType: source.ResourceType, Capability: childOperation.Capability(), TargetGeneration: childOperation.TargetGeneration(),
		Spec: source.Spec, OutputMappingRef: "mapping-v2", OutputResolution: application.OutputResolutionPending,
		State: application.AttemptSucceeded, RecoverySourceOperationID: source.OperationID,
		RecoverySourceAttempt: source.CurrentAttempt, NextObservation: 1, Version: 1,
	}
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		if err := tx.Operations().CreateOperation(ctx, application.OperationRecord{Operation: childOperation, Version: 1}); err != nil {
			return err
		}
		return tx.Executions().CreateExecution(ctx, child)
	}); err != nil {
		t.Fatal(err)
	}
	reloaded := getExecution(t, store, child.OperationID)
	if reloaded.RecoverySourceOperationID != source.OperationID || reloaded.RecoverySourceAttempt != source.CurrentAttempt || reloaded.OutputMappingRef != child.OutputMappingRef {
		t.Fatalf("reloaded recovery provenance = %+v", reloaded)
	}
	expectPostgresFailure(t, func() error {
		_, err := pool.Exec(ctx, `UPDATE provisioning_executions SET recovery_source_attempt=recovery_source_attempt+1 WHERE operation_id=$1`, child.OperationID)
		return err
	})
	expectPostgresFailure(t, func() error {
		_, err := pool.Exec(ctx, `UPDATE provisioning_executions SET recovery_source_operation_id=NULL WHERE operation_id=$1`, child.OperationID)
		return err
	})
	expectPostgresFailure(t, func() error {
		_, err := pool.Exec(ctx, `UPDATE provisioning_executions SET record_version=record_version+1 WHERE operation_id=$1`, source.OperationID)
		return err
	})
	expectPostgresFailure(t, func() error {
		_, err := pool.Exec(ctx, `DELETE FROM provisioning_executions WHERE operation_id=$1`, source.OperationID)
		return err
	})
	expectPostgresFailure(t, func() error {
		_, err := pool.Exec(ctx, `UPDATE provisioning_submission_attempts SET failure_message='mutated' WHERE operation_id=$1 AND attempt_number=$2`, source.OperationID, source.CurrentAttempt)
		return err
	})
	expectPostgresFailure(t, func() error {
		_, err := pool.Exec(ctx, `DELETE FROM provisioning_submission_attempts WHERE operation_id=$1 AND attempt_number=$2`, source.OperationID, source.CurrentAttempt)
		return err
	})
}

func TestPostgresOperationRetryConstraintsAndTerminalImmutability(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	service, _ := postgresService(t, store, provisioningfake.New(provisioningfake.ModeSynchronous))
	command := postgresCreateCommand(t, "resource-retry-history", "operation-failed-source", map[string]any{"n": int64(1)})
	if _, err := service.AdmitCreateResource(ctx, command); err != nil {
		t.Fatal(err)
	}

	var source application.OperationRecord
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		var err error
		source, err = tx.Operations().GetOperation(ctx, command.OperationID)
		if err != nil {
			return err
		}
		if err := source.Operation.Fail("DispatchFailed", "failed before dispatch", command.RequestedAt.Add(time.Second)); err != nil {
			return err
		}
		return tx.Operations().SaveOperation(ctx, application.OperationRecord{Operation: source.Operation}, source.Version)
	}); err != nil {
		t.Fatal(err)
	}

	retry, err := domain.NewOperation("operation-valid-retry", command.ID, source.Operation.Capability(), source.Operation.TargetGeneration(), command.RequestedAt.Add(time.Minute), source.Operation.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		return tx.Operations().CreateOperation(ctx, application.OperationRecord{Operation: retry})
	}); err != nil {
		t.Fatalf("valid retry rejected: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE operations SET failure_message='mutated' WHERE id=$1`, source.Operation.ID()); err == nil {
		t.Fatal("terminal operation row was mutable")
	}
	if _, err := pool.Exec(ctx, `UPDATE operations SET operation_seq=operation_seq+100 WHERE id=$1`, retry.ID()); err == nil {
		t.Fatal("operation sequence was mutable")
	}
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		record, err := tx.Operations().GetOperation(ctx, retry.ID())
		if err != nil {
			return err
		}
		if err := record.Operation.Cancel(command.RequestedAt.Add(2 * time.Minute)); err != nil {
			return err
		}
		return tx.Operations().SaveOperation(ctx, application.OperationRecord{Operation: record.Operation}, record.Version)
	}); err != nil {
		t.Fatal(err)
	}
	invalidRetry, err := domain.NewOperation("operation-invalid-retry", command.ID, retry.Capability(), retry.TargetGeneration(), command.RequestedAt.Add(3*time.Minute), retry.ID())
	if err != nil {
		t.Fatal(err)
	}
	err = store.Within(ctx, func(tx application.UnitOfWork) error {
		return tx.Operations().CreateOperation(ctx, application.OperationRecord{Operation: invalidRetry})
	})
	if !errors.Is(err, application.ErrInvalidApplicationCall) {
		t.Fatalf("non-failed retry source error = %v, want ErrInvalidApplicationCall", err)
	}
}

// TestPostgresGetOperationMissingReturnsOperationNotFound pins that reads of
// unknown Operations surface an Operation-specific sentinel.
func TestPostgresGetOperationMissingReturnsOperationNotFound(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	err = store.Within(ctx, func(tx application.UnitOfWork) error {
		_, err := tx.Operations().GetOperation(ctx, "operation-missing")
		return err
	})
	if !errors.Is(err, application.ErrOperationNotFound) {
		t.Fatalf("missing operation error = %v, want ErrOperationNotFound", err)
	}
}

func mustEqualTimestampOperation(t *testing.T, id domain.OperationID, resourceID domain.ResourceID, capability domain.Capability, requestedAt time.Time) domain.Operation {
	t.Helper()
	operation, err := domain.NewOperation(id, resourceID, capability, 2, requestedAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := operation.Fail("TestCleanup", "terminal for ordering test", requestedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	return operation
}

func TestPostgresPersistenceRestartWorkerAndImmutableIntent(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	provider := provisioningfake.New(provisioningfake.ModeSynchronous)
	service, resolver := postgresService(t, store, provider)
	command := postgresCreateCommand(t, "resource-persisted", "operation-persisted", map[string]any{
		"min": int64(-1), "max": uint64(^uint64(0)), "float": float32(1.25), "nested": []any{uint8(2), map[string]any{"enabled": true}},
	})
	if _, err := service.AdmitCreateResource(ctx, command); err != nil {
		t.Fatal(err)
	}
	instance, err := worker.New(store, resolver)
	if err != nil {
		t.Fatal(err)
	}
	instance.RetryBase = 0
	drainWorker(t, instance)

	restarted, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	replayService, _ := postgresService(t, restarted, provider)
	replay, err := replayService.AdmitCreateResource(ctx, command)
	if err != nil || !replay.Replay || replay.Operation.ID() != command.OperationID {
		t.Fatalf("restart replay error=%v replay=%t operation=%q", err, replay.Replay, replay.Operation.ID())
	}

	originalExecution := getExecution(t, restarted, command.OperationID)
	if !reflect.DeepEqual(originalExecution.Spec.Values(), command.Spec.Values()) {
		t.Fatal("submitted spec changed during PostgreSQL round trip")
	}
	newSpec, err := domain.NewResourceSpec(map[string]any{"replacement": "desired"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replayService.AdmitUpdateResource(ctx, application.UpdateResourceCommand{Actor: applicationfake.Principal("tester"), ID: command.ID, ExpectedGeneration: 1, Spec: newSpec,
		OperationID: "operation-update", EventID: "event-update", RequestedAt: replay.Resource.Status.UpdatedAt().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	afterUpdate := getExecution(t, restarted, command.OperationID)
	if !reflect.DeepEqual(afterUpdate.Spec.Values(), command.Spec.Values()) {
		t.Fatal("submitted spec mutated after later Resource update")
	}
}

func TestPostgresTransactionRollbackAndCrossRowConstraints(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, _ := postgres.NewStore(pool)
	sentinel := errors.New("rollback")
	err := store.Within(ctx, func(tx application.UnitOfWork) error {
		spec, _ := domain.NewResourceSpec(nil)
		resource, _ := domain.NewResource("rolled-back", provisioningfake.ResourceType(), domain.OwnerRef{Kind: "team", ID: "x"}, spec, time.Now().UTC())
		status, _ := domain.NewResourceStatus(resource.ID(), 0, domain.ResourceStateUnknown, nil, resource.CreatedAt())
		ref, _ := application.NewProvisionerRef("provider")
		if err := tx.Resources().CreateResource(ctx, application.ResourceRecord{Resource: resource, Status: status, ProvisionerRef: ref, Version: 1}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("rollback error=%v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM resources WHERE id='rolled-back'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("rolled back resources=%d error=%v", count, err)
	}
}

func TestPostgresTransactionPanicRollsBack(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, _ := postgres.NewStore(pool)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("transaction callback did not panic")
			}
		}()
		_ = store.Within(ctx, func(tx application.UnitOfWork) error {
			spec, _ := domain.NewResourceSpec(nil)
			resource, _ := domain.NewResource("panic-rollback", provisioningfake.ResourceType(), domain.OwnerRef{Kind: "team", ID: "x"}, spec, time.Now().UTC())
			status, _ := domain.NewResourceStatus(resource.ID(), 0, domain.ResourceStateUnknown, nil, resource.CreatedAt())
			ref, _ := application.NewProvisionerRef("provider")
			if err := tx.Resources().CreateResource(ctx, application.ResourceRecord{Resource: resource, Status: status, ProvisionerRef: ref, Version: 1}); err != nil {
				t.Fatal(err)
			}
			panic("test panic")
		})
	}()
	var count int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM resources WHERE id='panic-rollback'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("panic rollback resources=%d error=%v", count, err)
	}
}

func TestPostgresOutboxUsesServerTimeAndFencesClaims(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, _ := postgres.NewStore(pool)
	provider := provisioningfake.New(provisioningfake.ModeSynchronous)
	service, _ := postgresService(t, store, provider)
	command := postgresCreateCommand(t, "resource-lease", "operation-lease", map[string]any{})
	if _, err := service.AdmitCreateResource(ctx, command); err != nil {
		t.Fatal(err)
	}
	var first application.OutboxMessage
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		var found bool
		var err error
		first, found, err = tx.Outbox().ClaimOutbox(ctx, "token-one", time.Minute)
		if err == nil && !found {
			return errors.New("no claimable work")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		_, found, err := tx.Outbox().ClaimOutbox(ctx, "token-two", time.Minute)
		if err == nil && found {
			return errors.New("lease was concurrently reclaimed")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		return tx.Outbox().RenewOutbox(ctx, first.ID, "token-one", time.Minute)
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		_, found, err := tx.Outbox().ClaimOutbox(ctx, "token-before-renewed-expiry", time.Minute)
		if err == nil && found {
			return errors.New("renewed lease was reclaimed")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Lease expiry is decided by PostgreSQL server time. Positioning the
	// deadline on the server clock makes the reclamation assertion
	// deterministic without sleeping past millisecond-scale windows.
	if _, err := pool.Exec(ctx,
		"UPDATE outbox_messages SET leased_until = clock_timestamp() - interval '1 microsecond' WHERE id=$1", first.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		reclaimed, found, err := tx.Outbox().ClaimOutbox(ctx, "token-three", time.Minute)
		if err == nil && (!found || reclaimed.ID != first.ID || reclaimed.LeaseToken != "token-three") {
			return errors.New("expired non-dispatch lease was not reclaimed")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresExpiredPendingDispatchRequeuesSameMessage(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, _ := postgres.NewStore(pool)
	provider := provisioningfake.New(provisioningfake.ModeSynchronous)
	service, resolver := postgresService(t, store, provider)
	command := postgresCreateCommand(t, "resource-pending-dispatch", "operation-pending-dispatch", map[string]any{"v": uint64(1)})
	if _, err := service.AdmitCreateResource(ctx, command); err != nil {
		t.Fatal(err)
	}
	instance, _ := worker.New(store, resolver)
	for range 3 {
		if _, err := instance.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	instance.Lease = 10 * time.Millisecond
	var dispatch application.OutboxMessage
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		var found bool
		var err error
		dispatch, found, err = tx.Outbox().ClaimOutbox(ctx, "pending-crash-token", instance.Lease)
		if err == nil && (!found || dispatch.Kind != application.OutboxDispatch) {
			return errors.New("Dispatch was not claimed")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if worked, err := instance.RunOnce(ctx); err != nil || !worked {
		t.Fatalf("pending recovery worked=%t error=%v", worked, err)
	}
	var messageState, executionState, attemptState string
	if err := pool.QueryRow(ctx, `SELECT m.state,e.state,a.state FROM outbox_messages m
		JOIN provisioning_executions e ON e.operation_id=m.operation_id
		JOIN provisioning_submission_attempts a ON a.operation_id=e.operation_id AND a.attempt_number=e.current_attempt_number
		WHERE m.id=$1`, dispatch.ID).Scan(&messageState, &executionState, &attemptState); err != nil {
		t.Fatal(err)
	}
	if messageState != "Pending" || executionState != "Pending" || attemptState != "Pending" {
		t.Fatalf("message=%s execution=%s attempt=%s", messageState, executionState, attemptState)
	}
}

func TestPostgresNotAttemptedSubmissionRequeuesSameAttemptAndRefreshesFence(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, _ := postgres.NewStore(pool)
	provider := &postgresNotAttemptedOnceProvider{}
	service, resolver := postgresService(t, store, provider)
	command := postgresCreateCommand(t, "resource-not-attempted", "operation-not-attempted", map[string]any{"v": uint64(1)})
	if _, err := service.AdmitCreateResource(ctx, command); err != nil {
		t.Fatal(err)
	}
	instance, _ := worker.New(store, resolver)
	instance.RetryBase = 0
	for range 3 {
		if _, err := instance.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if worked, err := instance.RunOnce(ctx); !worked || !errors.Is(err, provisioning.ErrSubmissionNotAttempted) {
		t.Fatalf("first dispatch worked=%t error=%v", worked, err)
	}
	var messageState, executionState, attemptState, attemptNumber, expectedVersion, executionVersion string
	if err := pool.QueryRow(ctx, `SELECT m.state,e.state,a.state,m.attempt_number::text,m.expected_version::text,e.record_version::text
		FROM outbox_messages m JOIN provisioning_executions e ON e.operation_id=m.operation_id
		JOIN provisioning_submission_attempts a ON a.operation_id=e.operation_id AND a.attempt_number=m.attempt_number
		WHERE m.id=$1`, application.DispatchMessage(command.OperationID, 1, 0).ID).Scan(
		&messageState, &executionState, &attemptState, &attemptNumber, &expectedVersion, &executionVersion); err != nil {
		t.Fatal(err)
	}
	if messageState != "Pending" || executionState != "Pending" || attemptState != "Pending" || attemptNumber != "1" || expectedVersion != executionVersion {
		t.Fatalf("message=%s execution=%s attempt=%s number=%s expected=%s version=%s", messageState, executionState, attemptState, attemptNumber, expectedVersion, executionVersion)
	}
	if worked, err := instance.RunOnce(ctx); err != nil || !worked {
		t.Fatalf("second dispatch worked=%t error=%v", worked, err)
	}
	if provider.calls != 2 {
		t.Fatalf("Submit calls=%d, want 2", provider.calls)
	}
}

func TestPostgresRetryDispatchVersionMismatchRollsBackAttemptAndExecution(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, _ := postgres.NewStore(pool)
	provider := provisioningfake.New(provisioningfake.ModeSynchronous)
	service, _ := postgresService(t, store, provider)
	command := postgresCreateCommand(t, "resource-retry-rollback", "operation-retry-rollback", map[string]any{"v": uint64(1)})
	if _, err := service.AdmitCreateResource(ctx, command); err != nil {
		t.Fatal(err)
	}
	instance, _ := worker.New(store, &applicationfake.Resolver{})
	for range 3 {
		if _, err := instance.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var dispatch application.OutboxMessage
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		var found bool
		var err error
		dispatch, found, err = tx.Outbox().ClaimOutbox(ctx, "retry-rollback-token", time.Minute)
		if err == nil && (!found || dispatch.Kind != application.OutboxDispatch) {
			return errors.New("Dispatch was not claimed")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		attempt, err := tx.SubmissionAttempts().GetSubmissionAttempt(ctx, command.OperationID, 1)
		if err != nil {
			return err
		}
		attempt.State = application.SubmissionAttemptLeased
		if err := tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, attempt, application.SubmissionAttemptPending); err != nil {
			return err
		}
		execution, err := tx.Executions().GetExecution(ctx, command.OperationID)
		if err != nil {
			return err
		}
		execution.State = application.AttemptDispatching
		return tx.Executions().SaveExecution(ctx, execution, execution.Version)
	}); err != nil {
		t.Fatal(err)
	}

	before := getExecution(t, store, command.OperationID)
	err := store.Within(ctx, func(tx application.UnitOfWork) error {
		attempt, err := tx.SubmissionAttempts().GetSubmissionAttempt(ctx, command.OperationID, 1)
		if err != nil {
			return err
		}
		attempt.State = application.SubmissionAttemptPending
		attempt.ClaimedAt = time.Time{}
		if err := tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, attempt, application.SubmissionAttemptLeased); err != nil {
			return err
		}
		execution, err := tx.Executions().GetExecution(ctx, command.OperationID)
		if err != nil {
			return err
		}
		execution.State = application.AttemptPending
		if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
			return err
		}
		// The execution row is now version+1 inside this transaction. Supplying
		// its stale loaded version must fail the outbox CAS and roll everything back.
		return tx.Outbox().RetryDispatchOutbox(ctx, dispatch.ID, dispatch.LeaseToken, execution.Version, 0, "deliberate mismatch")
	})
	if !errors.Is(err, application.ErrConcurrencyConflict) {
		t.Fatalf("RetryDispatchOutbox error = %v, want ErrConcurrencyConflict", err)
	}
	after := getExecution(t, store, command.OperationID)
	var attempt application.SubmissionAttemptRecord
	var message application.OutboxMessage
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		var err error
		attempt, err = tx.SubmissionAttempts().GetSubmissionAttempt(ctx, command.OperationID, 1)
		if err != nil {
			return err
		}
		message, err = tx.Outbox().GetOutbox(ctx, dispatch.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if after.State != application.AttemptDispatching || after.Version != before.Version || attempt.State != application.SubmissionAttemptLeased ||
		message.State != application.OutboxLeased || message.LeaseToken != dispatch.LeaseToken || message.ExpectedVersion != dispatch.ExpectedVersion {
		t.Fatalf("failed retry was not atomic: before=%+v after=%+v attempt=%+v message=%+v", before, after, attempt, message)
	}
	if _, err := pool.Exec(ctx, `UPDATE outbox_messages SET leased_until=clock_timestamp()-interval '1 microsecond' WHERE id=$1`, dispatch.ID); err != nil {
		t.Fatal(err)
	}
	err = store.Within(ctx, func(tx application.UnitOfWork) error {
		attempt, err := tx.SubmissionAttempts().GetSubmissionAttempt(ctx, command.OperationID, 1)
		if err != nil {
			return err
		}
		attempt.State = application.SubmissionAttemptPending
		attempt.ClaimedAt = time.Time{}
		if err := tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, attempt, application.SubmissionAttemptLeased); err != nil {
			return err
		}
		execution, err := tx.Executions().GetExecution(ctx, command.OperationID)
		if err != nil {
			return err
		}
		execution.State = application.AttemptPending
		if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
			return err
		}
		return tx.Outbox().RetryExpiredDispatchOutbox(ctx, dispatch.ID, dispatch.LeaseToken, execution.Version, 0, "deliberate expired mismatch")
	})
	if !errors.Is(err, application.ErrConcurrencyConflict) {
		t.Fatalf("RetryExpiredDispatchOutbox error = %v, want ErrConcurrencyConflict", err)
	}
	after = getExecution(t, store, command.OperationID)
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		var err error
		attempt, err = tx.SubmissionAttempts().GetSubmissionAttempt(ctx, command.OperationID, 1)
		if err != nil {
			return err
		}
		message, err = tx.Outbox().GetOutbox(ctx, dispatch.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if after.State != application.AttemptDispatching || after.Version != before.Version || attempt.State != application.SubmissionAttemptLeased ||
		message.State != application.OutboxLeased || message.LeaseToken != dispatch.LeaseToken || message.ExpectedVersion != dispatch.ExpectedVersion {
		t.Fatalf("failed expired retry was not atomic: before=%+v after=%+v attempt=%+v message=%+v", before, after, attempt, message)
	}
}

func TestPostgresConcurrentUpdateDeleteLeavesOneActiveOperation(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, _ := postgres.NewStore(pool)
	provider := provisioningfake.New(provisioningfake.ModeSynchronous)
	service, resolver := postgresService(t, store, provider)
	command := postgresCreateCommand(t, "resource-concurrent", "operation-concurrent-create", map[string]any{"v": int64(1)})
	if _, err := service.AdmitCreateResource(ctx, command); err != nil {
		t.Fatal(err)
	}
	instance, _ := worker.New(store, resolver)
	instance.RetryBase = 0
	drainWorker(t, instance)
	resource := getResource(t, store, command.ID)
	updatedSpec, _ := domain.NewResourceSpec(map[string]any{"v": int64(2)})
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		_, err := service.AdmitUpdateResource(ctx, application.UpdateResourceCommand{Actor: applicationfake.Principal("tester"), ID: command.ID, ExpectedGeneration: 1, Spec: updatedSpec,
			OperationID: "operation-concurrent-update", EventID: "event-concurrent-update", RequestedAt: resource.Status.UpdatedAt().Add(time.Hour),
			IdempotencyKey: "concurrent-update"})
		results <- err
	}()
	go func() {
		<-start
		_, err := service.AdmitDeleteResource(ctx, application.DeleteResourceCommand{Actor: applicationfake.Principal("tester"), ID: command.ID, ExpectedGeneration: 1,
			OperationID: "operation-concurrent-delete", EventID: "event-concurrent-delete", RequestedAt: resource.Status.UpdatedAt().Add(2 * time.Hour),
			IdempotencyKey: "concurrent-delete"})
		results <- err
	}()
	close(start)
	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent commands=%d, want 1", successes)
	}
	var active int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM operations WHERE resource_id=$1 AND state IN ('Pending','Running')", command.ID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("active operations=%d error=%v", active, err)
	}
}

func TestPostgresConcurrentCreateIdempotencyPersistsOneAggregate(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, _ := postgres.NewStore(pool)
	service, _ := postgresService(t, store, provisioningfake.New(provisioningfake.ModeSynchronous))
	command := postgresCreateCommand(t, "resource-concurrent-create", "operation-concurrent-create", map[string]any{"v": uint64(1)})
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := service.AdmitCreateResource(ctx, command)
			results <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.AdmitDeleteResource(ctx, application.DeleteResourceCommand{Actor: applicationfake.Principal("tester"), ID: command.ID, ExpectedGeneration: 1,
		OperationID: "different-operation", EventID: "different-event", RequestedAt: command.RequestedAt.Add(time.Hour),
		IdempotencyKey: command.IdempotencyKey}); !errors.Is(err, application.ErrIdempotencyConflict) {
		t.Fatalf("cross-command idempotency error=%v", err)
	}
	for table, want := range map[string]int{"resources": 1, "operations": 1, "idempotency_records": 1, "outbox_messages": 1} {
		var count int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil || count != want {
			t.Fatalf("%s count=%d error=%v, want %d", table, count, err, want)
		}
	}
}

func TestPostgresNotFoundCreatesOneNewAttemptAndPreservesAudit(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, _ := postgres.NewStore(pool)
	provider := &postgresRecoveryProvider{}
	service, resolver := postgresService(t, store, provider)
	command := postgresCreateCommand(t, "resource-not-found", "operation-not-found", map[string]any{"v": uint64(1)})
	if _, err := service.AdmitCreateResource(ctx, command); err != nil {
		t.Fatal(err)
	}
	instance, _ := worker.New(store, resolver)
	instance.RetryBase = 0
	drainWorker(t, instance)
	provider.mu.Lock()
	submissions := provider.submissions
	provider.mu.Unlock()
	if submissions != 2 {
		t.Fatalf("submissions=%d, want 2", submissions)
	}
	var states []string
	rows, err := pool.Query(ctx, "SELECT state FROM provisioning_submission_attempts WHERE operation_id=$1 ORDER BY attempt_number", command.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		if err := rows.Scan(&state); err != nil {
			t.Fatal(err)
		}
		states = append(states, state)
	}
	if !reflect.DeepEqual(states, []string{"NotFound", "Accepted"}) {
		t.Fatalf("attempt states=%v", states)
	}
	var currentAttempt int
	if err := pool.QueryRow(ctx, "SELECT current_attempt_number FROM provisioning_executions WHERE operation_id=$1", command.OperationID).Scan(&currentAttempt); err != nil || currentAttempt != 2 {
		t.Fatalf("current attempt=%d error=%v", currentAttempt, err)
	}
}

func TestPostgresExpiredDispatchBecomesAmbiguousWithoutResubmission(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, _ := postgres.NewStore(pool)
	provider := provisioningfake.New(provisioningfake.ModeSynchronous)
	service, resolver := postgresService(t, store, provider)
	command := postgresCreateCommand(t, "resource-expired-dispatch", "operation-expired-dispatch", map[string]any{"v": uint64(1)})
	if _, err := service.AdmitCreateResource(ctx, command); err != nil {
		t.Fatal(err)
	}
	instance, _ := worker.New(store, resolver)
	instance.RetryBase = 0
	for range 3 {
		if _, err := instance.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	instance.Lease = 10 * time.Millisecond
	var dispatch application.OutboxMessage
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		var found bool
		var err error
		dispatch, found, err = tx.Outbox().ClaimOutbox(ctx, "dispatching-crash-token", instance.Lease)
		if err == nil && (!found || dispatch.Kind != application.OutboxDispatch) {
			return errors.New("Dispatch was not claimed")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		execution, err := tx.Executions().GetExecution(ctx, command.OperationID)
		if err != nil {
			return err
		}
		attempt, err := tx.SubmissionAttempts().GetSubmissionAttempt(ctx, command.OperationID, 1)
		if err != nil {
			return err
		}
		attempt.State = application.SubmissionAttemptLeased
		if err := tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, attempt, application.SubmissionAttemptPending); err != nil {
			return err
		}
		execution.State = application.AttemptDispatching
		return tx.Executions().SaveExecution(ctx, execution, execution.Version)
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if worked, err := instance.RunOnce(ctx); err != nil || !worked {
		t.Fatalf("expired recovery worked=%t error=%v", worked, err)
	}
	if provider.SubmissionCount(command.OperationID) != 0 {
		t.Fatalf("submissions=%d, want no blind submission", provider.SubmissionCount(command.OperationID))
	}
	var executionState, attemptState, terminalReason string
	if err := pool.QueryRow(ctx, `SELECT e.state,a.state,m.terminal_reason
		FROM provisioning_executions e
		JOIN provisioning_submission_attempts a ON a.operation_id=e.operation_id AND a.attempt_number=e.current_attempt_number
		JOIN outbox_messages m ON m.id=a.dispatch_message_id WHERE e.operation_id=$1`, command.OperationID).Scan(&executionState, &attemptState, &terminalReason); err != nil {
		t.Fatal(err)
	}
	if executionState != "Unknown" || attemptState != "Unknown" || terminalReason != "LeaseExpiredAmbiguous" {
		t.Fatalf("execution=%s attempt=%s reason=%s", executionState, attemptState, terminalReason)
	}
}

func TestPostgresEnforcesImmutableIdentityCrossRowAndOutboxIndexes(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, _ := postgres.NewStore(pool)
	provider := provisioningfake.New(provisioningfake.ModeSynchronous)
	service, resolver := postgresService(t, store, provider)
	first := postgresCreateCommand(t, "resource-integrity-one", "operation-integrity-one", map[string]any{"v": uint8(1)})
	second := postgresCreateCommand(t, "resource-integrity-two", "operation-integrity-two", map[string]any{"v": uint8(2)})
	if _, err := service.AdmitCreateResource(ctx, first); err != nil {
		t.Fatal(err)
	}
	instance, _ := worker.New(store, resolver)
	instance.RetryBase = 0
	drainWorker(t, instance)
	if _, err := service.AdmitCreateResource(ctx, second); err != nil {
		t.Fatal(err)
	}
	drainWorker(t, instance)

	expectPostgresFailure(t, func() error {
		_, err := pool.Exec(ctx, "UPDATE resources SET type_version='v2' WHERE id=$1", first.ID)
		return err
	})
	expectPostgresFailure(t, func() error {
		_, err := pool.Exec(ctx, "UPDATE provisioner_bindings SET provisioner_ref='changed' WHERE resource_id=$1", first.ID)
		return err
	})
	expectPostgresFailure(t, func() error {
		_, err := pool.Exec(ctx, "UPDATE provisioning_executions SET submitted_spec='{}'::jsonb WHERE operation_id=$1", first.OperationID)
		return err
	})
	expectPostgresFailure(t, func() error {
		_, err := pool.Exec(ctx, "UPDATE operations SET target_generation=2 WHERE id=$1", first.OperationID)
		return err
	})
	expectPostgresFailure(t, func() error {
		_, err := pool.Exec(ctx, "DELETE FROM provisioning_submission_attempts WHERE operation_id=$1 AND attempt_number=1", first.OperationID)
		return err
	})
	expectPostgresFailure(t, func() error {
		_, err := pool.Exec(ctx, "DELETE FROM outbox_messages WHERE id=$1", "dispatch:"+string(first.OperationID)+":1")
		return err
	})
	expectPostgresFailure(t, func() error {
		_, err := pool.Exec(ctx, `INSERT INTO events(id,resource_id,operation_id,generation,type,reason,message,occurred_at_ns)
			VALUES ('mismatched-event',$1,$2,1,'Audit','Mismatch','',1)`, second.ID, first.OperationID)
		return err
	})
	expectPostgresFailure(t, func() error {
		_, err := pool.Exec(ctx, `INSERT INTO resource_conditions(resource_id,type,status,reason,message,observed_generation,last_transition_at_ns)
			VALUES ('missing-resource','Ready','True','Test','',1,1)`)
		return err
	})

	insertWork := func(id, kind string, operationID any, resourceID any, attempt any) error {
		_, err := pool.Exec(ctx, `INSERT INTO outbox_messages(id,kind,operation_id,resource_id,attempt_number,dedupe_key,payload)
			VALUES ($1,$2,$3,$4,$5::numeric,$1,'{}'::jsonb)`, id, kind, operationID, resourceID, attempt)
		return err
	}
	if err := insertWork("coexist-drive", "Drive", first.OperationID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := insertWork("coexist-observe", "Observe", first.OperationID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO provisioning_submission_attempts(operation_id,attempt_number,state,dispatch_message_id)
		VALUES ($1,2,'Pending','coexist-dispatch'),($1,3,'Pending','duplicate-dispatch')`, first.OperationID); err != nil {
		t.Fatal(err)
	}
	expectPostgresFailure(t, func() error {
		_, err := pool.Exec(ctx, "UPDATE provisioning_submission_attempts SET dispatch_message_id='changed' WHERE operation_id=$1 AND attempt_number=2", first.OperationID)
		return err
	})
	if err := insertWork("coexist-dispatch", "Dispatch", first.OperationID, nil, "2"); err != nil {
		t.Fatal(err)
	}
	if err := insertWork("coexist-passive", "PassiveObserve", nil, first.ID, nil); err != nil {
		t.Fatal(err)
	}
	expectPostgresFailure(t, func() error { return insertWork("duplicate-drive", "Drive", first.OperationID, nil, nil) })
	expectPostgresFailure(t, func() error { return insertWork("duplicate-observe", "Observe", first.OperationID, nil, nil) })
	expectPostgresFailure(t, func() error { return insertWork("duplicate-dispatch", "Dispatch", first.OperationID, nil, "3") })
	expectPostgresFailure(t, func() error { return insertWork("duplicate-passive", "PassiveObserve", nil, first.ID, nil) })
}

func testPool(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	databaseURL := os.Getenv("LIFTR_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("LIFTR_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("liftr_test_%d_%d", time.Now().UnixNano(), schemaSequence.Add(1))
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	}
	return pool, cleanup
}

func migratedPool(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	pool, cleanup := testPool(t)
	if err := postgres.Migrate(context.Background(), pool); err != nil {
		cleanup()
		t.Fatal(err)
	}
	return pool, cleanup
}

func postgresService(t *testing.T, store *postgres.Store, provider provisioning.Provisioner) (*application.Service, *applicationfake.Resolver) {
	t.Helper()
	ref, _ := application.NewProvisionerRef("postgres-test-provider")
	selector := &applicationfake.Selector{Ref: ref}
	resolver := &applicationfake.Resolver{Providers: map[application.ProvisionerRef]provisioning.Provisioner{ref: provider}}
	typeValue, err := domain.NewResourceType(provisioningfake.ResourceType(), "PostgreSQL test resource", []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewService(applicationfake.Catalog{Types: map[domain.ResourceTypeRef]domain.ResourceType{provisioningfake.ResourceType(): typeValue}}, selector, resolver, store, applicationfake.AllowAll{})
	if err != nil {
		t.Fatal(err)
	}
	return service, resolver
}

func postgresCreateCommand(t *testing.T, resourceID domain.ResourceID, operationID domain.OperationID, values map[string]any) application.CreateResourceCommand {
	t.Helper()
	spec, err := domain.NewResourceSpec(values)
	if err != nil {
		t.Fatal(err)
	}
	return application.CreateResourceCommand{Actor: applicationfake.Principal("tester"), ID: resourceID, Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"}, Spec: spec,
		OperationID: operationID, EventID: domain.EventID("event-" + string(operationID)), RequestedAt: time.Date(2026, 8, 16, 12, 0, 0, 123, time.UTC),
		IdempotencyKey: "key-" + string(operationID)}
}

func getExecution(t *testing.T, store *postgres.Store, operationID domain.OperationID) application.ProvisioningExecutionRecord {
	t.Helper()
	var execution application.ProvisioningExecutionRecord
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		var err error
		execution, err = tx.Executions().GetExecution(context.Background(), operationID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return execution
}

func getResource(t *testing.T, store *postgres.Store, resourceID domain.ResourceID) application.ResourceRecord {
	t.Helper()
	var resource application.ResourceRecord
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		var err error
		resource, err = tx.Resources().GetResource(context.Background(), resourceID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return resource
}

func expectPostgresFailure(t *testing.T, call func() error) {
	t.Helper()
	if err := call(); err == nil {
		t.Fatal("PostgreSQL accepted an invalid mutation")
	}
}

func drainWorker(t *testing.T, instance *worker.Worker) {
	t.Helper()
	for range 16 {
		worked, err := instance.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !worked {
			return
		}
	}
	t.Fatal("worker did not drain")
}

type postgresRecoveryProvider struct {
	mu          sync.Mutex
	submissions int
}

type postgresNotAttemptedOnceProvider struct{ calls int }

func (*postgresNotAttemptedOnceProvider) Capabilities() []provisioning.ProvisionerCapability {
	return nil
}

func (p *postgresNotAttemptedOnceProvider) Submit(context.Context, provisioning.ExecutionRequest) (provisioning.Submission, error) {
	p.calls++
	if p.calls == 1 {
		return provisioning.Submission{}, provisioning.SubmissionNotAttemptedError{Failure: provisioning.ExecutionFailure{
			Kind: provisioning.FailureTimeout, Reason: "StartupTimeout", Message: "execution did not start",
		}}
	}
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded}, Resource: provisioning.ResourceObservation{
			Presence: provisioning.ResourcePresencePresent, Readiness: provisioning.ResourceReadinessReady, Drift: provisioning.ResourceDriftInSync,
		}}}, nil
}

func (*postgresNotAttemptedOnceProvider) Observe(context.Context, provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return provisioning.ExecutionObservation{}, errors.New("unexpected Observe")
}

type postgresBlockingProvider struct {
	mu          sync.Mutex
	submissions int
	started     chan struct{}
	release     chan struct{}
}

func newPostgresBlockingProvider() *postgresBlockingProvider {
	return &postgresBlockingProvider{started: make(chan struct{}), release: make(chan struct{})}
}

func (p *postgresBlockingProvider) Capabilities() []provisioning.ProvisionerCapability { return nil }

func (p *postgresBlockingProvider) Submit(context.Context, provisioning.ExecutionRequest) (provisioning.Submission, error) {
	p.mu.Lock()
	p.submissions++
	p.mu.Unlock()
	close(p.started)
	<-p.release
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted}}}, nil
}

func (p *postgresBlockingProvider) Observe(context.Context, provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateRunning}}, nil
}

func (p *postgresRecoveryProvider) Capabilities() []provisioning.ProvisionerCapability { return nil }

func (p *postgresRecoveryProvider) Submit(context.Context, provisioning.ExecutionRequest) (provisioning.Submission, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.submissions++
	if p.submissions == 1 {
		return provisioning.Submission{}, provisioning.ErrAmbiguousSubmission
	}
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded}, Resource: provisioning.ResourceObservation{
			Presence: provisioning.ResourcePresencePresent, Readiness: provisioning.ResourceReadinessReady, Drift: provisioning.ResourceDriftInSync,
		}}}, nil
}

func (p *postgresRecoveryProvider) Observe(context.Context, provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationNotFound, Resource: provisioning.ResourceObservation{
		Presence: provisioning.ResourcePresenceUnknown, Readiness: provisioning.ResourceReadinessUnknown, Drift: provisioning.ResourceDriftUnknown,
	}}, nil
}
