// SPDX-License-Identifier: Apache-2.0

// Package worker executes durable provisioner-neutral outbox work.
// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"fmt"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/lifecycle"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

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
		gatingUp := next == domain.OperationPhaseApplying && operation.Operation.Capability() != domain.CapabilityDelete
		var execution application.ProvisioningExecutionRecord
		var gate *application.DependencyEvaluation
		if gatingUp || next == domain.OperationPhaseDestroying {
			execution, err = tx.Executions().GetExecution(ctx, message.OperationID)
			if err != nil {
				return err
			}
		}
		if gatingUp && execution.CurrentAttempt == 0 && !execution.IsOutputRecovery() {
			// M21 pre-Submit dependency gate. The source Resource row is
			// already locked above; EvaluateDependencies locks the desired
			// target rows in ascending ID order and reads readiness facts
			// after those locks, so wait registration cannot lose a
			// concurrent target transition. The owner admission lock is
			// deliberately NOT taken: worker gating never serializes through
			// it (ADR-0022).
			evaluation, gated, evalErr := application.EvaluateDependencies(ctx, tx, w.Types, resource)
			if evalErr != nil {
				return evalErr
			}
			if gated {
				gate = &evaluation
				switch evaluation.Class {
				case application.DependencyReady:
					w.reportGateOutcome(GateResultReady)
				case application.DependencyWaiting:
					w.reportGateOutcome(GateResultWaiting)
				case application.DependencyTerminalFailure:
					w.reportGateOutcome(GateResultFailed)
				default:
					w.reportGateOutcome(GateResultInvalid)
				}
				if evaluation.Class != application.DependencyReady {
					return w.settleGateBlock(ctx, tx, message, resource, operation, execution, evaluation, changedAt)
				}
			}
		}
		transition, err := w.Lifecycle.Advance(resource.Resource, resource.Status, operation.Operation, next, application.InternalEventID(operation.Operation.ID(), application.InternalTransitionLabel(next)), changedAt)
		if err != nil {
			return err
		}
		resource.Status = transition.Status
		if gate != nil {
			// All dependencies were READY at gate time: record the satisfied
			// condition and release any obsolete wait rows in the same commit.
			status, condErr := w.Lifecycle.SetDependencyCondition(resource.Status, domain.ConditionStatusTrue, lifecycle.ReasonDependenciesSatisfied, "", changedAt)
			if condErr != nil {
				return condErr
			}
			resource.Status = status
			if err := tx.DependencyWaits().DeleteDependencyWaitsForOperation(ctx, message.OperationID); err != nil {
				return err
			}
		}
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
