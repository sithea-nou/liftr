// SPDX-License-Identifier: Apache-2.0

package application

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
)

const operatorRequestRepresentationVersion = 1

// OperatorMutationCommand carries the transport-neutral requirements shared
// by every M20 mutation. IfMatch is optional client concurrency assistance;
// the mutation always locks, reloads, rebuilds diagnostics, and re-runs the
// RecoveryPlanner regardless of whether it is present (ADR-0021).
type OperatorMutationCommand struct {
	Actor          identity.Principal
	IdempotencyKey string
	RequestID      string
	IfMatch        string
}

// OperatorMutationResult is the accepted scheduling response. Replay is an
// HTTP/telemetry request outcome only: the immutable OperatorAction identified
// here is never changed and no new audit or work row is created (ADR-0021,
// correction 1).
type OperatorMutationResult struct {
	Replay           bool
	Action           OperatorAuditAction
	TargetKind       identity.OperatorTargetKind
	TargetID         string
	OperatorActionID string
	CreatedWorkID    string
	SourceWorkKind   OutboxKind
}

type operatorMutationPlan struct {
	auditAction OperatorAuditAction
	permission  identity.Action
	target      identity.OperatorTarget
	actionID    string
	apply       func(context.Context, UnitOfWork, string) (string, error)
}

// TriggerOperationObservation schedules one fresh canonical Observe for the
// current durable execution of an existing Operation. It never submits,
// creates an Operation, retries, changes desired state or Generation,
// terminalizes manually, or evaluates M18 policy.
func (s *Service) TriggerOperationObservation(ctx context.Context, cmd OperatorMutationCommand, operationID domain.OperationID) (OperatorMutationResult, error) {
	actionID, err := newOperatorActionID()
	if err != nil {
		return OperatorMutationResult{}, err
	}
	return s.runOperatorMutation(ctx, cmd, operatorMutationPlan{
		auditAction: AuditTriggerObserve,
		permission:  identity.ActionOperatorObserveTrigger,
		target:      identity.OperatorTarget{Kind: identity.OperatorTargetOperation, ID: string(operationID)},
		actionID:    actionID,
		apply: func(ctx context.Context, tx UnitOfWork, ifMatch string) (string, error) {
			operation, err := tx.Operations().GetOperation(ctx, operationID)
			if err != nil {
				return "", err
			}
			attempts, err := tx.SubmissionAttempts().SummarizeSubmissionAttempts(ctx, operationID)
			if err != nil {
				return "", err
			}
			work, err := tx.Outbox().SummarizeWorkByOperation(ctx, operationID)
			if err != nil {
				return "", err
			}
			execution, err := tx.Executions().GetExecution(ctx, operationID)
			if err != nil {
				if errors.Is(err, ErrResourceNotFound) || errors.Is(err, ErrOperationNotFound) {
					assessment := PlanOperationObserve(OperationRecoverySnapshot{OperationState: operation.Operation.State()})
					return "", plannerError(assessment, "")
				}
				return "", err
			}
			execDiag := executionDiagnosticOf(execution)
			revision := operationRevision(operation, attempts, work, &execDiag, s.resolveRegistration(ctx, execution.ProvisionerRef))
			if !matchDiagnosticRevision(ifMatch, revision) {
				return "", ErrDiagnosticStale
			}
			assessment := PlanOperationObserve(OperationRecoverySnapshot{
				OperationState:        operation.Operation.State(),
				HasExecution:          true,
				Execution:             executionSummaryOf(execution),
				ActiveObserveWork:     work.HasActive(OutboxObserve),
				RegistrationAvailable: s.resolveRegistration(ctx, execution.ProvisionerRef),
			})
			if !assessmentAllows(assessment, ActionKindTriggerObserve) {
				return "", plannerError(assessment, work.ActiveID(OutboxObserve))
			}
			message := nextObserveMessage(&execution)
			if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
				return "", err
			}
			if err := tx.Outbox().Enqueue(ctx, message); err != nil {
				if errors.Is(err, ErrConcurrencyConflict) {
					return "", &RecoveryAlreadyActiveError{ExistingWorkID: work.ActiveID(OutboxObserve)}
				}
				return "", err
			}
			return message.ID, nil
		},
	})
}

// TriggerResourcePassiveObservation schedules one fresh PassiveObserve for a
// current non-Deleted Resource. It creates no Operation, does not submit, and
// does not change Generation.
func (s *Service) TriggerResourcePassiveObservation(ctx context.Context, cmd OperatorMutationCommand, resourceID domain.ResourceID) (OperatorMutationResult, error) {
	actionID, err := newOperatorActionID()
	if err != nil {
		return OperatorMutationResult{}, err
	}
	return s.runOperatorMutation(ctx, cmd, operatorMutationPlan{
		auditAction: AuditTriggerPassiveObserve,
		permission:  identity.ActionOperatorObserveTrigger,
		target:      identity.OperatorTarget{Kind: identity.OperatorTargetResource, ID: string(resourceID)},
		actionID:    actionID,
		apply: func(ctx context.Context, tx UnitOfWork, ifMatch string) (string, error) {
			record, err := tx.Resources().GetResource(ctx, resourceID)
			if err != nil {
				return "", err
			}
			active, activeFound, err := tx.Operations().ActiveForResource(ctx, resourceID)
			if err != nil {
				return "", err
			}
			latest, latestFound, err := tx.Operations().LatestForResource(ctx, resourceID)
			if err != nil {
				return "", err
			}
			work, err := tx.Outbox().SummarizeWorkByResource(ctx, resourceID)
			if err != nil {
				return "", err
			}
			var activeSummary, latestSummary *OperationRefSummary
			var revisionExecution *ExecutionDiagnostics
			if activeFound {
				value := operationRefOf(active.Operation)
				activeSummary = &value
				execution, executionErr := tx.Executions().GetExecution(ctx, active.Operation.ID())
				if executionErr == nil {
					value := executionDiagnosticOf(execution)
					revisionExecution = &value
				} else if !errors.Is(executionErr, ErrResourceNotFound) && !errors.Is(executionErr, ErrOperationNotFound) {
					return "", executionErr
				}
			}
			if latestFound {
				value := operationRefOf(latest.Operation)
				latestSummary = &value
			}
			var binding *StateIdentitySummary
			value, found, readErr := tx.OperatorDiagnostics().StateIdentity(ctx, resourceID)
			if readErr != nil {
				return "", readErr
			}
			if found {
				binding = &value
			}
			revision := computeResourceRevision(record.Version, record.Status.UpdatedAt(),
				record.Resource.Generation(), record.Status.ObservedGeneration(), record.Status.State(),
				activeSummary, latestSummary, binding, revisionExecution, work,
				s.resolveRegistration(ctx, record.ProvisionerRef))
			if !matchDiagnosticRevision(ifMatch, revision) {
				return "", ErrDiagnosticStale
			}
			assessment := PlanResourcePassiveObserve(ResourcePassiveObserveSnapshot{
				ResourceState:            record.Status.State(),
				ActiveOperation:          activeFound,
				ActivePassiveObserveWork: work.HasActive(OutboxPassiveObserve),
				RegistrationAvailable:    s.resolveRegistration(ctx, record.ProvisionerRef),
			})
			if !assessmentAllows(assessment, ActionKindTriggerPassiveObserve) {
				return "", plannerError(assessment, work.ActiveID(OutboxPassiveObserve))
			}
			message := operatorPassiveObserveMessage(actionID, resourceID, record.Version)
			if err := tx.Outbox().Enqueue(ctx, message); err != nil {
				if errors.Is(err, ErrConcurrencyConflict) {
					return "", &RecoveryAlreadyActiveError{ExistingWorkID: work.ActiveID(OutboxPassiveObserve)}
				}
				return "", err
			}
			return message.ID, nil
		},
	})
}

// RecoverDeadWork recovers one immutable Dead row by scheduling exactly one
// NEW canonical work item derived from CURRENT durable aggregate state. It
// never resurrects, updates, deletes, or clones the Dead row; never reuses an
// old lease/fence; and never blindly Dispatches.
func (s *Service) RecoverDeadWork(ctx context.Context, cmd OperatorMutationCommand, workID string) (OperatorMutationResult, error) {
	actionID, err := newOperatorActionID()
	if err != nil {
		return OperatorMutationResult{}, err
	}
	return s.runOperatorMutation(ctx, cmd, operatorMutationPlan{
		auditAction: AuditRecoverDeadWork,
		permission:  identity.ActionOperatorWorkRecover,
		target:      identity.OperatorTarget{Kind: identity.OperatorTargetWork, ID: workID},
		actionID:    actionID,
		apply: func(ctx context.Context, tx UnitOfWork, ifMatch string) (string, error) {
			dead, err := tx.Outbox().GetOutbox(ctx, workID)
			if err != nil {
				return "", err
			}
			if dead.State != OutboxDead {
				return "", &NotApplicableError{Reason: string(ReasonWorkNotDead)}
			}
			snapshot, _, equivalentID, execution, resource, operation, err := s.deadWorkSnapshot(ctx, tx, dead)
			if err != nil {
				return "", err
			}
			if !matchDiagnosticRevision(ifMatch, workRevision(dead, snapshot)) {
				return "", ErrDiagnosticStale
			}
			assessment := PlanDeadWorkRecovery(snapshot)
			if !assessmentAllows(assessment, ActionKindRecoverDeadWork) {
				return "", plannerError(assessment, equivalentID)
			}
			var created OutboxMessage
			switch dead.Kind {
			case OutboxDispatch, OutboxObserve:
				if execution == nil {
					return "", &UnsafeRecoveryError{Reason: string(ReasonExecutionRecordMissing)}
				}
				created = nextObserveMessage(execution)
				if err := tx.Executions().SaveExecution(ctx, *execution, execution.Version); err != nil {
					return "", err
				}
			case OutboxPassiveObserve:
				if resource == nil {
					return "", &UnsafeRecoveryError{Reason: string(ReasonManualInterventionRequired)}
				}
				created = operatorPassiveObserveMessage(actionID, resource.Resource.ID(), resource.Version)
			case OutboxDrive:
				if operation == nil {
					return "", &UnsafeRecoveryError{Reason: string(ReasonExecutionRecordMissing)}
				}
				created = operatorDriveMessage(actionID, operation.Operation.ID(), operation.Version)
			default:
				return "", &UnsafeRecoveryError{Reason: string(ReasonManualInterventionRequired)}
			}
			if created.Kind == OutboxDispatch {
				return "", fmt.Errorf("%w: dead work recovery must never create Dispatch", ErrInvalidApplicationCall)
			}
			if err := tx.Outbox().Enqueue(ctx, created); err != nil {
				if errors.Is(err, ErrConcurrencyConflict) {
					return "", &RecoveryAlreadyActiveError{ExistingWorkID: equivalentID}
				}
				return "", err
			}
			return created.ID, nil
		},
	})
}

// runOperatorMutation atomically resolves operator idempotency, reauthorizes,
// applies one transactionally revalidated scheduling decision, appends one
// immutable OperatorAction, binds the idempotency key, and commits. Any error
// rolls back work, audit, and idempotency together. Provider calls are
// structurally absent.
func (s *Service) runOperatorMutation(ctx context.Context, cmd OperatorMutationCommand, plan operatorMutationPlan) (OperatorMutationResult, error) {
	if cmd.Actor.ID == "" || cmd.Actor.Kind == "" || strings.TrimSpace(cmd.IdempotencyKey) == "" || len(cmd.IdempotencyKey) > 200 {
		return OperatorMutationResult{}, fmt.Errorf("%w: actor and bounded Idempotency-Key are required", ErrInvalidApplicationCall)
	}
	if strings.TrimSpace(cmd.RequestID) == "" {
		return OperatorMutationResult{}, fmt.Errorf("%w: request ID is required", ErrInvalidApplicationCall)
	}
	if err := s.authorizeOperator(ctx, cmd.Actor, plan.permission, plan.target); err != nil {
		return OperatorMutationResult{}, err
	}
	fingerprint := FingerprintOperatorRequest(plan.auditAction, plan.target.Kind, plan.target.ID, operatorRequestRepresentationVersion)
	scope := string(cmd.Actor.ID)
	result := OperatorMutationResult{
		Action: plan.auditAction, TargetKind: plan.target.Kind, TargetID: plan.target.ID,
	}
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		bound, err := tx.OperatorIdempotency().GetOperatorIdempotency(ctx, scope, cmd.IdempotencyKey)
		switch {
		case err == nil:
			if !bytes.Equal(bound.Fingerprint, fingerprint) {
				return ErrIdempotencyConflict
			}
			action, err := tx.OperatorActions().GetOperatorAction(ctx, bound.OperatorActionID)
			if err != nil {
				return err
			}
			result.Replay = true
			result.Action = action.Action
			result.TargetKind = action.TargetKind
			result.TargetID = action.TargetID
			result.OperatorActionID = action.ID
			result.CreatedWorkID = action.CreatedWorkID
			if action.SourceWorkID != "" {
				source, loadErr := tx.Outbox().GetOutbox(ctx, action.SourceWorkID)
				if loadErr != nil {
					return loadErr
				}
				result.SourceWorkKind = source.Kind
			}
			return nil
		case errors.Is(err, ErrOperatorIdempotencyNotFound):
			// Continue with a fresh admission while the repository's advisory
			// lock serializes every other request for this scoped key.
		default:
			return err
		}
		// Authorization is repeated inside the transaction. The decision is
		// independent of ETags and every diagnostic recommendation.
		if err := s.authorizeOperator(ctx, cmd.Actor, plan.permission, plan.target); err != nil {
			return err
		}
		createdWorkID, err := plan.apply(ctx, tx, cmd.IfMatch)
		if err != nil {
			return err
		}
		if createdWorkID == "" {
			return fmt.Errorf("%w: accepted operator mutation created no work", ErrInvalidApplicationCall)
		}
		action := OperatorActionRecord{
			ID:                plan.actionID,
			ActorPrincipalID:  cmd.Actor.ID,
			ActorKind:         cmd.Actor.Kind,
			Action:            plan.auditAction,
			TargetKind:        plan.target.Kind,
			TargetID:          plan.target.ID,
			CreatedWorkID:     createdWorkID,
			IdempotencyDigest: DigestOperatorIdempotencyScope(scope, cmd.IdempotencyKey),
			RequestID:         cmd.RequestID,
			CreatedAt:         s.clock().UTC(),
		}
		if plan.auditAction == AuditRecoverDeadWork {
			action.SourceWorkID = plan.target.ID
			source, loadErr := tx.Outbox().GetOutbox(ctx, plan.target.ID)
			if loadErr != nil {
				return loadErr
			}
			result.SourceWorkKind = source.Kind
		}
		if err := tx.OperatorActions().InsertOperatorAction(ctx, action); err != nil {
			return err
		}
		if err := tx.OperatorIdempotency().PutOperatorIdempotency(ctx, OperatorIdempotencyRecord{
			Scope: scope, Key: cmd.IdempotencyKey, Fingerprint: fingerprint, OperatorActionID: action.ID,
		}); err != nil {
			return err
		}
		result.OperatorActionID = action.ID
		result.CreatedWorkID = createdWorkID
		return nil
	})
	if err != nil {
		return OperatorMutationResult{}, err
	}
	return result, nil
}

func (s *Service) deadWorkSnapshot(ctx context.Context, tx UnitOfWork, dead OutboxMessage) (
	DeadWorkKindSnapshot, bool, string, *ProvisioningExecutionRecord, *ResourceRecord, *OperationRecord, error,
) {
	snapshot := DeadWorkKindSnapshot{Kind: dead.Kind, State: dead.State}
	if dead.OperationID != "" {
		snapshot.OperationTarget = true
		operation, err := tx.Operations().GetOperation(ctx, dead.OperationID)
		if err != nil {
			return snapshot, false, "", nil, nil, nil, err
		}
		targetTerminal := operationStateIsTerminal(operation.Operation.State())
		snapshot.OperationState = operation.Operation.State()
		snapshot.TargetVersion = operation.Version
		work, err := tx.Outbox().SummarizeWorkByOperation(ctx, dead.OperationID)
		if err != nil {
			return snapshot, false, "", nil, nil, nil, err
		}
		equivalentKind := dead.Kind
		if dead.Kind == OutboxDispatch {
			equivalentKind = OutboxObserve
		}
		equivalentID := work.ActiveID(equivalentKind)
		if dead.Kind == OutboxDrive {
			resource, resourceErr := tx.Resources().GetResource(ctx, operation.Operation.ResourceID())
			if resourceErr != nil {
				return snapshot, false, "", nil, nil, nil, resourceErr
			}
			snapshot.ActiveEquivalentWork = equivalentID != ""
			snapshot.RegistrationAvailable = s.resolveRegistration(ctx, resource.ProvisionerRef)
			return snapshot, targetTerminal, equivalentID, nil, nil, &operation, nil
		}
		execution, err := tx.Executions().GetExecution(ctx, dead.OperationID)
		if err != nil {
			if errors.Is(err, ErrResourceNotFound) || errors.Is(err, ErrOperationNotFound) {
				snapshot.RegistrationAvailable = false
				return snapshot, targetTerminal, "", nil, nil, &operation, nil
			}
			return snapshot, false, "", nil, nil, nil, err
		}
		snapshot.HasExecution = true
		snapshot.Execution = executionSummaryOf(execution)
		snapshot.ExecutionVersion = execution.Version
		if dead.Kind == OutboxDispatch {
			snapshot.Superseded = dead.AttemptNumber != execution.CurrentAttempt
		}
		if dead.Kind == OutboxObserve {
			snapshot.Superseded = dead.Sequence == 0 || dead.Sequence+1 != execution.NextObservation
		}
		snapshot.ActiveEquivalentWork = equivalentID != ""
		snapshot.RegistrationAvailable = s.resolveRegistration(ctx, execution.ProvisionerRef)
		return snapshot, targetTerminal, equivalentID, &execution, nil, &operation, nil
	}
	record, err := tx.Resources().GetResource(ctx, dead.ResourceID)
	if err != nil {
		return snapshot, false, "", nil, nil, nil, err
	}
	work, err := tx.Outbox().SummarizeWorkByResource(ctx, dead.ResourceID)
	if err != nil {
		return snapshot, false, "", nil, nil, nil, err
	}
	equivalentID := work.ActiveID(OutboxPassiveObserve)
	_, snapshot.ActiveOperation, err = tx.Operations().ActiveForResource(ctx, dead.ResourceID)
	if err != nil {
		return snapshot, false, "", nil, nil, nil, err
	}
	snapshot.ResourceState = record.Status.State()
	snapshot.TargetVersion = record.Version
	snapshot.ActiveEquivalentWork = equivalentID != ""
	snapshot.RegistrationAvailable = s.resolveRegistration(ctx, record.ProvisionerRef)
	return snapshot, record.Status.State() == domain.ResourceStateDeleted, equivalentID, nil, &record, nil, nil
}

func executionDiagnosticOf(execution ProvisioningExecutionRecord) ExecutionDiagnostics {
	return ExecutionDiagnostics{
		State: execution.State, Correlation: string(execution.Correlation),
		AcceptanceConfirmed: execution.AcceptanceConfirmed,
		HandlePresent:       execution.Handle != nil, IsOutputRecovery: execution.IsOutputRecovery(),
		OutputResolution:  execution.OutputResolution,
		OutputFailureKind: failureKindOf(execution.LastFailure),
		CurrentAttempt:    execution.CurrentAttempt, NextObservationSequence: execution.NextObservation,
	}
}

func operationRefOf(operation domain.Operation) OperationRefSummary {
	return OperationRefSummary{
		ID: operation.ID(), Capability: operation.Capability(), State: operation.State(),
		Phase: operation.Phase(), TargetGeneration: operation.TargetGeneration(),
	}
}

func nextObserveMessage(execution *ProvisioningExecutionRecord) OutboxMessage {
	sequence := execution.NextObservation
	if sequence == 0 {
		sequence = 1
	}
	execution.NextObservation = sequence + 1
	return ObserveMessage(execution.OperationID, sequence, execution.Version+1)
}

func operatorPassiveObserveMessage(actionID string, resourceID domain.ResourceID, expectedVersion uint64) OutboxMessage {
	id := "operator:" + actionID + ":passive-observe"
	sequence := expectedVersion
	if sequence == 0 {
		sequence = 1
	}
	return OutboxMessage{
		ID: id, Kind: OutboxPassiveObserve, ResourceID: resourceID,
		DedupeKey: id, ExpectedVersion: expectedVersion, Sequence: sequence,
		PayloadVersion: 1, Payload: []byte(`{}`), State: OutboxPending,
	}
}

func operatorDriveMessage(actionID string, operationID domain.OperationID, expectedVersion uint64) OutboxMessage {
	id := "operator:" + actionID + ":drive"
	return OutboxMessage{
		ID: id, Kind: OutboxDrive, OperationID: operationID,
		DedupeKey: id, ExpectedVersion: expectedVersion,
		PayloadVersion: 1, Payload: []byte(`{}`), State: OutboxPending,
	}
}

func assessmentAllows(assessment RecoveryAssessment, action OperatorActionKind) bool {
	for _, allowed := range assessment.AllowedActions {
		if allowed == action {
			return true
		}
	}
	return false
}

func plannerError(assessment RecoveryAssessment, existingWorkID string) error {
	if existingWorkID != "" {
		return &RecoveryAlreadyActiveError{ExistingWorkID: existingWorkID}
	}
	reason := ""
	if len(assessment.Reasons) != 0 {
		reason = string(assessment.Reasons[0])
	}
	switch assessment.State {
	case RecoveryManualInterventionNeeded, RecoveryUnsupportedRepair:
		return &UnsafeRecoveryError{Reason: reason}
	default:
		return &NotApplicableError{Reason: reason}
	}
}

func matchDiagnosticRevision(ifMatch, current string) bool {
	if ifMatch == "" {
		return true
	}
	return ifMatch == `"`+current+`"`
}

func newOperatorActionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("create operator action ID: %w", err)
	}
	return "opact_" + hex.EncodeToString(raw[:]), nil
}
