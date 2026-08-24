// SPDX-License-Identifier: Apache-2.0

package worker_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	applicationfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
	"github.com/sithea-nou/liftr/internal/worker"
)

var testTime = time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

func TestRunOnceDrivesAndDispatchesOperation(t *testing.T) {
	provider := provisioningfake.New(provisioningfake.ModeSynchronous)
	service, store, instance := newHarness(t, provider)
	command := createCommand(t, "resource-worker", "operation-worker")
	result, err := service.CreateResource(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if result.Operation.State() != domain.OperationStatePending || provider.SubmissionCount(command.OperationID) != 0 {
		t.Fatal("admission performed provider work")
	}
	drain(t, instance, 8)
	operation, err := store.GetOperation(context.Background(), command.OperationID)
	if err != nil || operation.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("operation error=%v state=%s", err, operation.Operation.State())
	}
	if provider.SubmissionCount(command.OperationID) != 1 {
		t.Fatalf("submissions=%d, want 1", provider.SubmissionCount(command.OperationID))
	}
}

func TestDispatchCarriesProviderNeutralAttemptCorrelation(t *testing.T) {
	provider := &capturingProvider{}
	service, _, instance := newHarness(t, provider)
	command := createCommand(t, "resource-correlation", "operation-correlation")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	drain(t, instance, 8)
	if provider.request.OperationID != command.OperationID || provider.request.AttemptNumber != 1 || provider.request.Capability != domain.CapabilityCreate {
		t.Fatalf("request = %+v", provider.request)
	}
}

func TestNotFoundCreatesExactlyOneNewAttempt(t *testing.T) {
	provider := &recoveryProvider{}
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = 0
	command := createCommand(t, "resource-recovery", "operation-recovery")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	drain(t, instance, 12)
	provider.mu.Lock()
	submissions := provider.submissions
	provider.mu.Unlock()
	if submissions != 2 {
		t.Fatalf("submissions=%d, want 2", submissions)
	}
	first, err := store.GetSubmissionAttempt(context.Background(), command.OperationID, 1)
	if err != nil || first.State != application.SubmissionAttemptNotFound {
		t.Fatalf("first attempt error=%v state=%s", err, first.State)
	}
	second, err := store.GetSubmissionAttempt(context.Background(), command.OperationID, 2)
	if err != nil || second.State != application.SubmissionAttemptAccepted {
		t.Fatalf("second attempt error=%v state=%s", err, second.State)
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.CurrentAttempt != 2 {
		t.Fatalf("execution error=%v attempt=%d", err, execution.CurrentAttempt)
	}
}

func TestLongDispatchRenewsLease(t *testing.T) {
	provider := newBlockingProvider()
	service, store, instance := newHarness(t, provider)
	instance.Lease = 5 * time.Millisecond
	instance.RetryBase = 0
	command := createCommand(t, "resource-expired", "operation-expired")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := instance.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	result := make(chan error, 1)
	go func() {
		_, err := instance.RunOnce(context.Background())
		result <- err
	}()
	<-provider.started
	time.Sleep(20 * time.Millisecond)
	var message application.OutboxMessage
	err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		var err error
		message, err = tx.Outbox().GetOutbox(context.Background(), application.DispatchMessage(command.OperationID, 1, 0).ID)
		return err
	})
	if err != nil || message.State != application.OutboxLeased || !message.LeasedUntil.After(time.Now()) {
		t.Fatalf("dispatch message error=%v state=%s leasedUntil=%v", err, message.State, message.LeasedUntil)
	}
	if worked, err := instance.RunOnce(context.Background()); err != nil || worked {
		t.Fatalf("renewed dispatch was reclaimed worked=%t error=%v", worked, err)
	}
	close(provider.release)
	if err := <-result; err != nil {
		t.Fatalf("dispatch error=%v", err)
	}
	provider.mu.Lock()
	submissions := provider.submissions
	provider.mu.Unlock()
	if submissions != 1 {
		t.Fatalf("submissions=%d, want no blind resubmission", submissions)
	}
}

func TestExpiredPendingDispatchRequeuesSameMessage(t *testing.T) {
	provider := provisioningfake.New(provisioningfake.ModeSynchronous)
	service, store, instance := newHarness(t, provider)
	instance.Lease = 5 * time.Millisecond
	command := createCommand(t, "resource-pending-crash", "operation-pending-crash")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := instance.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	var claimed application.OutboxMessage
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		var found bool
		var err error
		claimed, found, err = tx.Outbox().ClaimOutbox(context.Background(), "crashed-before-dispatching", instance.Lease)
		if err == nil && (!found || claimed.Kind != application.OutboxDispatch) {
			return errors.New("Dispatch was not claimed")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("pending recovery worked=%t error=%v", worked, err)
	}
	message, err := store.GetOutbox(context.Background(), claimed.ID)
	if err != nil || message.State != application.OutboxPending || message.AttemptNumber != 1 {
		t.Fatalf("message error=%v state=%s attempt=%d", err, message.State, message.AttemptNumber)
	}
	attempt, err := store.GetSubmissionAttempt(context.Background(), command.OperationID, 1)
	if err != nil || attempt.State != application.SubmissionAttemptPending {
		t.Fatalf("attempt error=%v state=%s", err, attempt.State)
	}
	if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("requeued dispatch worked=%t error=%v", worked, err)
	}
	if provider.SubmissionCount(command.OperationID) != 1 {
		t.Fatalf("submissions=%d, want 1", provider.SubmissionCount(command.OperationID))
	}
}

func TestExpiredClaimantCannotTransitionToDispatching(t *testing.T) {
	provider := provisioningfake.New(provisioningfake.ModeSynchronous)
	service, store, instance := newHarness(t, provider)
	command := createCommand(t, "resource-expired-claimant", "operation-expired-claimant")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := instance.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	delayed := &delayedTransactionRunner{inner: store, delayAfter: 2, delay: 15 * time.Millisecond}
	resolver := instance.Resolver
	staleWorker, err := worker.New(delayed, resolver)
	if err != nil {
		t.Fatal(err)
	}
	staleWorker.Lease = 5 * time.Millisecond
	staleWorker.RetryBase = 0
	if worked, err := staleWorker.RunOnce(context.Background()); err == nil || !worked {
		t.Fatalf("expired claimant worked=%t error=%v", worked, err)
	}
	if provider.SubmissionCount(command.OperationID) != 0 {
		t.Fatal("expired claimant invoked Submit")
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.State != application.AttemptPending {
		t.Fatalf("execution error=%v state=%s", err, execution.State)
	}
	instance.Lease = 5 * time.Millisecond
	if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("pending expiry recovery worked=%t error=%v", worked, err)
	}
}

func TestUnknownCorrelationCannotCompleteTerminalExecution(t *testing.T) {
	provider := &unknownTerminalProvider{}
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = time.Hour
	command := createCommand(t, "resource-unknown-terminal", "operation-unknown-terminal")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if _, err := instance.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.State != application.AttemptUnknown || execution.Correlation != provisioning.RequestCorrelationUnknown {
		t.Fatalf("execution error=%v state=%s correlation=%s", err, execution.State, execution.Correlation)
	}
	operation, err := store.GetOperation(context.Background(), command.OperationID)
	if err != nil || operation.Operation.IsTerminal() {
		t.Fatalf("operation error=%v state=%s", err, operation.Operation.State())
	}
}

func TestLeaseRenewalLossAfterDispatchingBecomesUnknown(t *testing.T) {
	provider := newStubbornBlockingProvider()
	service, store, instance := newHarness(t, provider)
	instance.Lease = 10 * time.Millisecond
	instance.RetryBase = 0
	command := createCommand(t, "resource-renewal-loss", "operation-renewal-loss")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := instance.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	result := make(chan error, 1)
	go func() {
		_, err := instance.RunOnce(context.Background())
		result <- err
	}()
	<-provider.started
	locked := make(chan struct{})
	releaseTransaction := make(chan struct{})
	transactionDone := make(chan struct{})
	go func() {
		_ = store.Within(context.Background(), func(application.UnitOfWork) error {
			close(locked)
			<-releaseTransaction
			return nil
		})
		close(transactionDone)
	}()
	<-locked
	time.Sleep(20 * time.Millisecond)
	close(releaseTransaction)
	<-transactionDone
	time.Sleep(2 * time.Millisecond)
	if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("expiry recovery worked=%t error=%v", worked, err)
	}
	close(provider.release)
	if err := <-result; err == nil {
		t.Fatal("stale worker returned a conclusive result")
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.State != application.AttemptUnknown || execution.Correlation != provisioning.RequestCorrelationUnknown {
		t.Fatalf("execution error=%v state=%s correlation=%s", err, execution.State, execution.Correlation)
	}
	attempt, err := store.GetSubmissionAttempt(context.Background(), command.OperationID, 1)
	if err != nil || attempt.State != application.SubmissionAttemptUnknown {
		t.Fatalf("attempt error=%v state=%s", err, attempt.State)
	}
	provider.mu.Lock()
	submissions := provider.submissions
	provider.mu.Unlock()
	if submissions != 1 {
		t.Fatalf("submissions=%d, want no blind resubmission", submissions)
	}
}

func TestPassiveObservationDoesNotCreateSyntheticOperation(t *testing.T) {
	provider := provisioningfake.New(provisioningfake.ModeExisting)
	_, store, instance := newHarness(t, provider)
	ref, _ := application.NewProvisionerRef("test-provider")
	spec, _ := domain.NewResourceSpec(map[string]any{"intent": "existing"})
	resource, _ := domain.NewResource("resource-passive", provisioningfake.ResourceType(), domain.OwnerRef{Kind: "team", ID: "platform"}, spec, testTime)
	status, _ := domain.NewResourceStatus(resource.ID(), 0, domain.ResourceStateUnknown, nil, testTime)
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		return tx.Resources().CreateResource(context.Background(), application.ResourceRecord{Resource: resource, Status: status, ProvisionerRef: ref, Version: 1})
	}); err != nil {
		t.Fatal(err)
	}
	if err := instance.SchedulePassiveObservation(context.Background(), resource.ID(), 1, 1); err != nil {
		t.Fatal(err)
	}
	if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("RunOnce() worked=%t error=%v", worked, err)
	}
	stored, err := store.GetResource(context.Background(), resource.ID())
	if err != nil || stored.Status.State() != domain.ResourceStateReady {
		t.Fatalf("resource error=%v state=%s", err, stored.Status.State())
	}
	if _, found, err := store.ActiveForResource(context.Background(), resource.ID()); err != nil || found {
		t.Fatalf("active operation found=%t error=%v", found, err)
	}
}

func TestContradictoryNotFoundDoesNotCreateAttempt(t *testing.T) {
	provider := &contradictoryProvider{}
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = 0
	command := createCommand(t, "resource-contradictory", "operation-contradictory")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if _, err := instance.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := instance.RunOnce(context.Background()); err == nil {
		t.Fatal("contradictory NotFound observation succeeded")
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.CurrentAttempt != 1 || execution.State != application.AttemptUnknown {
		t.Fatalf("execution error=%v attempt=%d state=%s", err, execution.CurrentAttempt, execution.State)
	}
	if _, err := store.GetSubmissionAttempt(context.Background(), command.OperationID, 2); err == nil {
		t.Fatal("contradictory observation created a second attempt")
	}
}

// TestRegressiveBackendTimestampStillCompletesWithAlignedTimeline pins the
// M10 refinement of mixed-clock handling: positively correlated terminal
// success whose backend timestamp regresses below Liftr's own frontier (for
// example second-granular history times) completes reconciliation immediately.
// The completion instant is lifted onto the persisted frontier, the execution
// evidence dimension moves with it, and the backend is never re-executed.
// Before ADR-0011 this shape was quarantined as a dispatch-result anomaly and
// recovered through a follow-up observation; that detour stranded operations
// whenever follow-up scheduling lost the race against quiescence detection.
func TestRegressiveBackendTimestampStillCompletesWithAlignedTimeline(t *testing.T) {
	provider := &staleTerminalProvider{}
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = 0
	command := createCommand(t, "resource-aligned-completion", "operation-aligned-completion")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	drain(t, instance, 8)

	record, err := store.GetOperation(context.Background(), command.OperationID)
	if err != nil || record.Operation.State() != domain.OperationStateSucceeded {
		failure, _ := record.Operation.Failure()
		t.Fatalf("operation error=%v state=%s failure=%+v", err, record.Operation.State(), failure)
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.State != application.AttemptSucceeded {
		t.Fatalf("execution error=%v state=%s", err, execution.State)
	}
	if !record.Operation.CompletedAt().Equal(execution.LastObservedAt) {
		t.Fatalf("completion %v diverges from evidence %v", record.Operation.CompletedAt(), execution.LastObservedAt)
	}
	view := readView(t, store, "resource-aligned-completion")
	if view.Resource.Status.State() != domain.ResourceStateReady {
		t.Fatalf("state = %s", view.Resource.Status.State())
	}
	if got := execution.Submission.Observation.ObservedAt; !got.Before(view.Resource.Status.UpdatedAt()) && !got.Equal(view.Resource.Status.UpdatedAt()) {
		t.Logf("backend evidence time %v vs aligned completion %v", got, view.Resource.Status.UpdatedAt())
	}
	if provider.submissionCalls != 1 {
		t.Fatalf("alignment triggered %d submissions", provider.submissionCalls)
	}
}

func TestConclusiveNonAcceptanceIsRejectedNotAccepted(t *testing.T) {
	provider := &rejectedProvider{}
	service, store, instance := newHarness(t, provider)
	command := createCommand(t, "resource-rejected", "operation-rejected")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	drain(t, instance, 8)
	attempt, err := store.GetSubmissionAttempt(context.Background(), command.OperationID, 1)
	if err != nil || attempt.State != application.SubmissionAttemptRejected || attempt.Failure == nil || attempt.Failure.Kind != provisioning.FailureInvalidRequest {
		t.Fatalf("attempt error=%v state=%s failure=%v", err, attempt.State, attempt.Failure)
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.AcceptanceConfirmed || execution.State != application.AttemptFailed {
		t.Fatalf("execution error=%v accepted=%t state=%s", err, execution.AcceptanceConfirmed, execution.State)
	}
}

func TestRetryableFailureRetriesWithoutQuarantine(t *testing.T) {
	service, store, instance := newUnresolvableHarness(t)
	instance.RetryBase = 0
	command := createCommand(t, "resource-unresolvable", "operation-unresolvable")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
			t.Fatalf("drive RunOnce worked=%t error=%v", worked, err)
		}
	}
	for range 24 {
		if worked, err := instance.RunOnce(context.Background()); err == nil || !worked {
			t.Fatalf("dispatch RunOnce worked=%t error=%v", worked, err)
		}
	}
	message, err := store.GetOutbox(context.Background(), application.DispatchMessage(command.OperationID, 1, 0).ID)
	if err != nil || message.State != application.OutboxPending {
		t.Fatalf("dispatch message error=%v state=%s", err, message.State)
	}
	if message.AttemptCount < 20 {
		t.Fatalf("attempt count=%d, want retries past any cap", message.AttemptCount)
	}
	if message.LastError == "" {
		t.Fatal("retryable failure did not record a last error")
	}
}

func TestRetryBackoffCapsAtMaximumDelay(t *testing.T) {
	service, store, instance := newUnresolvableHarness(t)
	instance.RetryBase = time.Millisecond
	command := createCommand(t, "resource-backoff-cap", "operation-backoff-cap")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
			t.Fatalf("drive RunOnce worked=%t error=%v", worked, err)
		}
	}
	dispatchID := application.DispatchMessage(command.OperationID, 1, 0).ID
	for range 14 {
		message, err := store.GetOutbox(context.Background(), dispatchID)
		if err != nil {
			t.Fatal(err)
		}
		if wait := time.Until(message.AvailableAt); wait > 0 {
			time.Sleep(wait)
		}
		if worked, err := instance.RunOnce(context.Background()); err == nil || !worked {
			t.Fatalf("dispatch RunOnce worked=%t error=%v", worked, err)
		}
		message, err = store.GetOutbox(context.Background(), dispatchID)
		if err != nil {
			t.Fatal(err)
		}
		if message.AttemptCount >= 10 {
			delay := message.AvailableAt.Sub(time.Now())
			if delay < 500*time.Millisecond || delay > 600*time.Millisecond {
				t.Fatalf("capped backoff delay=%v at attempt %d", delay, message.AttemptCount)
			}
		}
	}
}

func TestPoisonedWorkIsQuarantined(t *testing.T) {
	provider := provisioningfake.New(provisioningfake.ModeSynchronous)
	_, store, instance := newHarness(t, provider)
	message := application.DriveMessage(domain.OperationID("operation-missing"), 1)
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		return tx.Outbox().Enqueue(context.Background(), message)
	}); err != nil {
		t.Fatal(err)
	}
	if worked, err := instance.RunOnce(context.Background()); err == nil || !worked {
		t.Fatalf("RunOnce worked=%t error=%v", worked, err)
	}
	stored, err := store.GetOutbox(context.Background(), message.ID)
	if err != nil || stored.State != application.OutboxDead {
		t.Fatalf("message error=%v state=%s", err, stored.State)
	}
	if stored.TerminalReason == "" {
		t.Fatal("quarantined work lacks a terminal reason")
	}
}

func TestStaleWorkIsSettledNotQuarantined(t *testing.T) {
	provider := provisioningfake.New(provisioningfake.ModeSynchronous)
	_, store, instance := newHarness(t, provider)
	ref, err := application.NewProvisionerRef("test-provider")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := domain.NewResourceSpec(map[string]any{"size": uint64(3)})
	if err != nil {
		t.Fatal(err)
	}
	execution := application.ProvisioningExecutionRecord{
		OperationID:      domain.OperationID("operation-stale"),
		ResourceID:       domain.ResourceID("resource-stale"),
		ProvisionerRef:   ref,
		ResourceType:     provisioningfake.ResourceType(),
		Capability:       domain.CapabilityCreate,
		TargetGeneration: 1,
		Spec:             spec,
		State:            application.AttemptPending,
		CurrentAttempt:   1,
		NextObservation:  1,
		Version:          1,
	}
	message := application.ObserveMessage(execution.OperationID, 1, 99)
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		if err := tx.Executions().CreateExecution(context.Background(), execution); err != nil {
			return err
		}
		return tx.Outbox().Enqueue(context.Background(), message)
	}); err != nil {
		t.Fatal(err)
	}
	if worked, err := instance.RunOnce(context.Background()); err == nil || !worked {
		t.Fatalf("RunOnce worked=%t error=%v", worked, err)
	}
	stored, err := store.GetOutbox(context.Background(), message.ID)
	if err != nil || stored.State != application.OutboxCompleted {
		t.Fatalf("message error=%v state=%s", err, stored.State)
	}
	if stored.TerminalReason != "StaleWork" {
		t.Fatalf("terminal reason=%q, want StaleWork", stored.TerminalReason)
	}
}

func TestRetryablePersistenceErrorReschedulesOutbox(t *testing.T) {
	store := applicationfake.NewStore()
	message := application.DriveMessage("operation-retryable-persistence", 1)
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		return tx.Outbox().Enqueue(context.Background(), message)
	}); err != nil {
		t.Fatal(err)
	}
	runner := &failTransactionRunner{inner: store, failAt: 3, err: application.ErrRetryablePersistence}
	instance, err := worker.New(runner, &applicationfake.Resolver{})
	if err != nil {
		t.Fatal(err)
	}
	instance.RetryBase = 0
	worked, err := instance.RunOnce(context.Background())
	if !worked || !errors.Is(err, application.ErrRetryablePersistence) {
		t.Fatalf("RunOnce worked=%t error=%v", worked, err)
	}
	stored, err := store.GetOutbox(context.Background(), message.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != application.OutboxPending || stored.TerminalReason != "" || stored.LastError == "" {
		t.Fatalf("retryable persistence outbox state=%s terminal=%q lastError=%q", stored.State, stored.TerminalReason, stored.LastError)
	}
}

func TestAmbiguousDispatchKeepsLeaseForExpiryRecovery(t *testing.T) {
	provider := newBlockingProvider()
	service, store, instance := newHarness(t, provider)
	instance.Lease = 5 * time.Millisecond
	instance.RetryBase = 0
	command := createCommand(t, "resource-ambiguous-lease", "operation-ambiguous-lease")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := instance.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	result := make(chan error, 1)
	go func() {
		_, err := instance.RunOnce(context.Background())
		result <- err
	}()
	<-provider.started
	locked := make(chan struct{})
	releaseTransaction := make(chan struct{})
	transactionDone := make(chan struct{})
	go func() {
		_ = store.Within(context.Background(), func(application.UnitOfWork) error {
			close(locked)
			<-releaseTransaction
			return nil
		})
		close(transactionDone)
	}()
	<-locked
	time.Sleep(20 * time.Millisecond)
	close(releaseTransaction)
	<-transactionDone
	var message application.OutboxMessage
	var err error
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		var err error
		message, err = tx.Outbox().GetOutbox(context.Background(), application.DispatchMessage(command.OperationID, 1, 0).ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if message.State != application.OutboxLeased {
		t.Fatalf("dispatch state=%s, want leased, not retried or quarantined", message.State)
	}
	if message.AttemptCount != 1 {
		t.Fatalf("dispatch attempt count=%d, want 1", message.AttemptCount)
	}
	close(provider.release)
	if err := <-result; err == nil {
		t.Fatal("ambiguous dispatch returned a conclusive result")
	}
	if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("expiry recovery worked=%t error=%v", worked, err)
	}
	message, err = store.GetOutbox(context.Background(), application.DispatchMessage(command.OperationID, 1, 0).ID)
	if err != nil || message.State != application.OutboxCompleted {
		t.Fatalf("dispatch message error=%v state=%s, want completed after recovery", err, message.State)
	}
	attempt, err := store.GetSubmissionAttempt(context.Background(), command.OperationID, 1)
	if err != nil || attempt.State != application.SubmissionAttemptUnknown {
		t.Fatalf("attempt error=%v state=%s", err, attempt.State)
	}
}

func TestStaleObservationContinuesObservingWithoutTransition(t *testing.T) {
	provider := &nonAdvancingProvider{}
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = 0
	command := createCommand(t, "resource-stale-observation", "operation-stale-observation")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := instance.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("dispatch RunOnce worked=%t error=%v", worked, err)
	}
	if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("stale observe RunOnce worked=%t error=%v", worked, err)
	}
	message, err := store.GetOutbox(context.Background(), application.ObserveMessage(command.OperationID, 1, 0).ID)
	if err != nil || message.State != application.OutboxCompleted || message.TerminalReason != "StaleObservation" {
		t.Fatalf("observe message error=%v state=%s reason=%s", err, message.State, message.TerminalReason)
	}
	followUp, err := store.GetOutbox(context.Background(), application.ObserveMessage(command.OperationID, 2, 0).ID)
	if err != nil || followUp.State != application.OutboxPending {
		t.Fatalf("follow-up observe error=%v state=%s, want a pending follow-up", err, followUp.State)
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.State != application.AttemptAccepted {
		t.Fatalf("execution error=%v state=%s", err, execution.State)
	}
	if execution.NextObservation != 3 {
		t.Fatalf("execution NextObservation=%d, want 3", execution.NextObservation)
	}
	if !execution.LastObservedAt.Equal(testTime) {
		t.Fatalf("execution LastObservedAt=%v, want %v", execution.LastObservedAt, testTime)
	}
	operation, err := store.GetOperation(context.Background(), command.OperationID)
	if err != nil || operation.Operation.IsTerminal() {
		t.Fatalf("operation error=%v terminal=%t", err, operation.Operation.IsTerminal())
	}
}

func TestAdvancingObservationIsApplied(t *testing.T) {
	provider := &advancingProvider{}
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = 0
	command := createCommand(t, "resource-advancing", "operation-advancing")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := instance.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("dispatch RunOnce worked=%t error=%v", worked, err)
	}
	if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("observe RunOnce worked=%t error=%v", worked, err)
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.State != application.AttemptAccepted {
		t.Fatalf("execution error=%v state=%s", err, execution.State)
	}
	if execution.LastObservation == nil || !execution.LastObservation.ObservedAt.Equal(testTime.Add(time.Hour)) {
		t.Fatalf("execution observation=%v", execution.LastObservation)
	}
	message, err := store.GetOutbox(context.Background(), application.ObserveMessage(command.OperationID, 2, 0).ID)
	if err != nil || message.State != application.OutboxPending {
		t.Fatalf("follow-up observe error=%v state=%s", err, message.State)
	}
	if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("advancing observe RunOnce worked=%t error=%v", worked, err)
	}
	execution, err = store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.LastObservation == nil || !execution.LastObservation.ObservedAt.Equal(testTime.Add(2*time.Hour)) {
		t.Fatalf("execution error=%v observation=%v", err, execution.LastObservation)
	}
}

func TestContradictoryObservationRetriesNotSettled(t *testing.T) {
	provider := &contradictoryStaleProvider{}
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = 0
	command := createCommand(t, "resource-contradictory-stale", "operation-contradictory-stale")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := instance.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("dispatch RunOnce worked=%t error=%v", worked, err)
	}
	if worked, err := instance.RunOnce(context.Background()); err == nil || !worked {
		t.Fatalf("contradictory observe RunOnce worked=%t error=%v", worked, err)
	}
	message, err := store.GetOutbox(context.Background(), application.ObserveMessage(command.OperationID, 1, 0).ID)
	if err != nil || message.State == application.OutboxCompleted || message.State == application.OutboxDead {
		t.Fatalf("observe message error=%v state=%s", err, message.State)
	}
	if _, err := store.GetSubmissionAttempt(context.Background(), command.OperationID, 2); err == nil {
		t.Fatal("contradictory observation created a second attempt")
	}
}

func TestPassiveObservationStaleTimestampSettles(t *testing.T) {
	provider := &stalePassiveProvider{}
	_, store, instance := newHarness(t, provider)
	ref, _ := application.NewProvisionerRef("test-provider")
	spec, _ := domain.NewResourceSpec(map[string]any{"intent": "existing"})
	resource, _ := domain.NewResource("resource-passive-stale", provisioningfake.ResourceType(), domain.OwnerRef{Kind: "team", ID: "platform"}, spec, testTime)
	status, _ := domain.NewResourceStatus(resource.ID(), 0, domain.ResourceStateUnknown, nil, testTime)
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		return tx.Resources().CreateResource(context.Background(), application.ResourceRecord{Resource: resource, Status: status, ProvisionerRef: ref, Version: 1})
	}); err != nil {
		t.Fatal(err)
	}
	if err := instance.SchedulePassiveObservation(context.Background(), resource.ID(), 1, 1); err != nil {
		t.Fatal(err)
	}
	if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("RunOnce worked=%t error=%v", worked, err)
	}
	message, err := store.GetOutbox(context.Background(), application.PassiveObserveMessage(resource.ID(), 1, 1).ID)
	if err != nil || message.State != application.OutboxCompleted || message.TerminalReason != "StaleObservation" {
		t.Fatalf("message error=%v state=%s reason=%s", err, message.State, message.TerminalReason)
	}
	stored, err := store.GetResource(context.Background(), resource.ID())
	if err != nil || stored.Status.State() != domain.ResourceStateUnknown {
		t.Fatalf("resource error=%v state=%s", err, stored.Status.State())
	}
}

func TestPassiveObservationDuringActiveOperationSettles(t *testing.T) {
	provider := provisioningfake.New(provisioningfake.ModeExisting)
	_, store, instance := newHarness(t, provider)
	ref, _ := application.NewProvisionerRef("test-provider")
	spec, _ := domain.NewResourceSpec(map[string]any{"intent": "existing"})
	resource, _ := domain.NewResource("resource-passive-active", provisioningfake.ResourceType(), domain.OwnerRef{Kind: "team", ID: "platform"}, spec, testTime)
	status, _ := domain.NewResourceStatus(resource.ID(), 0, domain.ResourceStateUnknown, nil, testTime)
	operation, err := domain.RestoreOperation(domain.OperationSnapshot{
		ID: "operation-passive-active", ResourceID: resource.ID(), Capability: domain.CapabilityCreate, TargetGeneration: 1,
		RequestedAt: testTime, State: domain.OperationStateRunning, Phase: domain.OperationPhaseApplying,
		StartedAt: testTime, PhaseChangedAt: testTime,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		if err := tx.Resources().CreateResource(context.Background(), application.ResourceRecord{Resource: resource, Status: status, ProvisionerRef: ref, Version: 1}); err != nil {
			return err
		}
		return tx.Operations().CreateOperation(context.Background(), application.OperationRecord{Operation: operation, Version: 1})
	}); err != nil {
		t.Fatal(err)
	}
	if err := instance.SchedulePassiveObservation(context.Background(), resource.ID(), 1, 1); err != nil {
		t.Fatal(err)
	}
	if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("RunOnce worked=%t error=%v", worked, err)
	}
	message, err := store.GetOutbox(context.Background(), application.PassiveObserveMessage(resource.ID(), 1, 1).ID)
	if err != nil || message.State != application.OutboxCompleted || message.TerminalReason != "StaleObservation" {
		t.Fatalf("message error=%v state=%s reason=%s", err, message.State, message.TerminalReason)
	}
}

func newUnresolvableHarness(t *testing.T) (*application.Service, *applicationfake.Store, *worker.Worker) {
	t.Helper()
	ref, err := application.NewProvisionerRef("test-provider")
	if err != nil {
		t.Fatal(err)
	}
	store := applicationfake.NewStore()
	selector := &applicationfake.Selector{Ref: ref}
	resolver := &applicationfake.Resolver{Providers: map[application.ProvisionerRef]provisioning.Provisioner{}}
	typeValue, err := domain.NewResourceType(provisioningfake.ResourceType(), "worker test resource", []domain.Capability{
		domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewService(applicationfake.Catalog{Types: map[domain.ResourceTypeRef]domain.ResourceType{provisioningfake.ResourceType(): typeValue}}, selector, resolver, store, applicationfake.AllowAll{})
	if err != nil {
		t.Fatal(err)
	}
	instance, err := worker.New(store, resolver)
	if err != nil {
		t.Fatal(err)
	}
	instance.Clock = func() time.Time { return testTime.Add(time.Minute) }
	return service, store, instance
}

func newHarness(t *testing.T, provider provisioning.Provisioner) (*application.Service, *applicationfake.Store, *worker.Worker) {
	t.Helper()
	ref, err := application.NewProvisionerRef("test-provider")
	if err != nil {
		t.Fatal(err)
	}
	store := applicationfake.NewStore()
	selector := &applicationfake.Selector{Ref: ref}
	resolver := &applicationfake.Resolver{Providers: map[application.ProvisionerRef]provisioning.Provisioner{ref: provider}}
	typeValue, err := domain.NewResourceType(provisioningfake.ResourceType(), "worker test resource", []domain.Capability{
		domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewService(applicationfake.Catalog{Types: map[domain.ResourceTypeRef]domain.ResourceType{provisioningfake.ResourceType(): typeValue}}, selector, resolver, store, applicationfake.AllowAll{})
	if err != nil {
		t.Fatal(err)
	}
	instance, err := worker.New(store, resolver)
	if err != nil {
		t.Fatal(err)
	}
	instance.Clock = func() time.Time { return testTime.Add(time.Minute) }
	return service, store, instance
}

func createCommand(t *testing.T, resourceID domain.ResourceID, operationID domain.OperationID) application.CreateResourceCommand {
	t.Helper()
	spec, err := domain.NewResourceSpec(map[string]any{"size": uint64(3)})
	if err != nil {
		t.Fatal(err)
	}
	return application.CreateResourceCommand{Actor: applicationfake.Principal("tester"), ID: resourceID, Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: spec, OperationID: operationID, EventID: domain.EventID("event-" + string(operationID)), RequestedAt: testTime,
		IdempotencyKey: "key-" + string(operationID)}
}

func drain(t *testing.T, instance *worker.Worker, limit int) {
	t.Helper()
	for range limit {
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

type recoveryProvider struct {
	mu          sync.Mutex
	submissions int
}

type capturingProvider struct{ request provisioning.ExecutionRequest }

func (*capturingProvider) Capabilities() []provisioning.ProvisionerCapability { return nil }
func (p *capturingProvider) Submit(_ context.Context, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	p.request = request
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded}, Resource: readyFacts()}}, nil
}

type unknownTerminalProvider struct{}

func (*unknownTerminalProvider) Capabilities() []provisioning.ProvisionerCapability { return nil }
func (*unknownTerminalProvider) Submit(context.Context, provisioning.ExecutionRequest) (provisioning.Submission, error) {
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationUnknown,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded}, Resource: readyFacts()}}, nil
}
func (*unknownTerminalProvider) Observe(context.Context, provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationUnknown,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded}, Resource: readyFacts()}, nil
}

type delayedTransactionRunner struct {
	inner      application.TransactionRunner
	mu         sync.Mutex
	calls      int
	delayAfter int
	delay      time.Duration
}

type failTransactionRunner struct {
	inner  application.TransactionRunner
	calls  int
	failAt int
	err    error
}

func (r *failTransactionRunner) Within(ctx context.Context, fn func(application.UnitOfWork) error) error {
	r.calls++
	if r.calls == r.failAt {
		return r.err
	}
	return r.inner.Within(ctx, fn)
}

func (r *delayedTransactionRunner) Within(ctx context.Context, fn func(application.UnitOfWork) error) error {
	err := r.inner.Within(ctx, fn)
	r.mu.Lock()
	r.calls++
	shouldDelay := r.calls == r.delayAfter
	r.mu.Unlock()
	if shouldDelay {
		time.Sleep(r.delay)
	}
	return err
}
func (*capturingProvider) Observe(context.Context, provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return provisioning.ExecutionObservation{}, errors.New("unexpected observation")
}

func (p *recoveryProvider) Capabilities() []provisioning.ProvisionerCapability { return nil }

func (p *recoveryProvider) Submit(context.Context, provisioning.ExecutionRequest) (provisioning.Submission, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.submissions++
	if p.submissions == 1 {
		return provisioning.Submission{}, provisioning.ErrAmbiguousSubmission
	}
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded}, Resource: readyFacts()}}, nil
}

func (p *recoveryProvider) Observe(context.Context, provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationNotFound,
		Resource: provisioning.ResourceObservation{Presence: provisioning.ResourcePresenceUnknown, Readiness: provisioning.ResourceReadinessUnknown, Drift: provisioning.ResourceDriftUnknown}}, nil
}

type blockingProvider struct {
	mu          sync.Mutex
	submissions int
	started     chan struct{}
	release     chan struct{}
	stubborn    bool
}

type contradictoryProvider struct{}

func (*contradictoryProvider) Capabilities() []provisioning.ProvisionerCapability { return nil }
func (*contradictoryProvider) Submit(context.Context, provisioning.ExecutionRequest) (provisioning.Submission, error) {
	return provisioning.Submission{}, provisioning.ErrAmbiguousSubmission
}
func (*contradictoryProvider) Observe(context.Context, provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationNotFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateRunning}}, nil
}

type staleTerminalProvider struct{ submissionCalls int }

func (*staleTerminalProvider) Capabilities() []provisioning.ProvisionerCapability { return nil }
func (p *staleTerminalProvider) Submit(_ context.Context, _ provisioning.ExecutionRequest) (provisioning.Submission, error) {
	p.submissionCalls++
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded}, Resource: readyFacts(), ObservedAt: testTime.Add(-time.Hour)}}, nil
}

type rejectedProvider struct{}

func (*rejectedProvider) Capabilities() []provisioning.ProvisionerCapability { return nil }
func (*rejectedProvider) Submit(context.Context, provisioning.ExecutionRequest) (provisioning.Submission, error) {
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationNotFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateFailed, Failure: &provisioning.ExecutionFailure{
			Kind: provisioning.FailureInvalidRequest, Reason: "Invalid", Message: "request rejected",
		}},
	}}, nil
}
func (*rejectedProvider) Observe(context.Context, provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return provisioning.ExecutionObservation{}, errors.New("unexpected observation")
}
func (*staleTerminalProvider) Observe(context.Context, provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded}, Resource: readyFacts()}, nil
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{started: make(chan struct{}), release: make(chan struct{})}
}

func newStubbornBlockingProvider() *blockingProvider {
	return &blockingProvider{started: make(chan struct{}), release: make(chan struct{}), stubborn: true}
}

func (p *blockingProvider) Capabilities() []provisioning.ProvisionerCapability { return nil }

func (p *blockingProvider) Submit(ctx context.Context, _ provisioning.ExecutionRequest) (provisioning.Submission, error) {
	p.mu.Lock()
	p.submissions++
	p.mu.Unlock()
	close(p.started)
	if p.stubborn {
		<-p.release
		return provisioning.Submission{Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
			Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded}, Resource: readyFacts()}}, nil
	}
	select {
	case <-p.release:
		return provisioning.Submission{Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
			Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded}, Resource: readyFacts()}}, nil
	case <-ctx.Done():
		return provisioning.Submission{}, ctx.Err()
	}
}

func (p *blockingProvider) Observe(context.Context, provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateRunning}}, nil
}

func readyFacts() provisioning.ResourceObservation {
	return provisioning.ResourceObservation{Presence: provisioning.ResourcePresencePresent, Readiness: provisioning.ResourceReadinessReady, Drift: provisioning.ResourceDriftInSync}
}

type nonAdvancingProvider struct{}

func (*nonAdvancingProvider) Capabilities() []provisioning.ProvisionerCapability { return nil }
func (*nonAdvancingProvider) Submit(context.Context, provisioning.ExecutionRequest) (provisioning.Submission, error) {
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateRunning},
		Resource:  runningFacts(), ObservedAt: testTime}}, nil
}
func (*nonAdvancingProvider) Observe(context.Context, provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateRunning},
		Resource:  runningFacts(), ObservedAt: testTime}, nil
}

type advancingProvider struct {
	mu sync.Mutex
	n  int
}

func (*advancingProvider) Capabilities() []provisioning.ProvisionerCapability { return nil }
func (*advancingProvider) Submit(context.Context, provisioning.ExecutionRequest) (provisioning.Submission, error) {
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateRunning},
		Resource:  runningFacts(), ObservedAt: testTime}}, nil
}
func (p *advancingProvider) Observe(context.Context, provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	p.mu.Lock()
	p.n++
	observed := testTime.Add(time.Duration(p.n) * time.Hour)
	p.mu.Unlock()
	return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateRunning},
		Resource:  runningFacts(), ObservedAt: observed}, nil
}

type contradictoryStaleProvider struct{}

func (*contradictoryStaleProvider) Capabilities() []provisioning.ProvisionerCapability { return nil }
func (*contradictoryStaleProvider) Submit(context.Context, provisioning.ExecutionRequest) (provisioning.Submission, error) {
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateRunning},
		Resource:  runningFacts(), ObservedAt: testTime}}, nil
}
func (*contradictoryStaleProvider) Observe(context.Context, provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationNotFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateRunning},
		Resource:  runningFacts(), ObservedAt: testTime}, nil
}

type stalePassiveProvider struct{}

func (*stalePassiveProvider) Capabilities() []provisioning.ProvisionerCapability { return nil }
func (*stalePassiveProvider) Submit(context.Context, provisioning.ExecutionRequest) (provisioning.Submission, error) {
	return provisioning.Submission{}, errors.New("unexpected submit")
}
func (*stalePassiveProvider) Observe(context.Context, provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded},
		Resource:  readyFacts(), ObservedAt: testTime.Add(-time.Hour)}, nil
}

func runningFacts() provisioning.ResourceObservation {
	return provisioning.ResourceObservation{Presence: provisioning.ResourcePresencePresent, Readiness: provisioning.ResourceReadinessNotReady, Drift: provisioning.ResourceDriftInSync}
}
