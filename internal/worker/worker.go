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
