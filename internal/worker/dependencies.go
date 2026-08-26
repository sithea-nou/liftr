// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"context"
	"errors"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/lifecycle"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

// wakeFanoutBatch bounds one keyset page of the wake fan-out. Inbound
// dependency fan-out is intentionally unbounded, so the wake worker pages
// through waiters with LIMIT + wait_seq cursors — never OFFSET, never one
// unbounded load.
const wakeFanoutBatch = 256

// settleGateBlock handles a dependency gate that refused Submit. WAIT
// registers exact durable waits and leaves the Operation nonterminal;
// terminal dependency failure and invalid targets fail the Operation
// pre-submission with curated Liftr-owned reasons — never a provider failure,
// never an ExecutionHandle, never a submission attempt.
func (w *Worker) settleGateBlock(ctx context.Context, tx application.UnitOfWork, message application.OutboxMessage, resource application.ResourceRecord, operation application.OperationRecord, execution application.ProvisioningExecutionRecord, evaluation application.DependencyEvaluation, changedAt time.Time) error {
	at := changedAt
	var conditionReason string
	switch evaluation.Class {
	case application.DependencyWaiting:
		conditionReason = lifecycle.ReasonWaitingForDependencies
	case application.DependencyTerminalFailure:
		conditionReason = lifecycle.ReasonDependencyFailed
	default:
		conditionReason = lifecycle.ReasonDependencyInvalid
	}
	status, err := w.Lifecycle.SetDependencyCondition(resource.Status, domain.ConditionStatusFalse, conditionReason, "", at)
	if err != nil {
		return err
	}
	resource.Status = status

	if evaluation.Class == application.DependencyWaiting {
		targets := make(map[domain.ResourceID]uint64, len(evaluation.Blocking))
		for _, target := range evaluation.Blocking {
			version := evaluation.TargetVersions[target]
			if version == 0 {
				version = 1
			}
			targets[target] = version
		}
		// The WAIT branch settles this Drive without any lifecycle transition.
		// Drive dedupe keys are derived from the Operation record version, and
		// that key is now permanently spent, so the wait registration must
		// bump the Operation row version atomically: the wake worker's fresh
		// canonical Drive then mints an UNUSED key instead of colliding with
		// completed history.
		bumped := application.OperationRecord{Operation: operation.Operation, Version: operation.Version + 1}
		if err := tx.Operations().SaveOperation(ctx, bumped, operation.Version); err != nil {
			return err
		}
		if err := tx.DependencyWaits().RegisterDependencyWaits(ctx, message.OperationID, bumped.Version, targets); err != nil {
			return err
		}
		if err := tx.Resources().SaveResource(ctx, resource, resource.Version); err != nil {
			return err
		}
		return tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "WaitingForDependencies")
	}

	failureReason := string(lifecycle.ReasonDependencyFailed)
	failureMessage := "a referenced dependency failed and has no active recovery"
	if evaluation.Class == application.DependencyInvalid {
		failureReason = string(lifecycle.ReasonDependencyInvalid)
		failureMessage = "a referenced dependency is unavailable or being deleted"
	}
	result, err := application.BuildFinishEvidence(w.Lifecycle, operation.Operation, resource.Resource, resource.Status,
		application.Finish{Succeeded: false, Reason: failureReason, Message: failureMessage}, at)
	if err != nil {
		return err
	}
	resource.Status = result.Status
	execution.State = application.AttemptFailed
	execution.Correlation = provisioning.RequestCorrelationUnknown
	execution.AcceptanceConfirmed = false
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
	if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
		return err
	}
	if err := tx.DependencyWaits().DeleteDependencyWaitsForOperation(ctx, message.OperationID); err != nil {
		return err
	}
	return tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "DependencyGateRejected")
}

// wakeDependents processes one versioned wake. It means ONLY "something
// gate-relevant changed; cause blocked dependent Operations to re-evaluate":
// it validates waiter identity, schedules fresh canonical Drives, and never
// decides readiness, never submits, never duplicates the dependency
// classification algorithm (ADR-0022).
//
// Crash-safety: fan-out batches run before finalization; a crash repeats
// earlier batches, which is acceptable because Drive enqueue is deduplicated.
// Long fan-outs renew their lease every batch. The final transaction performs
// the version handshake THROUGH THE TARGET ROW LOCK: gate-relevant target
// transitions take the same lock, so either they committed first (this wake
// observes the newer version and schedules the follow-up) or they wait (their
// own conditional enqueue then coalesces or lands cleanly).
func (w *Worker) wakeDependents(ctx context.Context, message application.OutboxMessage) error {
	cursor := uint64(0)
	for {
		more := false
		err := w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
			current, err := tx.Outbox().GetOutbox(ctx, message.ID)
			if err != nil {
				return err
			}
			if current.State != application.OutboxLeased || current.LeaseToken != message.LeaseToken {
				return application.ErrConcurrencyConflict
			}
			waits, next, err := tx.DependencyWaits().PageDependencyWaitersByTarget(ctx, message.ResourceID, cursor, wakeFanoutBatch)
			if err != nil {
				return err
			}
			// Deterministic per-batch locking order: ascending OperationID.
			sortWaitsByID(waits)
			for _, wait := range waits {
				if err := w.revalidateWaiter(ctx, tx, wait); err != nil {
					return err
				}
			}
			if err := tx.Outbox().RenewOutbox(ctx, message.ID, message.LeaseToken, w.Lease); err != nil {
				return err
			}
			cursor = next
			more = next != 0
			return nil
		})
		if err != nil {
			return err
		}
		if !more {
			break
		}
	}
	return w.Transactions.Within(ctx, func(tx application.UnitOfWork) error {
		current, err := tx.Outbox().GetOutbox(ctx, message.ID)
		if err != nil {
			return err
		}
		if current.State != application.OutboxLeased || current.LeaseToken != message.LeaseToken {
			return application.ErrConcurrencyConflict
		}
		resource, err := tx.Resources().GetResource(ctx, message.ResourceID)
		if err != nil {
			return err
		}
		if err := tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "Woke"); err != nil {
			return err
		}
		// Version handshake: terminalize THIS wake before inserting the
		// follow-up so the one-active-per-target constraint admits it.
		if resource.Version != message.ExpectedVersion {
			if err := tx.Outbox().EnqueueWakeDependents(ctx, application.WakeDependentsMessage(message.ResourceID, resource.Version)); err != nil {
				return err
			}
			w.reportWakeOutcome(WakeResultProcessed)
			return nil
		}
		w.reportWakeOutcome(WakeResultProcessed)
		return nil
	})
}

// revalidateWaiter checks that the bound Operation is still exactly the one
// that registered, is nonterminal, and has not yet been dispatched; anything
// else makes the wait row obsolete and removes it. Valid waiters receive one
// fresh canonical Drive for their CURRENT operation version; duplicate or
// conflicting Drives coalesce silently because the one-active-per-operation
// index plus dedupe keys guarantee a single owner of the progression.
func (w *Worker) revalidateWaiter(ctx context.Context, tx application.UnitOfWork, wait application.DependencyWait) error {
	operation, err := tx.Operations().GetOperation(ctx, wait.OperationID)
	if errors.Is(err, application.ErrResourceNotFound) || errors.Is(err, application.ErrOperationNotFound) {
		return tx.DependencyWaits().DeleteDependencyWaitsForOperation(ctx, wait.OperationID)
	}
	if err != nil {
		return err
	}
	stale := operation.Operation.IsTerminal() || operation.Version != wait.OperationVersion
	if !stale {
		execution, execErr := tx.Executions().LookupExecution(ctx, wait.OperationID)
		switch {
		case execErr == nil && (execution.CurrentAttempt != 0 || execution.IsOutputRecovery()):
			stale = true
		case execErr != nil && !errors.Is(execErr, application.ErrResourceNotFound):
			return execErr
		}
	}
	if stale {
		return tx.DependencyWaits().DeleteDependencyWaitsForOperation(ctx, wait.OperationID)
	}
	if err := tx.Outbox().Enqueue(ctx, application.DriveMessage(wait.OperationID, operation.Version)); err != nil {
		if errors.Is(err, application.ErrConcurrencyConflict) {
			// An active Drive already owns this Operation's progression; it
			// will reach the gate and settle the wait itself.
			return nil
		}
		return err
	}
	return nil
}

func sortWaitsByID(waits []application.DependencyWait) {
	for i := 1; i < len(waits); i++ {
		for j := i; j > 0 && waits[j].OperationID < waits[j-1].OperationID; j-- {
			waits[j], waits[j-1] = waits[j-1], waits[j]
		}
	}
}

// applyTerminalSideEffects centralizes the M21 side effects of every durable
// terminal Operation transition: obsolete wait rows disappear, dependent
// Operations are woken when the Resource's classification may have changed,
// and a successful delete releases BOTH reference sets atomically with the
// Deleted tombstone. It must run inside the transaction holding the Resource
// row; savedVersion is the persisted record version AFTER the status save.
// Gate-relevance is deliberate: success/failure of the target's own Operation
// flips WAIT<->READY or WAIT<->TERMINAL_DEPENDENCY_FAILURE classification, so
// blocked dependents must re-evaluate.
func applyTerminalSideEffects(ctx context.Context, tx application.UnitOfWork, operation domain.Operation, resourceID domain.ResourceID, savedVersion uint64) error {
	if err := tx.DependencyWaits().DeleteDependencyWaitsForOperation(ctx, operation.ID()); err != nil {
		return err
	}
	if operation.Capability() == domain.CapabilityDelete && operation.State() == domain.OperationStateSucceeded {
		// Source deletion releases desired AND applied references in the same
		// commit as the Deleted transition (ADR-0022 protective union rule).
		if err := tx.References().DeleteReferencesForSource(ctx, resourceID); err != nil {
			return err
		}
	}
	return application.EnqueueWakeDependentsIfWaited(ctx, tx, resourceID, savedVersion)
}
