// SPDX-License-Identifier: Apache-2.0

// Package worker executes durable provisioner-neutral outbox work.
package worker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/lifecycle"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

type Worker struct {
	Transactions application.TransactionRunner
	Resolver     application.ProvisionerResolver
	Types        application.ResourceTypeCatalog
	Lifecycle    lifecycle.Engine
	Lease        time.Duration
	RetryBase    time.Duration
	Clock        func() time.Time
	// Telemetry optionally receives bounded work events. It is injected by
	// composition; the worker never imports a telemetry library and telemetry
	// can never influence durable outcomes.
	Telemetry TelemetrySink

	// pendingTerminal buffers one terminal transition until its transaction
	// commits (single-slot; the loop drains items sequentially per process).
	pendingTerminal *operationSnapshot
}

func New(transactions application.TransactionRunner, resolver application.ProvisionerResolver) (*Worker, error) {
	return NewWithCatalog(transactions, resolver, nil)
}

// NewWithCatalog composes a worker with access to the developer-contract
// catalog. The catalog validates provider output candidates before any value
// can be published; without it only types without output contracts complete.
func NewWithCatalog(transactions application.TransactionRunner, resolver application.ProvisionerResolver, types application.ResourceTypeCatalog) (*Worker, error) {
	if transactions == nil || resolver == nil {
		return nil, fmt.Errorf("worker dependencies are required")
	}
	return &Worker{Transactions: transactions, Resolver: resolver, Types: types, Lease: time.Minute, RetryBase: time.Second, Clock: time.Now}, nil
}

// RunOnce recovers one ambiguous expired Dispatch or processes one claimable
// message. The boolean reports whether durable work was found.
//
// A panic inside one work item is recovered at this per-work boundary: the
// item's lease stays intact so expiry recovery routes it through the existing
// Unknown -> Observe machinery, the panic is reported to telemetry, and the
// loop continues. A panic never marks work successful or failed (ADR-0018).
func (w *Worker) RunOnce(ctx context.Context) (found bool, err error) {
	w.clearPendingTerminal()
	// activeKind tracks the in-progress work kind so a panic inside a handler
	// reports the true kind instead of whatever the outer variable last held.
	activeKind := ""
	operationID := ""
	func() {
		defer func() {
			recovered := recover()
			if recovered == nil {
				return
			}
			err = &PanicError{Value: sanitizePanicValue(recovered)}
			if w.Telemetry != nil {
				kind := activeKind
				if kind == "" {
					kind = "unknown"
				}
				w.Telemetry.WorkerPanic(kind, err.(*PanicError).Value)
				w.Telemetry.WorkCompleted(WorkEvent{Kind: kind, Outcome: OutcomePanic})
			}
		}()
		found, operationID, err = w.runOnce(ctx, &activeKind)
	}()
	if err != nil && errors.Is(err, ErrRecoveredPanic) {
		return true, err
	}
	w.reportWork(activeKind, operationID, found, err)
	if found && err == nil {
		w.flushTerminal()
	}
	return found, err
}

func (w *Worker) reportWork(kind, operationID string, found bool, err error) {
	if w.Telemetry == nil || !found || errors.Is(err, ErrRecoveredPanic) {
		return
	}
	event := WorkEvent{Kind: kind, OperationID: operationID}
	switch {
	case err == nil:
		event.Outcome = OutcomeSuccess
	case isLeaseOwnershipLost(err):
		event.Outcome = OutcomeLeaseLos
		event.ErrorClass = "lease_lost"
	case isAmbiguousDispatch(err):
		// Ambiguous and lease-lost are different diagnoses; the error itself
		// carries which one occurred.
		var ambiguous ambiguousDispatchError
		errors.As(err, &ambiguous)
		if ambiguous.leaseLost {
			event.Outcome = OutcomeLeaseLos
			event.ErrorClass = "lease_lost"
		} else {
			event.Outcome = OutcomeAmbiguous
			event.ErrorClass = "provisioner_submission_ambiguous"
		}
	case classifyFailure(err) == failureStale:
		event.Outcome = OutcomeStale
		event.ErrorClass = "stale"
	case classifyFailure(err) == failurePoison:
		event.Outcome = OutcomeFailed
		event.ErrorClass = "invalid_work"
	default:
		event.Outcome = OutcomeRetry
		event.ErrorClass = "retryable"
	}
	w.Telemetry.WorkCompleted(event)
}

func isLeaseOwnershipLost(err error) bool {
	var lost leaseOwnershipLostError
	return errors.As(err, &lost)
}

func (w *Worker) runOnce(ctx context.Context, kindPtr *string) (found bool, operationID string, err error) {
	*kindPtr = WorkKindExpiredRecovery
	if recovered, recoveryErr := w.recoverExpiredDispatch(ctx); recoveryErr != nil || recovered {
		return recovered, "", recoveryErr
	}
	token, tokenErr := newToken()
	if tokenErr != nil {
		return false, "", tokenErr
	}
	var message application.OutboxMessage
	found = false
	if claimErr := w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
		var err error
		message, found, err = tx.Outbox().ClaimOutbox(ctx, token, w.Lease)
		return err
	}); claimErr != nil || !found {
		return found, string(message.OperationID), claimErr
	}
	kind := workKindOf(message.Kind)
	*kindPtr = kind
	operationID = string(message.OperationID)

	switch message.Kind {
	case application.OutboxDrive:
		err = w.drive(ctx, message)
	case application.OutboxDispatch:
		err = w.dispatch(ctx, message)
	case application.OutboxObserve:
		err = w.observe(ctx, message)
	case application.OutboxPassiveObserve:
		err = w.passiveObserve(ctx, message)
	default:
		err = fmt.Errorf("unsupported outbox kind %q", message.Kind)
	}
	if err == nil {
		return true, operationID, nil
	}
	var ambiguous ambiguousDispatchError
	if errors.As(err, &ambiguous) {
		// Submit may already have reached the provider. Keep the lease intact so
		// expiry recovery moves the attempt through Unknown and Observe.
		return true, operationID, err
	}
	var requeued dispatchRequeuedError
	if errors.As(err, &requeued) {
		// The dispatch handler already restored and rescheduled this exact
		// attempt atomically. Generic outbox retry would lose its refreshed
		// execution-version fence.
		return true, operationID, err
	}
	var leaseLost leaseOwnershipLostError
	if errors.As(err, &leaseLost) {
		// Fenced ownership is gone. Do not attempt completion, quarantine, or
		// retry writes with a stale token.
		return true, operationID, err
	}
	switch classifyFailure(err) {
	case failureStale:
		// The work this message represents has already moved on, or another
		// claimant owns it. Settle it instead of retrying it.
		if settleErr := w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
			return tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "StaleWork")
		}); settleErr != nil && !errors.Is(settleErr, application.ErrConcurrencyConflict) {
			return true, operationID, fmt.Errorf("process work: %w; settle stale work: %v", err, settleErr)
		}
		return true, operationID, err
	case failurePoison:
		// The work is provably invalid and can never succeed by retrying.
		// Quarantine it for administrative redrive instead of retrying it
		// until it strands an active operation.
		if deadErr := w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
			return tx.Outbox().DeadOutbox(ctx, message.ID, message.LeaseToken, err.Error())
		}); deadErr != nil && !errors.Is(deadErr, application.ErrConcurrencyConflict) {
			return true, operationID, fmt.Errorf("process work: %w; quarantine work: %v", err, deadErr)
		}
		return true, operationID, err
	default:
		retryErr := w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
			return tx.Outbox().RetryOutbox(ctx, message.ID, message.LeaseToken, w.backoff(message.AttemptCount), err.Error())
		})
		if retryErr != nil && !errors.Is(retryErr, application.ErrConcurrencyConflict) {
			return true, operationID, fmt.Errorf("process work: %w; reschedule: %v", err, retryErr)
		}
		return true, operationID, err
	}
}

func isAmbiguousDispatch(err error) bool {
	var ambiguous ambiguousDispatchError
	return errors.As(err, &ambiguous)
}

func workKindOf(kind application.OutboxKind) string {
	switch kind {
	case application.OutboxDrive:
		return WorkKindDrive
	case application.OutboxDispatch:
		return WorkKindDispatch
	case application.OutboxObserve:
		return WorkKindObserve
	case application.OutboxPassiveObserve:
		return WorkKindPassiveObserve
	default:
		return "unknown"
	}
}

type failureClass int

const (
	failureRetryable failureClass = iota
	failureStale
	failurePoison
)

// classifyFailure maps a worker processing error to its outbox disposition.
// Errors from ErrConcurrencyConflict mean the loaded work is obsolete or owned
// by someone else. Domain-invalid errors are deliberate quarantines. Everything
// else (provider transport, resolver availability, malformed provider
// observations) is transient and is retried with bounded backoff.
func classifyFailure(err error) failureClass {
	switch {
	case errors.Is(err, application.ErrConcurrencyConflict):
		return failureStale
	case errors.Is(err, application.ErrInvalidApplicationCall),
		errors.Is(err, application.ErrResourceNotFound),
		errors.Is(err, application.ErrOperationNotFound),
		errors.Is(err, lifecycle.ErrInvalidTransition):
		return failurePoison
	default:
		return failureRetryable
	}
}

func (w *Worker) SchedulePassiveObservation(ctx context.Context, resourceID domain.ResourceID, sequence, expectedVersion uint64) error {
	return w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
		return tx.Outbox().Enqueue(ctx, application.PassiveObserveMessage(resourceID, sequence, expectedVersion))
	})
}

func (w *Worker) drive(ctx context.Context, message application.OutboxMessage) error {
	return w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
		current, err := tx.Outbox().GetOutbox(ctx, message.ID)
		if err != nil {
			return err
		}
		if current.State != application.OutboxLeased || current.LeaseToken != message.LeaseToken {
			return application.ErrConcurrencyConflict
		}
		preflight, err := tx.Operations().LookupOperation(ctx, message.OperationID)
		if err != nil {
			return err
		}
		resource, err := tx.Resources().GetResource(ctx, preflight.Operation.ResourceID())
		if err != nil {
			return err
		}
		operation, err := tx.Operations().GetOperation(ctx, message.OperationID)
		if err != nil {
			return err
		}
		if operation.Version != message.ExpectedVersion || operation.Operation.IsTerminal() {
			return tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "StaleDrive")
		}
		next, ok := nextPhase(operation.Operation)
		if !ok {
			return tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "AlreadyDispatchable")
		}
		changedAt := operation.Operation.PhaseChangedAt().Add(time.Nanosecond)
		transition, err := w.Lifecycle.Advance(resource.Resource, resource.Status, operation.Operation, next, application.InternalEventID(operation.Operation.ID(), application.InternalTransitionLabel(next)), changedAt)
		if err != nil {
			return err
		}
		resource.Status = transition.Status
		if err := tx.Resources().SaveResource(ctx, resource, resource.Version); err != nil {
			return err
		}
		if err := tx.Operations().SaveOperation(ctx, application.OperationRecord{Operation: transition.Operation}, operation.Version); err != nil {
			return err
		}
		if err := tx.Events().Append(ctx, transition.Event); err != nil {
			return err
		}
		if err := tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "Driven"); err != nil {
			return err
		}
		if next == domain.OperationPhaseApplying || next == domain.OperationPhaseDestroying {
			execution, err := tx.Executions().GetExecution(ctx, message.OperationID)
			if err != nil {
				return err
			}
			if execution.CurrentAttempt != 0 {
				return application.ErrConcurrencyConflict
			}
			if execution.IsOutputRecovery() {
				if next != domain.OperationPhaseApplying || execution.Capability == domain.CapabilityDelete ||
					execution.State != application.AttemptSucceeded || execution.OutputResolution != application.OutputResolutionPending {
					return fmt.Errorf("%w: invalid output recovery execution", application.ErrInvalidApplicationCall)
				}
				observe := scheduleObserve(&execution, 0)
				if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
					return err
				}
				return tx.Outbox().Enqueue(ctx, observe)
			}
			execution.CurrentAttempt = 1
			dispatch := application.DispatchMessage(message.OperationID, 1, execution.Version+1)
			if err := tx.SubmissionAttempts().CreateSubmissionAttempt(ctx, application.SubmissionAttemptRecord{OperationID: message.OperationID, AttemptNumber: 1, State: application.SubmissionAttemptPending, DispatchMessage: dispatch.ID}); err != nil {
				return err
			}
			if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
				return err
			}
			return tx.Outbox().Enqueue(ctx, dispatch)
		}
		return tx.Outbox().Enqueue(ctx, application.DriveMessage(message.OperationID, operation.Version+1))
	})
}

func nextPhase(operation domain.Operation) (domain.OperationPhase, bool) {
	switch operation.Phase() {
	case domain.OperationPhaseRequested:
		return domain.OperationPhaseValidating, true
	case domain.OperationPhaseValidating:
		if operation.Capability() == domain.CapabilityDelete {
			return domain.OperationPhaseDestroying, true
		}
		return domain.OperationPhasePlanning, true
	case domain.OperationPhasePlanning:
		return domain.OperationPhaseApplying, true
	default:
		return "", false
	}
}

type dispatchContext struct {
	execution application.ProvisioningExecutionRecord
	version   uint64
	provider  provisioning.Provisioner
}

type observationRecords struct {
	execution application.ProvisioningExecutionRecord
	source    application.ProvisioningExecutionRecord
	operation application.OperationRecord
}

// lockObservationRecords uses unlocked execution metadata only to discover
// IDs, then acquires every mutable row in the same order as retry replay.
func lockObservationRecords(ctx context.Context, tx application.UnitOfWork, operationID domain.OperationID, expectedVersion, expectedNextObservation uint64) (observationRecords, error) {
	preflight, err := tx.Executions().LookupExecution(ctx, operationID)
	if err != nil {
		return observationRecords{}, err
	}
	if preflight.Version != expectedVersion || preflight.NextObservation != expectedNextObservation {
		return observationRecords{}, application.ErrConcurrencyConflict
	}
	resource, err := tx.Resources().GetResource(ctx, preflight.ResourceID)
	if err != nil {
		return observationRecords{}, err
	}

	var sourceOperation application.OperationRecord
	if preflight.IsOutputRecovery() {
		sourceOperation, err = tx.Operations().GetOperation(ctx, preflight.RecoverySourceOperationID)
		if err != nil {
			return observationRecords{}, err
		}
	}
	operation, err := tx.Operations().GetOperation(ctx, operationID)
	if err != nil {
		return observationRecords{}, err
	}

	var source application.ProvisioningExecutionRecord
	if preflight.IsOutputRecovery() {
		source, err = tx.Executions().GetExecution(ctx, preflight.RecoverySourceOperationID)
		if err != nil {
			return observationRecords{}, err
		}
	}
	execution, err := tx.Executions().GetExecution(ctx, operationID)
	if err != nil {
		return observationRecords{}, err
	}

	var sourceAttempt application.SubmissionAttemptRecord
	if preflight.IsOutputRecovery() {
		sourceAttempt, err = tx.SubmissionAttempts().GetSubmissionAttempt(ctx, preflight.RecoverySourceOperationID, preflight.RecoverySourceAttempt)
		if err != nil {
			return observationRecords{}, err
		}
	}

	if execution.ResourceID != preflight.ResourceID || execution.RecoverySourceOperationID != preflight.RecoverySourceOperationID ||
		execution.RecoverySourceAttempt != preflight.RecoverySourceAttempt || resource.Resource.ID() != execution.ResourceID ||
		operation.Operation.ID() != execution.OperationID || operation.Operation.ResourceID() != execution.ResourceID {
		return observationRecords{}, application.ErrConcurrencyConflict
	}
	if execution.IsOutputRecovery() {
		if err := application.ValidateOutputRecoverySource(execution, source, sourceOperation.Operation, sourceAttempt); err != nil {
			return observationRecords{}, err
		}
	}
	return observationRecords{execution: execution, source: source, operation: operation}, nil
}

func (w *Worker) dispatch(ctx context.Context, message application.OutboxMessage) error {
	prepared, err := w.prepareDispatch(ctx, message)
	if err != nil {
		return err
	}
	submitCtx, cancel := context.WithCancel(ctx)
	heartbeat, heartbeatStartErr := w.startLeaseHeartbeat(submitCtx, cancel, message)
	if heartbeatStartErr != nil {
		cancel()
		return ambiguousDispatchError{cause: fmt.Errorf("dispatch lease ownership lost: %w", heartbeatStartErr), leaseLost: true}
	}
	var submission provisioning.Submission
	var submitErr error
	var heartbeatErr error
	submitPanicked := false
	func() {
		// A panic during Submit is indistinguishable from an interrupted
		// submission: infrastructure work may already have launched. It is
		// converted to ambiguity, never to failure, and the heartbeat is
		// always stopped so an abandoned lease can expire and recover through
		// the existing Unknown -> Observe machinery (ADR-0018).
		defer func() {
			if recovered := recover(); recovered != nil {
				submitPanicked = true
				submitErr = fmt.Errorf("provider submit panicked: %s", sanitizePanicValue(recovered))
			}
			heartbeatErr = heartbeat.stop()
		}()
		request := executionRequest(prepared.execution)
		if fenced, ok := prepared.provider.(provisioning.FencedProvisioner); ok {
			submission, submitErr = fenced.SubmitFenced(submitCtx, request, executionFence(message, false))
		} else {
			submission, submitErr = prepared.provider.Submit(submitCtx, request)
		}
	}()
	cancel()
	if submitPanicked {
		return ambiguousDispatchError{cause: submitErr}
	}
	if heartbeatErr != nil {
		// The heartbeat could no longer renew: this worker provably lost
		// fenced ownership of the lease. That is a different operator
		// diagnosis from submission ambiguity (ADR-0018).
		return ambiguousDispatchError{cause: fmt.Errorf("dispatch lease ownership lost"), leaseLost: true}
	}
	if notAttempted, ok := provisioning.AsSubmissionNotAttempted(submitErr); ok {
		if notAttempted.Validate() == nil && !errors.Is(submitErr, provisioning.ErrAmbiguousSubmission) && submission.IsZero() {
			if retryErr := w.retryNotAttemptedDispatch(ctx, message, prepared, notAttempted); retryErr != nil {
				return ambiguousDispatchError{cause: retryErr, leaseLost: errors.Is(retryErr, application.ErrConcurrencyConflict)}
			}
			return dispatchRequeuedError{cause: submitErr}
		}
		// A not-attempted claim that is malformed, joined with ambiguity, or
		// accompanied by backend facts is contradictory. Discard those facts
		// and durably recover through Unknown -> Observe, never redispatch.
		submission = provisioning.Submission{}
		submitErr = fmt.Errorf("%w: contradictory submission-not-attempted result: %v", provisioning.ErrAmbiguousSubmission, submitErr)
	}
	if err := w.recordDispatch(ctx, message, prepared, submission, submitErr); err != nil {
		if recoveryErr := w.markDispatchUnknown(ctx, message, prepared, err); recoveryErr == nil {
			return nil
		}
		// If the persistence failure was a fencing conflict, another claimant
		// already owns or settled this work: ownership is lost. Anything else
		// conservatively preserves ambiguity with the lease intact.
		return ambiguousDispatchError{cause: err, leaseLost: errors.Is(err, application.ErrConcurrencyConflict)}
	}
	return nil
}

type leaseHeartbeat struct {
	stopSignal chan struct{}
	done       chan error
}

func (h leaseHeartbeat) stop() error {
	close(h.stopSignal)
	return <-h.done
}

func (w *Worker) startLeaseHeartbeat(ctx context.Context, cancel context.CancelFunc, message application.OutboxMessage) (leaseHeartbeat, error) {
	heartbeat := leaseHeartbeat{stopSignal: make(chan struct{}), done: make(chan error, 1)}
	renew := func() error {
		return w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
			return tx.Outbox().RenewOutbox(ctx, message.ID, message.LeaseToken, w.Lease)
		})
	}
	if err := renew(); err != nil {
		return leaseHeartbeat{}, err
	}
	interval := w.Lease / 3
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeat.stopSignal:
				heartbeat.done <- renew()
				return
			case <-ticker.C:
				if err := renew(); err != nil {
					cancel()
					heartbeat.done <- err
					return
				}
			case <-ctx.Done():
				heartbeat.done <- ctx.Err()
				return
			}
		}
	}()
	return heartbeat, nil
}

// dispatchRequeuedError reports a transient Submit failure that conclusively
// happened before any provider execution attempt. The durable retry has
// already been committed by the dispatch handler.
type dispatchRequeuedError struct{ cause error }

func (e dispatchRequeuedError) Error() string { return e.cause.Error() }
func (e dispatchRequeuedError) Unwrap() error { return e.cause }

// leaseOwnershipLostError fences observation results after any heartbeat
// renewal failure. Callers must return it without further durable writes.
type leaseOwnershipLostError struct{ cause error }

func (e leaseOwnershipLostError) Error() string {
	return "outbox lease ownership lost: " + e.cause.Error()
}
func (e leaseOwnershipLostError) Unwrap() error { return e.cause }

// ambiguousDispatchError reports that the outcome of one provider submission
// is uncertain. leaseLost distinguishes the two operator diagnoses: a plain
// ambiguous error means this worker still owns the fenced lease and the
// external submission outcome is merely unknown; leaseLost means fencing
// ownership was provably lost (heartbeat renewal failure or another claimant
// moved the durable state). Both keep the lease machinery intact so expiry
// recovery decides safely; neither ever invents a definitive failure.
type ambiguousDispatchError struct {
	cause     error
	leaseLost bool
}

func (e ambiguousDispatchError) Error() string {
	return "dispatch result is ambiguous: " + e.cause.Error()
}
func (e ambiguousDispatchError) Unwrap() error { return e.cause }

func (w *Worker) markDispatchUnknown(ctx context.Context, message application.OutboxMessage, prepared dispatchContext, cause error) error {
	return w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
		currentMessage, err := tx.Outbox().GetOutbox(ctx, message.ID)
		if err != nil {
			return err
		}
		if currentMessage.State != application.OutboxLeased || currentMessage.LeaseToken != message.LeaseToken {
			return application.ErrConcurrencyConflict
		}
		execution, err := tx.Executions().GetExecution(ctx, message.OperationID)
		if err != nil {
			return err
		}
		if execution.Version != prepared.version || execution.CurrentAttempt != message.AttemptNumber || execution.State != application.AttemptDispatching {
			return application.ErrConcurrencyConflict
		}
		attempt, err := tx.SubmissionAttempts().GetSubmissionAttempt(ctx, message.OperationID, message.AttemptNumber)
		if err != nil {
			return err
		}
		if attempt.State != application.SubmissionAttemptLeased {
			return application.ErrConcurrencyConflict
		}
		failure := &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "DispatchResultPersistenceFailed", Message: cause.Error()}
		attempt.State = application.SubmissionAttemptUnknown
		attempt.Failure = failure
		if err := tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, attempt, application.SubmissionAttemptLeased); err != nil {
			return err
		}
		execution.State = application.AttemptUnknown
		execution.Correlation = provisioning.RequestCorrelationUnknown
		execution.LastFailure = failure
		observe := scheduleObserve(&execution, w.RetryBase)
		if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
			return err
		}
		if err := tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "DispatchResultPersistenceFailed"); err != nil {
			return err
		}
		return tx.Outbox().Enqueue(ctx, observe)
	})
}

func (w *Worker) retryNotAttemptedDispatch(ctx context.Context, message application.OutboxMessage, prepared dispatchContext, failure provisioning.SubmissionNotAttemptedError) error {
	return w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
		current, err := tx.Outbox().GetOutbox(ctx, message.ID)
		if err != nil {
			return err
		}
		if current.State != application.OutboxLeased || current.LeaseToken != message.LeaseToken {
			return application.ErrConcurrencyConflict
		}
		execution, err := tx.Executions().GetExecution(ctx, message.OperationID)
		if err != nil {
			return err
		}
		if execution.Version != prepared.version || execution.CurrentAttempt != message.AttemptNumber || execution.State != application.AttemptDispatching {
			return application.ErrConcurrencyConflict
		}
		attempt, err := tx.SubmissionAttempts().GetSubmissionAttempt(ctx, message.OperationID, message.AttemptNumber)
		if err != nil {
			return err
		}
		if attempt.State != application.SubmissionAttemptLeased || attempt.DispatchMessage != message.ID {
			return application.ErrConcurrencyConflict
		}
		attempt.State = application.SubmissionAttemptPending
		attempt.ClaimedAt = time.Time{}
		attempt.ResolvedAt = time.Time{}
		attempt.Failure = nil
		if err := tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, attempt, application.SubmissionAttemptLeased); err != nil {
			return err
		}
		execution.State = application.AttemptPending
		if err := tx.Executions().SaveExecution(ctx, execution, prepared.version); err != nil {
			return err
		}
		return tx.Outbox().RetryDispatchOutbox(ctx, message.ID, message.LeaseToken, prepared.version+1,
			w.backoff(message.AttemptCount), failure.Error())
	})
}

func (w *Worker) prepareDispatch(ctx context.Context, message application.OutboxMessage) (dispatchContext, error) {
	var result dispatchContext
	err := w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
		current, err := tx.Outbox().GetOutbox(ctx, message.ID)
		if err != nil {
			return err
		}
		if current.State != application.OutboxLeased || current.LeaseToken != message.LeaseToken {
			return application.ErrConcurrencyConflict
		}
		execution, err := tx.Executions().GetExecution(ctx, message.OperationID)
		if err != nil {
			return err
		}
		if execution.Version != message.ExpectedVersion || execution.CurrentAttempt != message.AttemptNumber || execution.State != application.AttemptPending {
			return application.ErrConcurrencyConflict
		}
		if execution.IsOutputRecovery() {
			return fmt.Errorf("%w: output recovery cannot be dispatched", application.ErrInvalidApplicationCall)
		}
		result.execution = execution
		return nil
	})
	if err != nil {
		return dispatchContext{}, err
	}
	provider, err := w.Resolver.Resolve(ctx, result.execution.ProvisionerRef)
	if err != nil || provider == nil {
		return dispatchContext{}, fmt.Errorf("%w: %v", application.ErrProvisionerNotFound, err)
	}
	result.provider = provider
	outputMappingRef := result.execution.OutputMappingRef
	if outputMappingRef == "" {
		if source, ok := provider.(provisioning.OutputMappingSource); ok {
			outputMappingRef = source.OutputMappingRef(result.execution.ResourceType, result.execution.Capability)
		}
	}
	err = w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
		current, err := tx.Outbox().GetOutbox(ctx, message.ID)
		if err != nil {
			return err
		}
		if current.State != application.OutboxLeased || current.LeaseToken != message.LeaseToken {
			return application.ErrConcurrencyConflict
		}
		execution, err := tx.Executions().GetExecution(ctx, message.OperationID)
		if err != nil {
			return err
		}
		if execution.Version != message.ExpectedVersion || execution.CurrentAttempt != message.AttemptNumber || execution.State != application.AttemptPending {
			return application.ErrConcurrencyConflict
		}
		attempt, err := tx.SubmissionAttempts().GetSubmissionAttempt(ctx, message.OperationID, message.AttemptNumber)
		if err != nil {
			return err
		}
		if attempt.State != application.SubmissionAttemptPending || attempt.DispatchMessage != message.ID {
			return application.ErrConcurrencyConflict
		}
		if err := tx.Outbox().RenewOutbox(ctx, message.ID, message.LeaseToken, w.Lease); err != nil {
			return err
		}
		attempt.State = application.SubmissionAttemptLeased
		if err := tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, attempt, application.SubmissionAttemptPending); err != nil {
			return err
		}
		// The durable output-mapping identity is bound before provider work and
		// never changes afterwards. SaveExecution applies the version CAS and
		// immutable mapping guard to the value selected outside this transaction.
		if execution.OutputMappingRef == "" {
			execution.OutputMappingRef = outputMappingRef
		}
		execution.State = application.AttemptDispatching
		if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
			return err
		}
		execution.Version++
		result.execution = execution
		result.version = execution.Version
		return nil
	})
	if err != nil {
		return dispatchContext{}, err
	}
	return result, nil
}

func (w *Worker) recordDispatch(ctx context.Context, message application.OutboxMessage, prepared dispatchContext, submission provisioning.Submission, submitErr error) error {
	return w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
		currentMessage, err := tx.Outbox().GetOutbox(ctx, message.ID)
		if err != nil {
			return err
		}
		if currentMessage.State != application.OutboxLeased || currentMessage.LeaseToken != message.LeaseToken {
			return application.ErrConcurrencyConflict
		}
		execution, err := tx.Executions().GetExecution(ctx, message.OperationID)
		if err != nil {
			return err
		}
		if execution.Version != prepared.version || execution.CurrentAttempt != message.AttemptNumber || execution.State != application.AttemptDispatching {
			return application.ErrConcurrencyConflict
		}
		attempt, err := tx.SubmissionAttempts().GetSubmissionAttempt(ctx, message.OperationID, message.AttemptNumber)
		if err != nil {
			return err
		}
		if attempt.State != application.SubmissionAttemptLeased {
			return application.ErrConcurrencyConflict
		}
		observedAt := w.observedAt(submission.Observation.ObservedAt)
		execution, attempt, outcome, finish, err := application.InterpretSubmission(execution, attempt, submission, submitErr, observedAt)
		if err != nil {
			return err
		}
		if err := tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, attempt, application.SubmissionAttemptLeased); err != nil {
			return err
		}
		switch outcome {
		case application.SubmissionOutcomeAmbiguous, application.SubmissionOutcomeAccepted:
			reason := "Submitted"
			if outcome == application.SubmissionOutcomeAmbiguous {
				reason = "AmbiguousSubmission"
			}
			observe := scheduleObserve(&execution, w.RetryBase)
			if err := tx.Executions().SaveExecution(ctx, execution, prepared.version); err != nil {
				return err
			}
			if err := tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, reason); err != nil {
				return err
			}
			return tx.Outbox().Enqueue(ctx, observe)
		case application.SubmissionOutcomeSucceeded:
			return w.finishSuccessInTx(ctx, tx, message, execution, prepared.version, *finish, observedAt, submission.Observation.Outputs)
		case application.SubmissionOutcomeRejected, application.SubmissionOutcomeFailed:
			return w.finishOperation(ctx, tx, message, execution, prepared.version, *finish, observedAt)
		default:
			return fmt.Errorf("invalid submission outcome %d", outcome)
		}
	})
}

func (w *Worker) observe(ctx context.Context, message application.OutboxMessage) error {
	var loaded application.ProvisioningExecutionRecord
	var request provisioning.ObservationRequest
	var provider provisioning.Provisioner
	if err := w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
		current, err := tx.Outbox().GetOutbox(ctx, message.ID)
		if err != nil {
			return err
		}
		if current.State != application.OutboxLeased || current.LeaseToken != message.LeaseToken {
			return application.ErrConcurrencyConflict
		}
		records, err := lockObservationRecords(ctx, tx, message.OperationID, message.ExpectedVersion, message.Sequence+1)
		if err != nil {
			return err
		}
		loaded = records.execution
		if loaded.Version != message.ExpectedVersion || loaded.NextObservation != message.Sequence+1 {
			return application.ErrConcurrencyConflict
		}
		request = observationRequest(loaded)
		if loaded.IsOutputRecovery() {
			source := records.source
			request = provisioning.ObservationRequest{
				OperationID: source.OperationID, AttemptNumber: loaded.RecoverySourceAttempt,
				ResourceID: source.ResourceID, ResourceType: source.ResourceType, Spec: source.Spec,
				Capability: source.Capability, TargetGeneration: source.TargetGeneration,
				Handle: source.Handle, OutputMappingRef: loaded.OutputMappingRef,
				OutputSourceMappingRef: source.OutputMappingRef,
			}
		}
		return nil
	}); err != nil {
		return err
	}
	var err error
	provider, err = w.Resolver.Resolve(ctx, loaded.ProvisionerRef)
	if err != nil || provider == nil {
		return fmt.Errorf("%w: %v", application.ErrProvisionerNotFound, err)
	}
	observation, observeErr := w.observeWithHeartbeat(ctx, message, provider, request)
	if observeErr != nil {
		return observeErr
	}
	if !application.ValidCorrelation(observation.Correlation) {
		return fmt.Errorf("invalid request correlation %q", observation.Correlation)
	}
	if observation.Correlation == provisioning.RequestCorrelationNotFound && observation.Execution != nil && observation.Execution.State != provisioning.ExecutionStateFailed {
		return fmt.Errorf("contradictory observation reports NotFound with an execution")
	}
	return w.recordObservation(ctx, message, loaded, observation)
}

func (w *Worker) observeWithHeartbeat(ctx context.Context, message application.OutboxMessage, provider provisioning.Provisioner, request provisioning.ObservationRequest) (observation provisioning.ExecutionObservation, err error) {
	observeCtx, cancel := context.WithCancel(ctx)
	heartbeat, heartbeatStartErr := w.startLeaseHeartbeat(observeCtx, cancel, message)
	if heartbeatStartErr != nil {
		cancel()
		return provisioning.ExecutionObservation{}, leaseOwnershipLostError{cause: heartbeatStartErr}
	}
	var heartbeatErr error
	func() {
		// The defer also runs during a provider panic, preventing a leaked
		// heartbeat from retaining ownership after the work item unwinds.
		defer func() {
			heartbeatErr = heartbeat.stop()
			cancel()
		}()
		if fenced, ok := provider.(provisioning.FencedProvisioner); ok {
			observation, err = fenced.ObserveFenced(observeCtx, request, executionFence(message, message.Kind == application.OutboxPassiveObserve))
		} else {
			observation, err = provider.Observe(observeCtx, request)
		}
	}()
	if heartbeatErr != nil {
		return provisioning.ExecutionObservation{}, leaseOwnershipLostError{cause: heartbeatErr}
	}
	return observation, err
}

func executionFence(message application.OutboxMessage, passive bool) provisioning.ExecutionFence {
	return provisioning.ExecutionFence{MessageID: message.ID, LeaseToken: message.LeaseToken, Passive: passive}
}

func (w *Worker) recordObservation(ctx context.Context, message application.OutboxMessage, loaded application.ProvisioningExecutionRecord, observation provisioning.ExecutionObservation) error {
	return w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
		currentMessage, err := tx.Outbox().GetOutbox(ctx, message.ID)
		if err != nil {
			return err
		}
		if currentMessage.State != application.OutboxLeased || currentMessage.LeaseToken != message.LeaseToken {
			return application.ErrConcurrencyConflict
		}
		records, err := lockObservationRecords(ctx, tx, message.OperationID, loaded.Version, message.Sequence+1)
		if err != nil {
			return err
		}
		execution := records.execution
		if execution.Version != loaded.Version || execution.CurrentAttempt != loaded.CurrentAttempt || execution.NextObservation != message.Sequence+1 {
			return application.ErrConcurrencyConflict
		}
		// Backend success is already durable and only the output dimension is
		// outstanding. Re-drive extraction through the provider's observation
		// of existing state — never a re-execution — with evidence timestamps
		// pinned to the persisted terminal instant so repeated observations of
		// an unchanged backend are accepted while resolution advances.
		if execution.State == application.AttemptSucceeded && execution.OutputResolution == application.OutputResolutionPending {
			if records.operation.Operation.IsTerminal() {
				return application.ErrConcurrencyConflict
			}
			if observation.Correlation != provisioning.RequestCorrelationFound || observation.Execution == nil || observation.Execution.State != provisioning.ExecutionStateSucceeded {
				if execution.IsOutputRecovery() {
					execution.LastObservation = &observation
					execution.LastObservedAt = w.observedAt(observation.ObservedAt)
					execution.Correlation = observation.Correlation
					if !observation.ObservedAt.IsZero() && (execution.LastProviderObservedAt.IsZero() || observation.ObservedAt.After(execution.LastProviderObservedAt)) {
						execution.LastProviderObservedAt = observation.ObservedAt
					}
					next := scheduleObserve(&execution, w.RetryBase)
					if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
						return err
					}
					if err := tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, observeCompletionReason(observation)); err != nil {
						return err
					}
					return tx.Outbox().Enqueue(ctx, next)
				}
				return fmt.Errorf("%w: pending-output observation is not positively correlated terminal success", lifecycle.ErrInvalidTransition)
			}
			finishAt := execution.LastObservedAt
			if execution.IsOutputRecovery() {
				finishAt = w.observedAt(observation.ObservedAt)
				execution.LastObservation = &observation
				execution.LastObservedAt = finishAt
				execution.Correlation = observation.Correlation
				if observation.Execution.Handle != nil {
					execution.Handle = observation.Execution.Handle
				}
				if !observation.ObservedAt.IsZero() {
					execution.LastProviderObservedAt = observation.ObservedAt
				}
			}
			return w.finishSuccessInTx(ctx, tx, message, execution, execution.Version,
				application.Finish{Succeeded: true, Reason: "ObservationSucceeded", Facts: observation.Resource},
				finishAt, observation.Outputs)
		}
		attempt, err := tx.SubmissionAttempts().GetSubmissionAttempt(ctx, execution.OperationID, execution.CurrentAttempt)
		if err != nil {
			return err
		}
		observedAt := w.observedAt(observation.ObservedAt)
		execution, attempt, outcome, finish, err := application.InterpretObservation(execution, attempt, observation, observedAt)
		if err != nil {
			return err
		}
		switch outcome {
		case application.ObservationOutcomeStale:
			// Stale evidence must not terminate the observe loop: the
			// execution is still nonterminal, so keep observing with a
			// bounded delay instead of stranding the active operation.
			next := scheduleObserve(&execution, w.RetryBase)
			if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
				return err
			}
			if err := tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "StaleObservation"); err != nil {
				return err
			}
			return tx.Outbox().Enqueue(ctx, next)
		case application.ObservationOutcomeRejected:
			if err := tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, attempt, application.SubmissionAttemptUnknown); err != nil {
				return err
			}
			return w.finishOperation(ctx, tx, message, execution, execution.Version, *finish, observedAt)
		case application.ObservationOutcomeRetry:
			dispatch := application.DispatchMessage(execution.OperationID, execution.CurrentAttempt, execution.Version+1)
			if err := tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, attempt, application.SubmissionAttemptUnknown); err != nil {
				return err
			}
			if err := tx.SubmissionAttempts().CreateSubmissionAttempt(ctx, application.SubmissionAttemptRecord{OperationID: execution.OperationID, AttemptNumber: execution.CurrentAttempt, State: application.SubmissionAttemptPending, DispatchMessage: dispatch.ID}); err != nil {
				return err
			}
			if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
				return err
			}
			if err := tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "SubmissionNotFound"); err != nil {
				return err
			}
			return tx.Outbox().Enqueue(ctx, dispatch)
		case application.ObservationOutcomeObserve:
			next := scheduleObserve(&execution, w.RetryBase)
			if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
				return err
			}
			if err := tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, observeCompletionReason(observation)); err != nil {
				return err
			}
			return tx.Outbox().Enqueue(ctx, next)
		case application.ObservationOutcomeSucceeded:
			return w.finishSuccessInTx(ctx, tx, message, execution, execution.Version, *finish, observedAt, observation.Outputs)
		case application.ObservationOutcomeFailed:
			return w.finishOperation(ctx, tx, message, execution, execution.Version, *finish, observedAt)
		default:
			return fmt.Errorf("invalid observation outcome %d", outcome)
		}
	})
}

func (w *Worker) passiveObserve(ctx context.Context, message application.OutboxMessage) error {
	var loaded application.ResourceRecord
	stale := false
	if err := w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
		currentMessage, err := tx.Outbox().GetOutbox(ctx, message.ID)
		if err != nil {
			return err
		}
		if currentMessage.State != application.OutboxLeased || currentMessage.LeaseToken != message.LeaseToken {
			return application.ErrConcurrencyConflict
		}
		loaded, err = tx.Resources().GetResource(ctx, message.ResourceID)
		if err != nil {
			return err
		}
		if loaded.Version != message.ExpectedVersion {
			stale = true
			return tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "StalePassiveObservation")
		}
		return nil
	}); err != nil {
		return err
	}
	if stale {
		return nil
	}
	provider, err := w.Resolver.Resolve(ctx, loaded.ProvisionerRef)
	if err != nil || provider == nil {
		return fmt.Errorf("%w: %v", application.ErrProvisionerNotFound, err)
	}
	observation, err := w.observeWithHeartbeat(ctx, message, provider, provisioning.ObservationRequest{ResourceID: loaded.Resource.ID(), ResourceType: loaded.Resource.Type(), Spec: loaded.Resource.Spec(), TargetGeneration: loaded.Resource.Generation()})
	if err != nil {
		return err
	}
	return w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
		currentMessage, err := tx.Outbox().GetOutbox(ctx, message.ID)
		if err != nil {
			return err
		}
		if currentMessage.State != application.OutboxLeased || currentMessage.LeaseToken != message.LeaseToken {
			return application.ErrConcurrencyConflict
		}
		current, err := tx.Resources().GetResource(ctx, message.ResourceID)
		if err != nil {
			return err
		}
		if current.Version != loaded.Version {
			return application.ErrConcurrencyConflict
		}
		observedAt := w.observedAt(observation.ObservedAt)
		if observedAt.Before(current.Resource.UpdatedAt()) || observedAt.Before(current.Status.UpdatedAt()) {
			return tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "StaleObservation")
		}
		if _, found, err := tx.Operations().ActiveForResource(ctx, message.ResourceID); err != nil {
			return err
		} else if found {
			return tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "StaleObservation")
		}
		status, err := w.Lifecycle.ApplyObservation(current.Resource, current.Status, observation.Resource, observedAt)
		if err != nil {
			return err
		}
		current.Status = status
		if err := tx.Resources().SaveResource(ctx, current, current.Version); err != nil {
			return err
		}
		return tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "PassivelyObserved")
	})
}

func (w *Worker) finishOperation(ctx context.Context, tx application.UnitOfWork, message application.OutboxMessage, execution application.ProvisioningExecutionRecord, expectedExecutionVersion uint64, finish application.Finish, at time.Time) error {
	preflight, err := tx.Operations().LookupOperation(ctx, execution.OperationID)
	if err != nil {
		return err
	}
	resource, err := tx.Resources().GetResource(ctx, preflight.Operation.ResourceID())
	if err != nil {
		return err
	}
	operation, err := tx.Operations().GetOperation(ctx, execution.OperationID)
	if err != nil {
		return err
	}
	at = alignEvidenceTimeline(at, &execution, resource)
	result, err := application.BuildFinishEvidence(w.Lifecycle, operation.Operation, resource.Resource, resource.Status, finish, at)
	if err != nil {
		return err
	}
	resource.Status = result.Status
	if err := tx.Resources().SaveResource(ctx, resource, resource.Version); err != nil {
		return err
	}
	if err := tx.Operations().SaveOperation(ctx, application.OperationRecord{Operation: result.Operation}, operation.Version); err != nil {
		return err
	}
	w.noteTerminalTransition(operation.Operation, result.Operation, resource.Resource.ID())
	if err := tx.Events().Append(ctx, result.Event); err != nil {
		return err
	}
	if err := tx.Executions().SaveExecution(ctx, execution, expectedExecutionVersion); err != nil {
		return err
	}
	return tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "TerminalExecution")
}

// finishSuccessInTx applies a positively correlated backend success inside an
// already-fenced transaction. The output dimension is resolved against the
// developer contract before any lifecycle completion:
//
//   - Publish: validated values persist atomically with reconciliation success.
//   - Reject: the postcondition failure fails the operation while backend
//     success evidence stays intact.
//   - Defer: backend success persists with Pending resolution; extraction is
//     re-driven through Observe without re-executing the backend.
//   - None: no outputs are declared; completion is plain.
func (w *Worker) finishSuccessInTx(ctx context.Context, tx application.UnitOfWork, message application.OutboxMessage, execution application.ProvisioningExecutionRecord, expectedExecutionVersion uint64, finish application.Finish, at time.Time, outputs *provisioning.OutputEvidence) error {
	preflight, err := tx.Operations().LookupOperation(ctx, execution.OperationID)
	if err != nil {
		return err
	}
	resource, err := tx.Resources().GetResource(ctx, preflight.Operation.ResourceID())
	if err != nil {
		return err
	}
	operation, err := tx.Operations().GetOperation(ctx, execution.OperationID)
	if err != nil {
		return err
	}
	if operation.Operation.IsTerminal() {
		return fmt.Errorf("%w: cannot complete a terminal operation", lifecycle.ErrInvalidTransition)
	}
	if err := application.ValidateOutputEvidenceMapping(execution.OutputMappingRef, outputs); err != nil {
		return err
	}

	var plan application.OutputPlan
	switch {
	case w.Types == nil && outputs == nil:
		plan = application.OutputPlan{Action: application.OutputPlanNone}
	case w.Types == nil:
		return fmt.Errorf("output evidence requires a composed resource type catalog")
	default:
		contract, contractErr := w.Types.Get(ctx, execution.ResourceType)
		if contractErr != nil {
			return fmt.Errorf("resource type catalog unavailable for output validation: %w", contractErr)
		}
		plan, err = application.PlanTerminalOutputs(contract, execution.Capability, outputs, execution.TargetGeneration, at)
		if err != nil {
			return err
		}
	}

	if plan.Action == application.OutputPlanDefer {
		execution.State = application.AttemptSucceeded
		execution.OutputResolution = application.OutputResolutionPending
		next := scheduleObserve(&execution, w.RetryBase)
		if err := tx.Executions().SaveExecution(ctx, execution, expectedExecutionVersion); err != nil {
			return err
		}
		if err := tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "OutputsPending"); err != nil {
			return err
		}
		return tx.Outbox().Enqueue(ctx, next)
	}

	if plan.Action == application.OutputPlanReject {
		rejection := application.Finish{Succeeded: false, Reason: plan.Failure.Reason, Message: plan.Failure.Message, Facts: domain.ObservedFacts{}}
		result, err := application.BuildFinishEvidence(w.Lifecycle, operation.Operation, resource.Resource, resource.Status, rejection, at)
		if err != nil {
			return err
		}
		execution.State = application.AttemptSucceeded
		execution.OutputResolution = application.OutputResolutionRejected
		execution.OutputFailureReason = plan.Failure.Reason
		execution.OutputFailureMessage = plan.Failure.Message
		resource.Status = result.Status
		if err := tx.Resources().SaveResource(ctx, resource, resource.Version); err != nil {
			return err
		}
		if err := tx.Operations().SaveOperation(ctx, application.OperationRecord{Operation: result.Operation}, operation.Version); err != nil {
			return err
		}
		w.noteTerminalTransition(operation.Operation, result.Operation, resource.Resource.ID())
		if err := tx.Events().Append(ctx, result.Event); err != nil {
			return err
		}
		if err := tx.Executions().SaveExecution(ctx, execution, expectedExecutionVersion); err != nil {
			return err
		}
		return tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "OutputPostconditionRejected")
	}

	var publishRecord *application.ResourceOutputRecord
	if plan.Action == application.OutputPlanPublish {
		contract, contractErr := w.Types.Get(ctx, execution.ResourceType)
		if contractErr != nil {
			return fmt.Errorf("resource type catalog unavailable for output provenance: %w", contractErr)
		}
		contractDigest, digestErr := application.OutputContractDigest(contract.OutputContract())
		if digestErr != nil {
			return digestErr
		}
		valuesDigest, digestErr := application.ValuesDigest(plan.Snapshot.Values())
		if digestErr != nil {
			return digestErr
		}
		publishRecord = &application.ResourceOutputRecord{
			ResourceID:           resource.Resource.ID(),
			ObservedGeneration:   plan.Snapshot.ObservedGeneration(),
			OperationID:          operation.Operation.ID(),
			Capability:           operation.Operation.Capability(),
			OutputMappingRef:     execution.OutputMappingRef,
			OutputContractDigest: contractDigest,
			Values:               plan.Snapshot,
			ValuesDigest:         valuesDigest,
		}
		execution.OutputResolution = application.OutputResolutionPublished
	} else {
		execution.OutputResolution = application.OutputResolutionNone
	}

	at = alignEvidenceTimeline(at, &execution, resource)
	result, err := application.BuildFinishEvidence(w.Lifecycle, operation.Operation, resource.Resource, resource.Status, finish, at)
	if err != nil {
		return err
	}
	resource.Status = result.Status
	if err := tx.Resources().SaveResource(ctx, resource, resource.Version); err != nil {
		return err
	}
	if publishRecord != nil {
		if err := tx.Outputs().SaveResourceOutputs(ctx, *publishRecord); err != nil {
			return err
		}
	}
	if err := tx.Operations().SaveOperation(ctx, application.OperationRecord{Operation: result.Operation}, operation.Version); err != nil {
		return err
	}
	w.noteTerminalTransition(operation.Operation, result.Operation, resource.Resource.ID())
	if err := tx.Events().Append(ctx, result.Event); err != nil {
		return err
	}
	if err := tx.Executions().SaveExecution(ctx, execution, expectedExecutionVersion); err != nil {
		return err
	}
	return tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "TerminalExecution")
}

func (w *Worker) recoverExpiredDispatch(ctx context.Context) (bool, error) {
	recovered := false
	err := w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
		message, found, err := tx.Outbox().FindExpiredDispatch(ctx)
		if err != nil || !found {
			return err
		}
		recovered = true
		execution, err := tx.Executions().GetExecution(ctx, message.OperationID)
		if err != nil {
			return err
		}
		if execution.CurrentAttempt != message.AttemptNumber {
			return tx.Outbox().CompleteExpiredOutbox(ctx, message.ID, message.LeaseToken, "StaleExpiredDispatch")
		}
		attempt, err := tx.SubmissionAttempts().GetSubmissionAttempt(ctx, message.OperationID, message.AttemptNumber)
		if err != nil {
			return err
		}
		if execution.State == application.AttemptPending && attempt.State == application.SubmissionAttemptPending {
			return tx.Outbox().RequeueExpiredOutbox(ctx, message.ID, message.LeaseToken)
		}
		if execution.State != application.AttemptDispatching || attempt.State != application.SubmissionAttemptLeased {
			return application.ErrConcurrencyConflict
		}
		provider, resolveErr := w.Resolver.Resolve(ctx, execution.ProvisionerRef)
		if resolveErr == nil && provider != nil {
			if redeliverer, ok := provider.(provisioning.ExpiredDispatchRedeliverer); ok && redeliverer.CanRedeliverExpiredDispatch() {
				attempt.State = application.SubmissionAttemptPending
				attempt.ClaimedAt = time.Time{}
				attempt.ResolvedAt = time.Time{}
				attempt.Failure = nil
				if err := tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, attempt, application.SubmissionAttemptLeased); err != nil {
					return err
				}
				execution.State = application.AttemptPending
				if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
					return err
				}
				return tx.Outbox().RetryExpiredDispatchOutbox(ctx, message.ID, message.LeaseToken, execution.Version+1,
					w.backoff(message.AttemptCount), "ExpiredFencedDispatch")
			}
		}
		attempt.State = application.SubmissionAttemptUnknown
		if err := tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, attempt, application.SubmissionAttemptLeased); err != nil {
			return err
		}
		execution.State = application.AttemptUnknown
		execution.Correlation = provisioning.RequestCorrelationUnknown
		next := scheduleObserve(&execution, 0)
		if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
			return err
		}
		if err := tx.Outbox().CompleteExpiredOutbox(ctx, message.ID, message.LeaseToken, "LeaseExpiredAmbiguous"); err != nil {
			return err
		}
		return tx.Outbox().Enqueue(ctx, next)
	})
	return recovered, err
}

func scheduleObserve(execution *application.ProvisioningExecutionRecord, delay time.Duration) application.OutboxMessage {
	sequence := execution.NextObservation
	if sequence == 0 {
		sequence = 1
	}
	execution.NextObservation = sequence + 1
	message := application.ObserveMessage(execution.OperationID, sequence, execution.Version+1)
	message.Delay = delay
	return message
}

func executionRequest(execution application.ProvisioningExecutionRecord) provisioning.ExecutionRequest {
	return provisioning.ExecutionRequest{OperationID: execution.OperationID, AttemptNumber: execution.CurrentAttempt, ResourceID: execution.ResourceID, ResourceType: execution.ResourceType,
		Spec: execution.Spec, Capability: execution.Capability, TargetGeneration: execution.TargetGeneration,
		OutputMappingRef: execution.OutputMappingRef}
}

func observationRequest(execution application.ProvisioningExecutionRecord) provisioning.ObservationRequest {
	return provisioning.ObservationRequest{OperationID: execution.OperationID, AttemptNumber: execution.CurrentAttempt, ResourceID: execution.ResourceID, ResourceType: execution.ResourceType,
		Spec: execution.Spec, Capability: execution.Capability, TargetGeneration: execution.TargetGeneration, Handle: execution.Handle,
		OutputMappingRef: execution.OutputMappingRef}
}

func observeCompletionReason(observation provisioning.ExecutionObservation) string {
	switch {
	case observation.Execution == nil:
		return "ObservedNoCurrentExecution"
	case observation.Correlation != provisioning.RequestCorrelationFound:
		return "ObservedUncorrelatedExecution"
	default:
		return "ObservedNonterminal"
	}
}

func (w *Worker) observedAt(providerTime time.Time) time.Time {
	if !providerTime.IsZero() {
		return providerTime
	}
	return w.Clock().UTC()
}

func (w *Worker) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 10 {
		attempt = 10
	}
	return w.RetryBase * time.Duration(1<<(attempt-1))
}

// alignEvidenceTimeline pins correlated terminal evidence onto Liftr's
// monotonic timeline. Backend clocks can be coarser than Liftr's own (for
// example second-granular history end times), so provider evidence that
// predates state Liftr durably advanced AFTER launching this very execution
// is lifted to the persisted frontier instead of being rejected as regressive.
// The execution's effective observation time moves with it so completion
// timestamps and evidence stay consistent across restarts and replays.
func alignEvidenceTimeline(at time.Time, execution *application.ProvisioningExecutionRecord, resource application.ResourceRecord) time.Time {
	frontier := resource.Status.UpdatedAt()
	if resource.Resource.UpdatedAt().After(frontier) {
		frontier = resource.Resource.UpdatedAt()
	}
	if at.Before(frontier) {
		at = frontier
		execution.LastObservedAt = at
	}
	return at
}

func newToken() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("create lease token: %w", err)
	}
	return hex.EncodeToString(value), nil
}
