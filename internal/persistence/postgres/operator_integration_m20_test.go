// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	applicationfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
	"github.com/sithea-nou/liftr/internal/persistence/postgres"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
)

type m20AllowOperator struct{}

func (m20AllowOperator) AuthorizeOperator(context.Context, identity.Principal, identity.Action, identity.OperatorTarget) error {
	return nil
}

func TestPostgresOperatorMutationConcurrentReplayAndImmutability(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	service, _ := postgresService(t, store, provisioningfake.New(provisioningfake.ModeAsynchronous))
	service.OperatorAuthorizer = m20AllowOperator{}
	resourceID := domain.ResourceID("m20-operator-resource")
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	resource, err := domain.NewResource(resourceID, provisioningfake.ResourceType(), domain.OwnerRef{Kind: "team", ID: "platform"}, mustSpec(t, map[string]any{"n": int64(1)}), now)
	if err != nil {
		t.Fatal(err)
	}
	status, err := domain.NewResourceStatus(resourceID, 1, domain.ResourceStateReady, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	ref, _ := application.NewProvisionerRef("postgres-test-provider")
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		return tx.Resources().CreateResource(ctx, application.ResourceRecord{
			Resource: resource, Status: status, ProvisionerRef: ref, Version: 1,
		})
	}); err != nil {
		t.Fatal(err)
	}

	actor := applicationfake.Principal("m20-operator")
	start := make(chan struct{})
	results := make(chan application.OperatorMutationResult, 2)
	errorsCh := make(chan error, 2)
	var wait sync.WaitGroup
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func(requestID string) {
			defer wait.Done()
			<-start
			result, err := service.TriggerResourcePassiveObservation(ctx, application.OperatorMutationCommand{
				Actor: actor, IdempotencyKey: "same-key", RequestID: requestID,
			}, resourceID)
			results <- result
			errorsCh <- err
		}("request-" + string(rune('a'+i)))
	}
	close(start)
	wait.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatalf("concurrent mutation: %v", err)
		}
	}
	var applied, replayed int
	var actionID, workID string
	for result := range results {
		if result.Replay {
			replayed++
		} else {
			applied++
		}
		if actionID == "" {
			actionID, workID = result.OperatorActionID, result.CreatedWorkID
		} else if actionID != result.OperatorActionID || workID != result.CreatedWorkID {
			t.Fatalf("replay identity changed: action=%q/%q work=%q/%q", actionID, result.OperatorActionID, workID, result.CreatedWorkID)
		}
	}
	if applied != 1 || replayed != 1 {
		t.Fatalf("applied=%d replayed=%d, want one each", applied, replayed)
	}

	for table, want := range map[string]int{"operator_actions": 1, "operator_idempotency": 1, "outbox_messages": 1} {
		var count int
		query := "SELECT count(*) FROM " + table
		if table == "outbox_messages" {
			query += " WHERE resource_id='m20-operator-resource'"
		}
		if err := pool.QueryRow(ctx, query).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != want {
			t.Fatalf("%s rows = %d, want %d", table, count, want)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE operator_actions SET request_id='changed' WHERE id=$1`, actionID); err == nil {
		t.Fatal("immutable operator action was updated")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM operator_idempotency WHERE operator_action_id=$1`, actionID); err == nil {
		t.Fatal("immutable operator idempotency binding was deleted")
	}

	_, err = service.TriggerResourcePassiveObservation(ctx, application.OperatorMutationCommand{
		Actor: actor, IdempotencyKey: "rejected-key", RequestID: "request-rejected",
	}, "missing")
	if !errors.Is(err, application.ErrResourceNotFound) {
		t.Fatalf("rejected mutation error = %v", err)
	}
	var rejected int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM operator_idempotency WHERE key='rejected-key'`).Scan(&rejected); err != nil {
		t.Fatal(err)
	}
	if rejected != 0 {
		t.Fatalf("rejected mutation bound %d idempotency rows", rejected)
	}
}

// TestPostgresOperationDiagnosticsBoundedUnderLargeHistory pins the M20
// bounding contract against real PostgreSQL: thousands of historical attempts
// and completed work rows must not change the response shape, the planner
// outcome, or the diagnostic revision — only the honest total counts move,
// and every history read stays LIMIT/GROUP BY-bounded at SQL level.
func TestPostgresOperationDiagnosticsBoundedUnderLargeHistory(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()
	ctx := context.Background()
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	service, _ := postgresService(t, store, provisioningfake.New(provisioningfake.ModeAsynchronous))
	service.OperatorAuthorizer = m20AllowOperator{}
	command := postgresCreateCommand(t, "m20-history-resource", "m20-history-op", map[string]any{"n": int64(1)})
	if _, err := service.AdmitCreateResource(ctx, command); err != nil {
		t.Fatal(err)
	}

	const historyPairs = 1250
	// Even attempt numbers only (2..2500): leaves gaps so later appends can
	// add genuinely historical rows without moving the latest attempt.
	if _, err := pool.Exec(ctx, `INSERT INTO provisioning_submission_attempts
		(operation_id,attempt_number,state,dispatch_message_id,resolved_at)
		SELECT 'm20-history-op', g*2, 'Rejected', 'seed-dispatch-'||g*2, clock_timestamp()
		FROM generate_series(1, $1) g`, historyPairs); err != nil {
		t.Fatal(err)
	}
	const historyRows = historyPairs * 2
	if _, err := pool.Exec(ctx, `INSERT INTO outbox_messages
		(id,kind,operation_id,dedupe_key,state,completed_at)
		SELECT 'seed-observe-'||g, 'Observe', 'm20-history-op', 'seed-observe-'||g, 'Completed', clock_timestamp()
		FROM generate_series(1, $1) g`, historyRows); err != nil {
		t.Fatal(err)
	}

	diagnostics := func(t *testing.T) (application.OperationDiagnostics, error) {
		t.Helper()
		return service.OperationOperatorDiagnostics(ctx, applicationfake.Principal("m20-operator"), "m20-history-op")
	}
	first, err := diagnostics(t)
	if err != nil {
		t.Fatal(err)
	}
	if first.AttemptCount != historyPairs {
		t.Fatalf("attempt count = %d, want %d", first.AttemptCount, historyPairs)
	}
	if first.WorkCount != historyRows+1 { // seeded rows + the admission Drive row
		t.Fatalf("work count = %d, want %d", first.WorkCount, historyRows+1)
	}
	if first.LatestAttempt == nil || first.LatestAttempt.Number != historyRows {
		t.Fatalf("latest attempt = %+v, want number %d", first.LatestAttempt, historyRows)
	}
	if len(first.ActiveWork) > application.WorkActiveLimit {
		t.Fatalf("active work = %d rows, exceeds bound %d", len(first.ActiveWork), application.WorkActiveLimit)
	}
	if first.ActiveWorkTruncated {
		t.Fatal("active truncation flag set without overflow")
	}
	// Execution state Pending is provably pre-submit: nothing observable, no
	// operator action — and this must hold identically under any history size.
	if first.Assessment.State != application.RecoveryNoActionNeeded ||
		len(first.Assessment.AllowedActions) != 0 {
		t.Fatalf("assessment = %+v, want no action", first.Assessment)
	}

	beforeAppend := first.Revision
	// Irrelevant appends: a historical gap-fill attempt below the current
	// maximum and one more Completed work row. Neither can change recovery
	// safety, so the revision must not move even though both counts grow.
	appendix := []string{
		`INSERT INTO provisioning_submission_attempts
			(operation_id,attempt_number,state,dispatch_message_id,resolved_at)
			VALUES ('m20-history-op', 2499, 'Rejected', 'seed-dispatch-2499', clock_timestamp())`,
		`INSERT INTO outbox_messages(id,kind,operation_id,dedupe_key,state,completed_at)
			VALUES ('seed-observe-late','Observe','m20-history-op','seed-observe-late','Completed',clock_timestamp())`,
	}
	for _, statement := range appendix {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	second, err := diagnostics(t)
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision != beforeAppend {
		t.Fatalf("revision churned on irrelevant history append: %q -> %q", beforeAppend, second.Revision)
	}
	if second.Assessment.State != first.Assessment.State || second.LatestAttempt.Number != historyRows {
		t.Fatalf("planner/latest changed under identical current facts: %+v vs %+v", first.Assessment, second.Assessment)
	}
	if second.AttemptCount != historyPairs+1 || second.WorkCount != historyRows+2 {
		t.Fatalf("counts did not track appended history: attempts=%d work=%d", second.AttemptCount, second.WorkCount)
	}

	// Positive control: a NEW highest-numbered attempt is current-relevant
	// evidence and must move the revision.
	if _, err := pool.Exec(ctx, `INSERT INTO provisioning_submission_attempts
		(operation_id,attempt_number,state,dispatch_message_id,resolved_at)
		VALUES ('m20-history-op', 9999, 'Rejected', 'seed-dispatch-9999', clock_timestamp())`); err != nil {
		t.Fatal(err)
	}
	third, err := diagnostics(t)
	if err != nil {
		t.Fatal(err)
	}
	if third.Revision == beforeAppend {
		t.Fatal("revision ignored a new latest attempt")
	}
}
