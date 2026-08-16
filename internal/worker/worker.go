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
	Lifecycle    lifecycle.Engine
	Lease        time.Duration
	RetryBase    time.Duration
	MaxAttempts  int
	Clock        func() time.Time
}

func New(transactions application.TransactionRunner, resolver application.ProvisionerResolver) (*Worker, error) {
	if transactions == nil || resolver == nil {
		return nil, fmt.Errorf("worker dependencies are required")
	}
	return &Worker{Transactions: transactions, Resolver: resolver, Lease: time.Minute, RetryBase: time.Second, MaxAttempts: 10, Clock: time.Now}, nil
}

// RunOnce recovers one ambiguous expired Dispatch or processes one claimable
// message. The boolean reports whether durable work was found.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	if recovered, err := w.recoverExpiredDispatch(ctx); err != nil || recovered {
		return recovered, err
	}
	token, err := newToken()
	if err != nil {
		return false, err
	}
	var message application.OutboxMessage
	var found bool
	if err := w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
		var err error
		message, found, err = tx.Outbox().ClaimOutbox(ctx, token, w.Lease)
		return err
	}); err != nil || !found {
		return found, err
	}

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
		return true, nil
	}
	var ambiguous ambiguousDispatchError
	if errors.As(err, &ambiguous) {
		// Submit may already have reached the provider. Keep the lease intact so
		// expiry recovery moves the attempt through Unknown and Observe.
		return true, err
	}
	retryErr := w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
		return tx.Outbox().RetryOutbox(ctx, message.ID, message.LeaseToken, w.backoff(message.AttemptCount), err.Error(), w.MaxAttempts)
	})
	if retryErr != nil && !errors.Is(retryErr, application.ErrConcurrencyConflict) {
		return true, fmt.Errorf("process work: %w; reschedule: %v", err, retryErr)
	}
	return true, err
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
		operation, err := tx.Operations().GetOperation(ctx, message.OperationID)
		if err != nil {
			return err
		}
		if operation.Version != message.ExpectedVersion || operation.Operation.IsTerminal() {
			return tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "StaleDrive")
		}
		resource, err := tx.Resources().GetResource(ctx, operation.Operation.ResourceID())
		if err != nil {
			return err
		}
		next, ok := nextPhase(operation.Operation)
		if !ok {
			return tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "AlreadyDispatchable")
		}
		changedAt := operation.Operation.PhaseChangedAt().Add(time.Nanosecond)
		transition, err := w.Lifecycle.Advance(resource.Resource, resource.Status, operation.Operation, next, workerEventID(operation.Operation.ID(), string(next)), changedAt)
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

func (w *Worker) dispatch(ctx context.Context, message application.OutboxMessage) error {
	prepared, err := w.prepareDispatch(ctx, message)
	if err != nil {
		return err
	}
	submission, submitErr := prepared.provider.Submit(ctx, executionRequest(prepared.execution))
	if err := w.recordDispatch(ctx, message, prepared, submission, submitErr); err != nil {
		if recoveryErr := w.markDispatchUnknown(ctx, message, prepared, err); recoveryErr == nil {
			return nil
		}
		return ambiguousDispatchError{cause: err}
	}
	return nil
}

type ambiguousDispatchError struct{ cause error }

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
		attempt.State = application.SubmissionAttemptLeased
		if err := tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, attempt, application.SubmissionAttemptPending); err != nil {
			return err
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
		execution.Submission = &submission
		execution.LastObservation = &submission.Observation
		execution.Correlation = submission.Observation.Correlation
		if !validCorrelation(execution.Correlation) {
			execution.Correlation = provisioning.RequestCorrelationUnknown
		}
		if submission.Observation.Execution != nil && submission.Observation.Execution.Handle != nil {
			execution.Handle = submission.Observation.Execution.Handle
		}
		observedAt := w.observedAt(submission.Observation.ObservedAt)
		execution.LastObservedAt = observedAt
		backendExecution := submission.Observation.Execution
		terminalEvidence := backendExecution != nil && (backendExecution.State == provisioning.ExecutionStateSucceeded || backendExecution.State == provisioning.ExecutionStateFailed)
		validState := backendExecution != nil && validExecutionState(backendExecution.State)
		if (submitErr != nil && !terminalEvidence) || !validState || backendExecution.State == provisioning.ExecutionStateUnknown {
			execution.State = application.AttemptUnknown
			if submission.Observation.Correlation == provisioning.RequestCorrelationFound {
				execution.AcceptanceConfirmed = true
			}
			execution.LastFailure = failureFrom(submitErr, backendExecution)
			attempt.State = application.SubmissionAttemptUnknown
			attempt.Failure = execution.LastFailure
			if err := tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, attempt, application.SubmissionAttemptLeased); err != nil {
				return err
			}
			observe := scheduleObserve(&execution, w.RetryBase)
			if err := tx.Executions().SaveExecution(ctx, execution, prepared.version); err != nil {
				return err
			}
			if err := tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "AmbiguousSubmission"); err != nil {
				return err
			}
			return tx.Outbox().Enqueue(ctx, observe)
		}
		rejected := backendExecution.State == provisioning.ExecutionStateFailed && backendExecution.Failure != nil &&
			(backendExecution.Failure.Kind == provisioning.FailureInvalidRequest || backendExecution.Failure.Kind == provisioning.FailureUnsupported)
		attempt.ResolvedAt = observedAt
		if rejected {
			attempt.State = application.SubmissionAttemptRejected
			attempt.Failure = failureFrom(nil, backendExecution)
			execution.Correlation = provisioning.RequestCorrelationNotFound
			execution.AcceptanceConfirmed = false
		} else {
			attempt.State = application.SubmissionAttemptAccepted
			execution.Correlation = provisioning.RequestCorrelationFound
			execution.AcceptanceConfirmed = true
		}
		if err := tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, attempt, application.SubmissionAttemptLeased); err != nil {
			return err
		}
		switch backendExecution.State {
		case provisioning.ExecutionStateAccepted, provisioning.ExecutionStateRunning:
			execution.State = application.AttemptAccepted
			observe := scheduleObserve(&execution, w.RetryBase)
			if err := tx.Executions().SaveExecution(ctx, execution, prepared.version); err != nil {
				return err
			}
			if err := tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "Submitted"); err != nil {
				return err
			}
			return tx.Outbox().Enqueue(ctx, observe)
		case provisioning.ExecutionStateSucceeded:
			execution.State = application.AttemptSucceeded
			return w.finishOperation(ctx, tx, message, execution, prepared.version, true, "SubmissionSucceeded", "", observedAt, submission.Observation.Resource)
		case provisioning.ExecutionStateFailed:
			execution.State = application.AttemptFailed
			failure := failureFrom(nil, backendExecution)
			execution.LastFailure = failure
			return w.finishOperation(ctx, tx, message, execution, prepared.version, false, failure.Reason, failure.Message, observedAt, submission.Observation.Resource)
		default:
			return fmt.Errorf("invalid execution state %q", backendExecution.State)
		}
	})
}

func (w *Worker) observe(ctx context.Context, message application.OutboxMessage) error {
	var loaded application.ProvisioningExecutionRecord
	var provider provisioning.Provisioner
	if err := w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
		current, err := tx.Outbox().GetOutbox(ctx, message.ID)
		if err != nil {
			return err
		}
		if current.State != application.OutboxLeased || current.LeaseToken != message.LeaseToken {
			return application.ErrConcurrencyConflict
		}
		loaded, err = tx.Executions().GetExecution(ctx, message.OperationID)
		if err != nil {
			return err
		}
		if loaded.Version != message.ExpectedVersion || loaded.NextObservation != message.Sequence+1 {
			return application.ErrConcurrencyConflict
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
	observation, observeErr := provider.Observe(ctx, observationRequest(loaded))
	if observeErr != nil {
		return observeErr
	}
	if !validCorrelation(observation.Correlation) {
		return fmt.Errorf("invalid request correlation %q", observation.Correlation)
	}
	if observation.Correlation == provisioning.RequestCorrelationNotFound && observation.Execution != nil {
		return fmt.Errorf("contradictory observation reports NotFound with an execution")
	}
	return w.recordObservation(ctx, message, loaded, observation)
}

func (w *Worker) recordObservation(ctx context.Context, message application.OutboxMessage, loaded application.ProvisioningExecutionRecord, observation provisioning.ExecutionObservation) error {
	if observation.Correlation == provisioning.RequestCorrelationNotFound && observation.Execution != nil {
		return fmt.Errorf("contradictory observation reports NotFound with an execution")
	}
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
		if execution.Version != loaded.Version || execution.CurrentAttempt != loaded.CurrentAttempt || execution.NextObservation != message.Sequence+1 {
			return application.ErrConcurrencyConflict
		}
		observedAt := w.observedAt(observation.ObservedAt)
		execution.LastObservation = &observation
		execution.LastObservedAt = observedAt
		execution.Correlation = observation.Correlation
		if observation.Correlation == provisioning.RequestCorrelationNotFound && execution.State == application.AttemptUnknown && !execution.AcceptanceConfirmed {
			attempt, err := tx.SubmissionAttempts().GetSubmissionAttempt(ctx, execution.OperationID, execution.CurrentAttempt)
			if err != nil {
				return err
			}
			if attempt.State != application.SubmissionAttemptUnknown {
				return application.ErrConcurrencyConflict
			}
			attempt.State = application.SubmissionAttemptNotFound
			attempt.ResolvedAt = observedAt
			if err := tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, attempt, application.SubmissionAttemptUnknown); err != nil {
				return err
			}
			execution.CurrentAttempt++
			execution.State = application.AttemptPending
			dispatch := application.DispatchMessage(execution.OperationID, execution.CurrentAttempt, execution.Version+1)
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
		}
		if observation.Execution == nil {
			if observation.Correlation == provisioning.RequestCorrelationFound {
				execution.AcceptanceConfirmed = true
			}
			execution.State = application.AttemptUnknown
			next := scheduleObserve(&execution, w.RetryBase)
			if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
				return err
			}
			if err := tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "ObservedNoCurrentExecution"); err != nil {
				return err
			}
			return tx.Outbox().Enqueue(ctx, next)
		}
		if observation.Execution.Handle != nil {
			execution.Handle = observation.Execution.Handle
		}
		execution.AcceptanceConfirmed = true
		switch observation.Execution.State {
		case provisioning.ExecutionStateAccepted, provisioning.ExecutionStateRunning, provisioning.ExecutionStateUnknown:
			execution.State = application.AttemptAccepted
			if observation.Execution.State == provisioning.ExecutionStateUnknown {
				execution.State = application.AttemptUnknown
			}
			next := scheduleObserve(&execution, w.RetryBase)
			if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
				return err
			}
			if err := tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "ObservedNonterminal"); err != nil {
				return err
			}
			return tx.Outbox().Enqueue(ctx, next)
		case provisioning.ExecutionStateSucceeded:
			execution.State = application.AttemptSucceeded
			return w.finishOperation(ctx, tx, message, execution, execution.Version, true, "ObservationSucceeded", "", observedAt, observation.Resource)
		case provisioning.ExecutionStateFailed:
			execution.State = application.AttemptFailed
			failure := failureFrom(nil, observation.Execution)
			execution.LastFailure = failure
			return w.finishOperation(ctx, tx, message, execution, execution.Version, false, failure.Reason, failure.Message, observedAt, observation.Resource)
		default:
			return fmt.Errorf("invalid execution state %q", observation.Execution.State)
		}
	})
}

func (w *Worker) passiveObserve(ctx context.Context, message application.OutboxMessage) error {
	var loaded application.ResourceRecord
	stale := false
	if err := w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
		var err error
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
	observation, err := provider.Observe(ctx, provisioning.ObservationRequest{ResourceID: loaded.Resource.ID(), ResourceType: loaded.Resource.Type(), Spec: loaded.Resource.Spec(), TargetGeneration: loaded.Resource.Generation()})
	if err != nil {
		return err
	}
	return w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
		current, err := tx.Resources().GetResource(ctx, message.ResourceID)
		if err != nil {
			return err
		}
		if current.Version != loaded.Version {
			return application.ErrConcurrencyConflict
		}
		status, err := w.Lifecycle.ApplyObservation(current.Resource, current.Status, observation.Resource, w.observedAt(observation.ObservedAt))
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

func (w *Worker) finishOperation(ctx context.Context, tx application.UnitOfWork, message application.OutboxMessage, execution application.ProvisioningExecutionRecord, expectedExecutionVersion uint64, succeeded bool, reason, failureMessage string, at time.Time, facts domain.ObservedFacts) error {
	operation, err := tx.Operations().GetOperation(ctx, execution.OperationID)
	if err != nil {
		return err
	}
	resource, err := tx.Resources().GetResource(ctx, operation.Operation.ResourceID())
	if err != nil {
		return err
	}
	var result lifecycle.Result
	if succeeded {
		result, err = w.Lifecycle.Complete(resource.Resource, resource.Status, operation.Operation, workerEventID(operation.Operation.ID(), "succeeded"), at)
	} else {
		result, err = w.Lifecycle.Fail(resource.Resource, resource.Status, operation.Operation, reason, failureMessage, workerEventID(operation.Operation.ID(), "failed"), at)
	}
	if err != nil {
		return err
	}
	if facts.Presence != "" || facts.Readiness != "" || facts.Drift != "" {
		result.Status, err = w.Lifecycle.ApplyPostOperationObservation(resource.Resource, result.Status, facts, at)
		if err != nil {
			return err
		}
	}
	resource.Status = result.Status
	if err := tx.Resources().SaveResource(ctx, resource, resource.Version); err != nil {
		return err
	}
	if err := tx.Operations().SaveOperation(ctx, application.OperationRecord{Operation: result.Operation}, operation.Version); err != nil {
		return err
	}
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
		if execution.CurrentAttempt != message.AttemptNumber || execution.State != application.AttemptDispatching {
			return tx.Outbox().CompleteExpiredOutbox(ctx, message.ID, message.LeaseToken, "StaleExpiredDispatch")
		}
		attempt, err := tx.SubmissionAttempts().GetSubmissionAttempt(ctx, message.OperationID, message.AttemptNumber)
		if err != nil {
			return err
		}
		if attempt.State != application.SubmissionAttemptLeased {
			return application.ErrConcurrencyConflict
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
	return provisioning.ExecutionRequest{OperationID: execution.OperationID, ResourceID: execution.ResourceID, ResourceType: execution.ResourceType,
		Spec: execution.Spec, Capability: execution.Capability, TargetGeneration: execution.TargetGeneration}
}

func observationRequest(execution application.ProvisioningExecutionRecord) provisioning.ObservationRequest {
	return provisioning.ObservationRequest{OperationID: execution.OperationID, ResourceID: execution.ResourceID, ResourceType: execution.ResourceType,
		Spec: execution.Spec, TargetGeneration: execution.TargetGeneration, Handle: execution.Handle}
}

func failureFrom(err error, execution *provisioning.Execution) *provisioning.ExecutionFailure {
	if execution != nil && execution.Failure != nil {
		failure := *execution.Failure
		if failure.Reason == "" {
			failure.Reason = "ExecutionFailed"
		}
		return &failure
	}
	reason := "SubmissionUnknown"
	message := "submission outcome is ambiguous"
	if err != nil {
		message = err.Error()
	}
	return &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: reason, Message: message}
}

func validExecutionState(state provisioning.ExecutionState) bool {
	switch state {
	case provisioning.ExecutionStateAccepted, provisioning.ExecutionStateRunning, provisioning.ExecutionStateSucceeded, provisioning.ExecutionStateFailed, provisioning.ExecutionStateUnknown:
		return true
	default:
		return false
	}
}

func validCorrelation(correlation provisioning.RequestCorrelation) bool {
	switch correlation {
	case provisioning.RequestCorrelationFound, provisioning.RequestCorrelationNotFound, provisioning.RequestCorrelationUnknown:
		return true
	default:
		return false
	}
}

func workerEventID(operationID domain.OperationID, suffix string) domain.EventID {
	return domain.EventID("liftr-internal-" + string(operationID) + "-worker-" + suffix)
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

func newToken() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("create lease token: %w", err)
	}
	return hex.EncodeToString(value), nil
}
