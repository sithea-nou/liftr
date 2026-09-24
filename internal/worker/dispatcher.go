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
)

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
	case application.OutboxWakeDependents:
		err = w.wakeDependents(ctx, message)
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
	case application.OutboxWakeDependents:
		return WorkKindWakeDependents
	default:
		return "unknown"
	}
}

type failureClass int

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
