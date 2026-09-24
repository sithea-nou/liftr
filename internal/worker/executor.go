// SPDX-License-Identifier: Apache-2.0

// Package worker executes durable provisioner-neutral outbox work.
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/lifecycle"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

func (w *Worker) SchedulePassiveObservation(ctx context.Context, resourceID domain.ResourceID, sequence, expectedVersion uint64) error {
	return w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
		return tx.Outbox().Enqueue(ctx, application.PassiveObserveMessage(resourceID, sequence, expectedVersion))
	})
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
		if err := tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "PassivelyObserved"); err != nil {
			return err
		}
		// A passive fact that flips readiness or presence is gate-relevant:
		// blocked dependents must re-evaluate. Serialized through this same
		// Resource row lock, so wait registration cannot race it.
		return application.EnqueueWakeDependentsIfWaited(ctx, tx, message.ResourceID, current.Version+1)
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
	if err := applyTerminalSideEffects(ctx, tx, result.Operation, resource.Resource.ID(), resource.Version+1); err != nil {
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
		// Output-postcondition rejection must NOT advance applied references:
		// the protective union keeps covering the old applied set while the
		// desired set stands. Dependents are woken because the target's own
		// active reconciliation just became a terminal failure.
		if err := applyTerminalSideEffects(ctx, tx, result.Operation, resource.Resource.ID(), resource.Version+1); err != nil {
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
	// M21 applied-reference advancement: ONLY this durable terminal-success
	// transaction — which proves create/update convergence INCLUDING required
	// output postconditions — may move the applied set. ObservedGeneration is
	// deliberately not consulted; it advanced at Request admission.
	if execution.Capability == domain.CapabilityCreate || execution.Capability == domain.CapabilityUpdate {
		if operation.Operation.TargetGeneration() == resource.Resource.Generation() {
			desired, refErr := tx.References().DesiredReferences(ctx, resource.Resource.ID())
			if refErr != nil {
				return refErr
			}
			applied := make([]application.ReferenceEdge, 0, len(desired))
			for _, edge := range desired {
				if edge.Generation != operation.Operation.TargetGeneration() {
					return fmt.Errorf("%w: desired reference generation %d does not match converged generation %d",
						application.ErrReferenceInvariant, edge.Generation, operation.Operation.TargetGeneration())
				}
				applied = append(applied, edge)
			}
			if err := tx.References().AdvanceAppliedReferences(ctx, resource.Resource.ID(), operation.Operation.TargetGeneration(), applied); err != nil {
				return err
			}
		}
	}
	if err := applyTerminalSideEffects(ctx, tx, result.Operation, resource.Resource.ID(), resource.Version+1); err != nil {
		return err
	}
	return tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "TerminalExecution")
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
