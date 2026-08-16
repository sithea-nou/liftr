// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
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
	if _, err := replayService.AdmitUpdateResource(ctx, application.UpdateResourceCommand{ID: command.ID, ExpectedGeneration: 1, Spec: newSpec,
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
		first, found, err = tx.Outbox().ClaimOutbox(ctx, "token-one", 20*time.Millisecond)
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
	time.Sleep(10 * time.Millisecond)
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		return tx.Outbox().RenewOutbox(ctx, first.ID, "token-one", 30*time.Millisecond)
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(15 * time.Millisecond)
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		_, found, err := tx.Outbox().ClaimOutbox(ctx, "token-before-renewed-expiry", time.Minute)
		if err == nil && found {
			return errors.New("renewed lease was reclaimed")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
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
		_, err := service.AdmitUpdateResource(ctx, application.UpdateResourceCommand{ID: command.ID, ExpectedGeneration: 1, Spec: updatedSpec,
			OperationID: "operation-concurrent-update", EventID: "event-concurrent-update", RequestedAt: resource.Status.UpdatedAt().Add(time.Hour),
			IdempotencyKey: "concurrent-update", Fingerprint: "update"})
		results <- err
	}()
	go func() {
		<-start
		_, err := service.AdmitDeleteResource(ctx, application.DeleteResourceCommand{ID: command.ID, ExpectedGeneration: 1,
			OperationID: "operation-concurrent-delete", EventID: "event-concurrent-delete", RequestedAt: resource.Status.UpdatedAt().Add(2 * time.Hour),
			IdempotencyKey: "concurrent-delete", Fingerprint: "delete"})
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
	if _, err := service.AdmitDeleteResource(ctx, application.DeleteResourceCommand{ID: command.ID, ExpectedGeneration: 1,
		OperationID: "different-operation", EventID: "different-event", RequestedAt: command.RequestedAt.Add(time.Hour),
		IdempotencyKey: command.IdempotencyKey, Fingerprint: command.Fingerprint}); !errors.Is(err, application.ErrIdempotencyConflict) {
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
	service, err := application.NewService(applicationfake.Catalog{Types: map[domain.ResourceTypeRef]domain.ResourceType{provisioningfake.ResourceType(): typeValue}}, selector, resolver, store)
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
	return application.CreateResourceCommand{ID: resourceID, Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"}, Spec: spec,
		OperationID: operationID, EventID: domain.EventID("event-" + string(operationID)), RequestedAt: time.Date(2026, 8, 16, 12, 0, 0, 123, time.UTC),
		IdempotencyKey: "key-" + string(operationID), Fingerprint: "fingerprint-" + string(operationID)}
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
