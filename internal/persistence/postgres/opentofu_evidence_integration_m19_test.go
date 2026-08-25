// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/persistence/postgres"
	"github.com/sithea-nou/liftr/internal/provisioning/opentofu"
)

func TestOpenTofuEvidenceFencingAndDurabilityM19(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("stale worker after lease loss is rejected", func(t *testing.T) {
		fixture := createOpenTofuEvidenceFixture(t, pool, "stale-token")
		prepared, err := store.PrepareAttempt(ctx, fixture.key, fixture.fence)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE outbox_messages SET lease_token='replacement-token'
			WHERE id=$1`, fixture.fence.MessageID); err != nil {
			t.Fatal(err)
		}
		_, err = store.AdvanceAttempt(ctx, fixture.key, fixture.fence, prepared.Phase, prepared.Version, opentofu.AttemptApplyMayStart)
		if !errors.Is(err, opentofu.ErrFenceRejected) {
			t.Fatalf("stale transition error = %v, want ErrFenceRejected", err)
		}
		persisted, err := store.LoadAttempt(ctx, fixture.key)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.Phase != opentofu.AttemptPrepared || persisted.Version != 1 {
			t.Fatalf("stale worker changed evidence: %+v", persisted)
		}
	})

	t.Run("server-time expired lease is rejected", func(t *testing.T) {
		fixture := createOpenTofuEvidenceFixture(t, pool, "stale-expiry")
		prepared, err := store.PrepareAttempt(ctx, fixture.key, fixture.fence)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE outbox_messages
			SET leased_until=clock_timestamp()-interval '1 microsecond' WHERE id=$1`, fixture.fence.MessageID); err != nil {
			t.Fatal(err)
		}
		_, err = store.AdvanceAttempt(ctx, fixture.key, fixture.fence, prepared.Phase, prepared.Version, opentofu.AttemptApplyMayStart)
		if !errors.Is(err, opentofu.ErrFenceRejected) {
			t.Fatalf("expired transition error = %v, want ErrFenceRejected", err)
		}
	})

	t.Run("concurrent attempt CAS has one winner", func(t *testing.T) {
		fixture := createOpenTofuEvidenceFixture(t, pool, "attempt-cas")
		prepared, err := store.PrepareAttempt(ctx, fixture.key, fixture.fence)
		if err != nil {
			t.Fatal(err)
		}
		var wait sync.WaitGroup
		errs := make(chan error, 2)
		for range 2 {
			wait.Add(1)
			go func() {
				defer wait.Done()
				_, advanceErr := store.AdvanceAttempt(ctx, fixture.key, fixture.fence, prepared.Phase, prepared.Version, opentofu.AttemptApplyMayStart)
				errs <- advanceErr
			}()
		}
		wait.Wait()
		close(errs)
		var successes, conflicts int
		for advanceErr := range errs {
			switch {
			case advanceErr == nil:
				successes++
			case errors.Is(advanceErr, opentofu.ErrEvidenceConflict):
				conflicts++
			default:
				t.Fatalf("unexpected CAS error: %v", advanceErr)
			}
		}
		if successes != 1 || conflicts != 1 {
			t.Fatalf("CAS successes=%d conflicts=%d", successes, conflicts)
		}
	})

	t.Run("regression and terminal mutation are impossible", func(t *testing.T) {
		fixture := createOpenTofuEvidenceFixture(t, pool, "monotonic")
		record, err := store.PrepareAttempt(ctx, fixture.key, fixture.fence)
		if err != nil {
			t.Fatal(err)
		}
		record, err = store.AdvanceAttempt(ctx, fixture.key, fixture.fence, record.Phase, record.Version, opentofu.AttemptApplyMayStart)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.AdvanceAttempt(ctx, fixture.key, fixture.fence, record.Phase, record.Version, opentofu.AttemptPrepared); !errors.Is(err, opentofu.ErrEvidenceConflict) {
			t.Fatalf("API regression error = %v, want ErrEvidenceConflict", err)
		}
		expectPostgresFailure(t, func() error {
			_, directErr := pool.Exec(ctx, `UPDATE opentofu_attempt_evidence
				SET phase='Prepared',record_version=record_version+1 WHERE resource_id=$1`, fixture.key.ResourceID)
			return directErr
		})
		record, err = store.AdvanceAttempt(ctx, fixture.key, fixture.fence, record.Phase, record.Version, opentofu.AttemptApplyExited)
		if err != nil {
			t.Fatal(err)
		}
		record, err = store.AdvanceAttempt(ctx, fixture.key, fixture.fence, record.Phase, record.Version, opentofu.AttemptObservedConverged)
		if err != nil {
			t.Fatal(err)
		}
		expectPostgresFailure(t, func() error {
			_, directErr := pool.Exec(ctx, `UPDATE opentofu_attempt_evidence
				SET updated_at=clock_timestamp(),record_version=record_version+1 WHERE resource_id=$1`, fixture.key.ResourceID)
			return directErr
		})
	})

	t.Run("ApplyMayStart crash evidence is restart-readable", func(t *testing.T) {
		fixture := createOpenTofuEvidenceFixture(t, pool, "restart-attempt")
		record, err := store.PrepareAttempt(ctx, fixture.key, fixture.fence)
		if err != nil {
			t.Fatal(err)
		}
		record, err = store.AdvanceAttempt(ctx, fixture.key, fixture.fence, record.Phase, record.Version, opentofu.AttemptApplyMayStart)
		if err != nil {
			t.Fatal(err)
		}
		restarted, err := postgres.NewStore(pool)
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := restarted.LoadAttempt(ctx, fixture.key)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Phase != opentofu.AttemptApplyMayStart || loaded.Version != 2 {
			t.Fatalf("restart loaded %+v", loaded)
		}
	})

	t.Run("state binding is immutable durable and digest-monotonic", func(t *testing.T) {
		fixture := createOpenTofuEvidenceFixture(t, pool, "state")
		attempt, err := store.PrepareAttempt(ctx, fixture.key, fixture.fence)
		if err != nil {
			t.Fatal(err)
		}
		identity := opentofu.StateBindingIdentity{
			ResourceID: fixture.key.ResourceID, ProvisionerRef: fixture.key.ProvisionerRef,
			Engine: "opentofu/1.12.6", Program: "programs/network/v3", Backend: "s3-eu",
			StateKey: "liftr/resources/" + string(fixture.key.ResourceID) + ".tfstate",
		}
		binding, err := store.CreateStateBinding(ctx, fixture.key, fixture.fence, identity)
		if err != nil {
			t.Fatal(err)
		}
		first := opentofu.StateEvidence{Lineage: "5adbb45e-f672-4c2a-98fb-16c7d04b9a47", Serial: 7, Digest: sha256.Sum256([]byte("state-v7"))}
		if _, err := store.UpdateState(ctx, fixture.key, fixture.fence, binding.Version, first); !errors.Is(err, opentofu.ErrEvidenceConflict) {
			t.Fatalf("Prepared state update error = %v, want ErrEvidenceConflict", err)
		}
		if _, err := store.AdvanceAttempt(ctx, fixture.key, fixture.fence, attempt.Phase, attempt.Version, opentofu.AttemptApplyMayStart); err != nil {
			t.Fatal(err)
		}
		binding, err = store.UpdateState(ctx, fixture.key, fixture.fence, binding.Version, first)
		if err != nil {
			t.Fatal(err)
		}
		altered := first
		altered.Digest = sha256.Sum256([]byte("altered-state-v7"))
		if _, err := store.UpdateState(ctx, fixture.key, fixture.fence, binding.Version, altered); !errors.Is(err, opentofu.ErrEvidenceConflict) {
			t.Fatalf("same-serial altered digest error = %v, want ErrEvidenceConflict", err)
		}
		expectPostgresFailure(t, func() error {
			_, directErr := pool.Exec(ctx, `UPDATE opentofu_state_bindings SET program='programs/other/v1',
				record_version=record_version+1 WHERE resource_id=$1`, fixture.key.ResourceID)
			return directErr
		})
		restarted, err := postgres.NewStore(pool)
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := restarted.LoadStateBinding(ctx, fixture.key.ResourceID)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Identity != identity || loaded.State == nil || *loaded.State != first || loaded.Version != 2 {
			t.Fatalf("restart loaded state binding %+v", loaded)
		}
	})

	t.Run("concurrent state CAS has one winner", func(t *testing.T) {
		fixture := createOpenTofuEvidenceFixture(t, pool, "state-cas")
		attempt, err := store.PrepareAttempt(ctx, fixture.key, fixture.fence)
		if err != nil {
			t.Fatal(err)
		}
		binding, err := store.CreateStateBinding(ctx, fixture.key, fixture.fence, opentofu.StateBindingIdentity{
			ResourceID: fixture.key.ResourceID, ProvisionerRef: fixture.key.ProvisionerRef,
			Engine: "opentofu/1.12.6", Program: "programs/database/v2", Backend: "gcs-primary", StateKey: "db.tfstate",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.AdvanceAttempt(ctx, fixture.key, fixture.fence, attempt.Phase, attempt.Version, opentofu.AttemptApplyMayStart); err != nil {
			t.Fatal(err)
		}
		states := []opentofu.StateEvidence{
			{Lineage: "fd3f5f97-b987-4cc4-8ef7-1864a47d6ddd", Serial: 1, Digest: sha256.Sum256([]byte("winner-a"))},
			{Lineage: "fd3f5f97-b987-4cc4-8ef7-1864a47d6ddd", Serial: 1, Digest: sha256.Sum256([]byte("winner-b"))},
		}
		var wait sync.WaitGroup
		errs := make(chan error, len(states))
		for _, state := range states {
			wait.Add(1)
			go func() {
				defer wait.Done()
				_, updateErr := store.UpdateState(ctx, fixture.key, fixture.fence, binding.Version, state)
				errs <- updateErr
			}()
		}
		wait.Wait()
		close(errs)
		var successes, conflicts int
		for updateErr := range errs {
			switch {
			case updateErr == nil:
				successes++
			case errors.Is(updateErr, opentofu.ErrEvidenceConflict):
				conflicts++
			default:
				t.Fatalf("unexpected state CAS error: %v", updateErr)
			}
		}
		if successes != 1 || conflicts != 1 {
			t.Fatalf("state CAS successes=%d conflicts=%d", successes, conflicts)
		}
	})
}

func TestOpenTofuObserveRecoveryFenceM19(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	fixture := createOpenTofuEvidenceFixture(t, pool, "observe-recovery")
	dispatchFence := fixture.fence
	record, err := store.PrepareAttempt(ctx, fixture.key, dispatchFence)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := store.CreateStateBinding(ctx, fixture.key, dispatchFence, opentofu.StateBindingIdentity{
		ResourceID: fixture.key.ResourceID, ProvisionerRef: fixture.key.ProvisionerRef,
		Engine: "opentofu/1.12.6", Program: "programs/observe-recovery/v1", Backend: "s3-primary", StateKey: "observe-recovery.tfstate",
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.AdvanceAttempt(ctx, fixture.key, dispatchFence, record.Phase, record.Version, opentofu.AttemptApplyMayStart)
	if err != nil {
		t.Fatal(err)
	}

	recoveryOperationID := domain.OperationID("m19-operation-observe-recovery-child")
	observeFence := opentofu.LeaseFence{MessageID: "observe:" + string(recoveryOperationID) + ":1", Token: "observe-worker-b"}
	passiveFence := opentofu.LeaseFence{MessageID: "passive-observe:" + string(fixture.key.ResourceID) + ":1", Token: "passive-worker"}
	statements := []struct {
		query string
		args  []any
	}{
		{`UPDATE outbox_messages SET state='Completed',lease_token=NULL,leased_until=NULL,
			terminal_reason='LeaseExpiredAmbiguous',completed_at=clock_timestamp() WHERE id=$1`, []any{dispatchFence.MessageID}},
		{`UPDATE provisioning_submission_attempts SET state='Unknown' WHERE operation_id=$1 AND attempt_number=1`, []any{fixture.key.OperationID}},
		{`UPDATE provisioning_executions SET state='Unknown',correlation_status='Unknown' WHERE operation_id=$1`, []any{fixture.key.OperationID}},
		{`INSERT INTO operations(id,resource_id,capability,target_generation,state,phase,requested_at_ns,started_at_ns,completed_at_ns,phase_changed_at_ns,record_version)
			VALUES ($1,$2,'update',1,'Succeeded','Applying',2,2,2,2,1)`, []any{recoveryOperationID, fixture.key.ResourceID}},
		{`INSERT INTO provisioning_executions(operation_id,resource_id,provisioner_ref,resource_type_name,resource_type_version,
			capability,target_generation,spec_codec_version,submitted_spec,state,correlation_status,current_attempt_number,next_observation_sequence,
			output_mapping_ref,output_resolution,recovery_source_operation_id,recovery_source_attempt,record_version)
			VALUES ($1,$2,$3,'PrivateOpenTofuResource','v1','update',1,1,'{}','Succeeded','Found',0,2,
			'mapping-v2','Pending',$4,1,1)`, []any{recoveryOperationID, fixture.key.ResourceID, fixture.key.ProvisionerRef, fixture.key.OperationID}},
		{`INSERT INTO outbox_messages(id,kind,operation_id,dedupe_key,expected_version,sequence,state,lease_token,leased_until,payload)
			VALUES ($1,'Observe',$2,$1,1,1,'Leased',$3,clock_timestamp()+interval '10 minutes','{}')`, []any{observeFence.MessageID, recoveryOperationID, observeFence.Token}},
		{`INSERT INTO outbox_messages(id,kind,resource_id,dedupe_key,expected_version,sequence,state,lease_token,leased_until,payload)
			VALUES ($1,'PassiveObserve',$2,$1,1,1,'Leased',$3,clock_timestamp()+interval '10 minutes','{}')`, []any{passiveFence.MessageID, fixture.key.ResourceID, passiveFence.Token}},
	}
	for index, statement := range statements {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("recovery statement %d: %v", index+1, err)
		}
	}

	record, err = store.AdvanceAttempt(ctx, fixture.key, observeFence, record.Phase, record.Version, opentofu.AttemptApplyOutcomeUnknown)
	if err != nil {
		t.Fatalf("Observe ApplyMayStart -> ApplyOutcomeUnknown: %v", err)
	}
	if _, err := store.AdvanceAttempt(ctx, fixture.key, dispatchFence, opentofu.AttemptApplyOutcomeUnknown, record.Version, opentofu.AttemptObservedConverged); !errors.Is(err, opentofu.ErrFenceRejected) {
		t.Fatalf("stale Dispatch transition error = %v, want ErrFenceRejected", err)
	}
	if _, err := store.AdvanceAttempt(ctx, fixture.key, passiveFence, record.Phase, record.Version, opentofu.AttemptObservedConverged); !errors.Is(err, opentofu.ErrFenceRejected) {
		t.Fatalf("PassiveObserve transition error = %v, want ErrFenceRejected", err)
	}

	state := opentofu.StateEvidence{Lineage: "c49c9ac8-f779-4c3c-b228-af5d4159b0f1", Serial: 1, Digest: sha256.Sum256([]byte("observe-worker-b-state"))}
	if _, err := store.UpdateState(ctx, fixture.key, dispatchFence, binding.Version, state); !errors.Is(err, opentofu.ErrFenceRejected) {
		t.Fatalf("stale Dispatch state error = %v, want ErrFenceRejected", err)
	}
	if _, err := store.UpdateState(ctx, fixture.key, passiveFence, binding.Version, state); !errors.Is(err, opentofu.ErrFenceRejected) {
		t.Fatalf("PassiveObserve state error = %v, want ErrFenceRejected", err)
	}
	binding, err = store.UpdateState(ctx, fixture.key, observeFence, binding.Version, state)
	if err != nil {
		t.Fatalf("Observe state update: %v", err)
	}
	record, err = store.AdvanceAttempt(ctx, fixture.key, observeFence, record.Phase, record.Version, opentofu.AttemptObservedConverged)
	if err != nil {
		t.Fatalf("Observe ApplyOutcomeUnknown -> ObservedConverged: %v", err)
	}

	persisted, err := store.LoadAttempt(ctx, fixture.key)
	if err != nil {
		t.Fatal(err)
	}
	persistedBinding, err := store.LoadStateBinding(ctx, fixture.key.ResourceID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Key != record.Key || persisted.Phase != opentofu.AttemptObservedConverged || persisted.Version != 4 {
		t.Fatalf("attempt evidence was corrupted: persisted=%+v record=%+v", persisted, record)
	}
	if persistedBinding.Identity != binding.Identity || persistedBinding.State == nil || *persistedBinding.State != state || persistedBinding.Version != 2 {
		t.Fatalf("state evidence was corrupted: %+v", persistedBinding)
	}
}

type openTofuEvidenceFixture struct {
	key   opentofu.AttemptKey
	fence opentofu.LeaseFence
}

func createOpenTofuEvidenceFixture(t *testing.T, pool *pgxpool.Pool, suffix string) openTofuEvidenceFixture {
	t.Helper()
	ctx := context.Background()
	resourceID := domain.ResourceID("m19-resource-" + suffix)
	operationID := domain.OperationID("m19-operation-" + suffix)
	provisionerRef := "opentofu-registration-1.12.6"
	messageID := "dispatch:" + string(operationID) + ":1"
	token := "lease-token-" + suffix
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO resources(id,type_name,type_version,owner_kind,owner_id,generation,spec_codec_version,spec,record_version,created_at_ns,updated_at_ns)
			VALUES ($1,'PrivateOpenTofuResource','v1','team','platform',1,1,'{}',1,1,1)`, []any{resourceID}},
		{`INSERT INTO resource_statuses(resource_id,observed_generation,state,updated_at_ns) VALUES ($1,0,'Pending',1)`, []any{resourceID}},
		{`INSERT INTO provisioner_bindings(resource_id,provisioner_ref) VALUES ($1,$2)`, []any{resourceID, provisionerRef}},
		{`INSERT INTO operations(id,resource_id,capability,target_generation,state,phase,requested_at_ns,started_at_ns,phase_changed_at_ns,record_version)
			VALUES ($1,$2,'create',1,'Running','Applying',1,1,1,1)`, []any{operationID, resourceID}},
		{`INSERT INTO provisioning_executions(operation_id,resource_id,provisioner_ref,resource_type_name,resource_type_version,
			capability,target_generation,spec_codec_version,submitted_spec,state,correlation_status,current_attempt_number,next_observation_sequence,record_version)
			VALUES ($1,$2,$3,'PrivateOpenTofuResource','v1','create',1,1,'{}','Dispatching','Unknown',1,1,1)`, []any{operationID, resourceID, provisionerRef}},
		{`INSERT INTO provisioning_submission_attempts(operation_id,attempt_number,state,dispatch_message_id,claimed_at)
			VALUES ($1,1,'Leased',$2,clock_timestamp())`, []any{operationID, messageID}},
		{`INSERT INTO outbox_messages(id,kind,operation_id,attempt_number,dedupe_key,state,lease_token,leased_until,payload)
			VALUES ($1,'Dispatch',$2,1,$1,'Leased',$3,clock_timestamp()+interval '10 minutes','{}')`, []any{messageID, operationID, token}},
	}
	for index, statement := range statements {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("fixture statement %d: %v", index+1, err)
		}
	}
	return openTofuEvidenceFixture{
		key:   opentofu.AttemptKey{ResourceID: resourceID, OperationID: operationID, AttemptNumber: 1, ProvisionerRef: provisionerRef},
		fence: opentofu.LeaseFence{MessageID: messageID, Token: token},
	}
}
