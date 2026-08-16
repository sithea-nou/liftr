// SPDX-License-Identifier: Apache-2.0

// Package application coordinates Liftr domain, lifecycle, provisioning, and
// persistence ports. It does not implement persistence or transport.
package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/lifecycle"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

var (
	ErrResourceNotFound       = errors.New("resource not found")
	ErrOperationNotFound      = errors.New("operation not found")
	ErrResourceTypeNotFound   = errors.New("resource type not found")
	ErrProvisionerNotFound    = errors.New("provisioner not found")
	ErrIdempotencyConflict    = errors.New("idempotency key conflict")
	ErrIdempotencyNotFound    = errors.New("idempotency record not found")
	ErrConcurrencyConflict    = errors.New("concurrency conflict")
	ErrInvalidApplicationCall = errors.New("invalid application call")
)

// ProvisionerRef is private platform metadata. It is never part of ResourceSpec
// or the developer-facing Resource contract.
type ProvisionerRef string

func NewProvisionerRef(value string) (ProvisionerRef, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("provisioner reference is required")
	}
	return ProvisionerRef(value), nil
}

type ResourceRecord struct {
	Resource       domain.Resource
	Status         domain.ResourceStatus
	ProvisionerRef ProvisionerRef
	Version        uint64
}

type ProvisioningAttemptState string

const (
	AttemptPending     ProvisioningAttemptState = "Pending"
	AttemptDispatching ProvisioningAttemptState = "Dispatching"
	AttemptAccepted    ProvisioningAttemptState = "Accepted"
	AttemptSucceeded   ProvisioningAttemptState = "Succeeded"
	AttemptFailed      ProvisioningAttemptState = "Failed"
	AttemptUnknown     ProvisioningAttemptState = "Unknown"
)

type ProvisioningExecutionRecord struct {
	OperationID         domain.OperationID
	ProvisionerRef      ProvisionerRef
	ResourceID          domain.ResourceID
	ResourceType        domain.ResourceTypeRef
	Capability          domain.Capability
	TargetGeneration    uint64
	Spec                domain.ResourceSpec
	Handle              *provisioning.ExecutionHandle
	State               ProvisioningAttemptState
	Submission          *provisioning.Submission
	AcceptanceConfirmed bool
	LastObservation     *provisioning.ExecutionObservation
	LastObservedAt      time.Time
	LastFailure         *provisioning.ExecutionFailure
	Correlation         provisioning.RequestCorrelation
	CurrentAttempt      uint64
	NextObservation     uint64
	Version             uint64
}

type IdempotencyRecord struct {
	Key         string
	Fingerprint string
	CommandKind string
	ResourceID  domain.ResourceID
	OperationID domain.OperationID
}

type ResourceTypeCatalog interface {
	Get(context.Context, domain.ResourceTypeRef) (domain.ResourceType, error)
}

type ProvisionerSelector interface {
	Select(context.Context, domain.ResourceTypeRef, domain.Capability) (ProvisionerRef, error)
}

type ProvisionerResolver interface {
	Resolve(context.Context, ProvisionerRef) (provisioning.Provisioner, error)
}

type ResourceRepository interface {
	GetResource(context.Context, domain.ResourceID) (ResourceRecord, error)
	CreateResource(context.Context, ResourceRecord) error
	SaveResource(context.Context, ResourceRecord, uint64) error
}

type OperationRecord struct {
	Operation domain.Operation
	Version   uint64
}

type OperationRepository interface {
	GetOperation(context.Context, domain.OperationID) (OperationRecord, error)
	ActiveForResource(context.Context, domain.ResourceID) (OperationRecord, bool, error)
	CreateOperation(context.Context, OperationRecord) error
	SaveOperation(context.Context, OperationRecord, uint64) error
}

type EventRepository interface {
	Append(context.Context, domain.Event) error
}

type ExecutionRepository interface {
	GetExecution(context.Context, domain.OperationID) (ProvisioningExecutionRecord, error)
	CreateExecution(context.Context, ProvisioningExecutionRecord) error
	SaveExecution(context.Context, ProvisioningExecutionRecord, uint64) error
}

type IdempotencyRepository interface {
	GetIdempotency(context.Context, string) (IdempotencyRecord, error)
	PutIdempotency(context.Context, IdempotencyRecord) error
}

type UnitOfWork interface {
	Resources() ResourceRepository
	Operations() OperationRepository
	Events() EventRepository
	Executions() ExecutionRepository
	Idempotency() IdempotencyRepository
	SubmissionAttempts() SubmissionAttemptRepository
	Outbox() OutboxRepository
}

type TransactionRunner interface {
	Within(context.Context, func(UnitOfWork) error) error
}

type Service struct {
	Types        ResourceTypeCatalog
	Selector     ProvisionerSelector
	Resolver     ProvisionerResolver
	Transactions TransactionRunner
	Lifecycle    lifecycle.Engine
	eager        bool
}

// EnableEagerExecutionForTesting preserves the Milestone 4 synchronous test
// harness. Production services must use the durable outbox worker.
func (s *Service) EnableEagerExecutionForTesting() { s.eager = true }

func NewService(types ResourceTypeCatalog, selector ProvisionerSelector, resolver ProvisionerResolver, transactions TransactionRunner) (*Service, error) {
	if isNilInterface(types) || isNilInterface(selector) || isNilInterface(resolver) || isNilInterface(transactions) {
		return nil, fmt.Errorf("%w: application dependencies are required", ErrInvalidApplicationCall)
	}
	return &Service{Types: types, Selector: selector, Resolver: resolver, Transactions: transactions}, nil
}

type CreateResourceCommand struct {
	ID             domain.ResourceID
	Type           domain.ResourceTypeRef
	Owner          domain.OwnerRef
	Spec           domain.ResourceSpec
	OperationID    domain.OperationID
	EventID        domain.EventID
	RequestedAt    time.Time
	IdempotencyKey string
	Fingerprint    string
}

type UpdateResourceCommand struct {
	ID                 domain.ResourceID
	ExpectedGeneration uint64
	Spec               domain.ResourceSpec
	OperationID        domain.OperationID
	EventID            domain.EventID
	RequestedAt        time.Time
	IdempotencyKey     string
	Fingerprint        string
}

type DeleteResourceCommand struct {
	ID                 domain.ResourceID
	ExpectedGeneration uint64
	OperationID        domain.OperationID
	EventID            domain.EventID
	RequestedAt        time.Time
	IdempotencyKey     string
	Fingerprint        string
}

type AdvanceOperationCommand struct {
	OperationID domain.OperationID
	Phase       domain.OperationPhase
	EventID     domain.EventID
	ChangedAt   time.Time
}

type ObserveOperationCommand struct {
	OperationID domain.OperationID
	ObservedAt  time.Time
}

type ObserveResourceCommand struct {
	ID         domain.ResourceID
	ObservedAt time.Time
}

type RetryOperationCommand struct {
	OperationID    domain.OperationID
	NewOperationID domain.OperationID
	EventID        domain.EventID
	RequestedAt    time.Time
	IdempotencyKey string
	Fingerprint    string
}

type Result struct {
	Resource  ResourceRecord
	Operation domain.Operation
	Execution *ProvisioningExecutionRecord
	Event     *domain.Event
	Replay    bool
}

func (s *Service) CreateResource(ctx context.Context, cmd CreateResourceCommand) (Result, error) {
	if !s.eager {
		return s.AdmitCreateResource(ctx, cmd)
	}
	if err := validateIdempotency(cmd.IdempotencyKey, cmd.Fingerprint); err != nil {
		return Result{}, err
	}
	if err := validateExternalEventID(cmd.EventID); err != nil {
		return Result{}, err
	}
	if replay, found, err := s.replay(ctx, cmd.IdempotencyKey, cmd.Fingerprint, string(domain.CapabilityCreate)); err != nil {
		return Result{}, err
	} else if found {
		return replay, nil
	}
	result, err := s.persistCreateRequest(ctx, cmd)
	if err != nil {
		return Result{}, err
	}
	if result.Replay {
		return result, nil
	}
	return s.drive(ctx, result.Operation.ID())
}

// AdmitCreateResource durably records a create request without executing
// provider work. A worker advances the resulting outbox message.
func (s *Service) AdmitCreateResource(ctx context.Context, cmd CreateResourceCommand) (Result, error) {
	if err := validateIdempotency(cmd.IdempotencyKey, cmd.Fingerprint); err != nil {
		return Result{}, err
	}
	if err := validateExternalEventID(cmd.EventID); err != nil {
		return Result{}, err
	}
	if replay, found, err := s.replay(ctx, cmd.IdempotencyKey, cmd.Fingerprint, string(domain.CapabilityCreate)); err != nil || found {
		return replay, err
	}
	return s.persistCreateRequest(ctx, cmd)
}

func (s *Service) UpdateResource(ctx context.Context, cmd UpdateResourceCommand) (Result, error) {
	if !s.eager {
		return s.AdmitUpdateResource(ctx, cmd)
	}
	if err := validateIdempotency(cmd.IdempotencyKey, cmd.Fingerprint); err != nil {
		return Result{}, err
	}
	if err := validateExternalEventID(cmd.EventID); err != nil {
		return Result{}, err
	}
	if replay, found, err := s.replay(ctx, cmd.IdempotencyKey, cmd.Fingerprint, string(domain.CapabilityUpdate)); err != nil {
		return Result{}, err
	} else if found {
		return replay, nil
	}
	result, err := s.persistExistingRequest(ctx, existingRequest{
		id: cmd.ID, expectedGeneration: cmd.ExpectedGeneration, spec: &cmd.Spec,
		capability: domain.CapabilityUpdate, operationID: cmd.OperationID, eventID: cmd.EventID,
		requestedAt: cmd.RequestedAt, idempotencyKey: cmd.IdempotencyKey, fingerprint: cmd.Fingerprint,
	})
	if err != nil {
		return Result{}, err
	}
	if result.Replay {
		return result, nil
	}
	return s.drive(ctx, result.Operation.ID())
}

func (s *Service) AdmitUpdateResource(ctx context.Context, cmd UpdateResourceCommand) (Result, error) {
	if err := validateIdempotency(cmd.IdempotencyKey, cmd.Fingerprint); err != nil {
		return Result{}, err
	}
	if err := validateExternalEventID(cmd.EventID); err != nil {
		return Result{}, err
	}
	if replay, found, err := s.replay(ctx, cmd.IdempotencyKey, cmd.Fingerprint, string(domain.CapabilityUpdate)); err != nil || found {
		return replay, err
	}
	return s.persistExistingRequest(ctx, existingRequest{id: cmd.ID, expectedGeneration: cmd.ExpectedGeneration, spec: &cmd.Spec, capability: domain.CapabilityUpdate, operationID: cmd.OperationID, eventID: cmd.EventID, requestedAt: cmd.RequestedAt, idempotencyKey: cmd.IdempotencyKey, fingerprint: cmd.Fingerprint})
}

func (s *Service) DeleteResource(ctx context.Context, cmd DeleteResourceCommand) (Result, error) {
	if !s.eager {
		return s.AdmitDeleteResource(ctx, cmd)
	}
	if err := validateIdempotency(cmd.IdempotencyKey, cmd.Fingerprint); err != nil {
		return Result{}, err
	}
	if err := validateExternalEventID(cmd.EventID); err != nil {
		return Result{}, err
	}
	if replay, found, err := s.replay(ctx, cmd.IdempotencyKey, cmd.Fingerprint, string(domain.CapabilityDelete)); err != nil {
		return Result{}, err
	} else if found {
		return replay, nil
	}
	result, err := s.persistExistingRequest(ctx, existingRequest{
		id: cmd.ID, expectedGeneration: cmd.ExpectedGeneration,
		capability: domain.CapabilityDelete, operationID: cmd.OperationID, eventID: cmd.EventID,
		requestedAt: cmd.RequestedAt, idempotencyKey: cmd.IdempotencyKey, fingerprint: cmd.Fingerprint,
	})
	if err != nil {
		return Result{}, err
	}
	if result.Replay {
		return result, nil
	}
	return s.drive(ctx, result.Operation.ID())
}

func (s *Service) AdmitDeleteResource(ctx context.Context, cmd DeleteResourceCommand) (Result, error) {
	if err := validateIdempotency(cmd.IdempotencyKey, cmd.Fingerprint); err != nil {
		return Result{}, err
	}
	if err := validateExternalEventID(cmd.EventID); err != nil {
		return Result{}, err
	}
	if replay, found, err := s.replay(ctx, cmd.IdempotencyKey, cmd.Fingerprint, string(domain.CapabilityDelete)); err != nil || found {
		return replay, err
	}
	return s.persistExistingRequest(ctx, existingRequest{id: cmd.ID, expectedGeneration: cmd.ExpectedGeneration, capability: domain.CapabilityDelete, operationID: cmd.OperationID, eventID: cmd.EventID, requestedAt: cmd.RequestedAt, idempotencyKey: cmd.IdempotencyKey, fingerprint: cmd.Fingerprint})
}

func (s *Service) AdvanceOperation(ctx context.Context, cmd AdvanceOperationCommand) (Result, error) {
	if !s.eager {
		return Result{}, fmt.Errorf("%w: direct operation advancement is disabled; use the durable worker", ErrInvalidApplicationCall)
	}
	if err := validateExternalEventID(cmd.EventID); err != nil {
		return Result{}, err
	}
	return s.advanceOperation(ctx, cmd)
}

func (s *Service) advanceOperation(ctx context.Context, cmd AdvanceOperationCommand) (Result, error) {
	var result Result
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		opRecord, err := tx.Operations().GetOperation(ctx, cmd.OperationID)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrOperationNotFound, err)
		}
		resourceRecord, err := tx.Resources().GetResource(ctx, opRecord.Operation.ResourceID())
		if err != nil {
			return fmt.Errorf("%w: %v", ErrResourceNotFound, err)
		}
		transition, err := s.Lifecycle.Advance(resourceRecord.Resource, resourceRecord.Status, opRecord.Operation, cmd.Phase, cmd.EventID, cmd.ChangedAt)
		if err != nil {
			return err
		}
		resourceRecord.Status = transition.Status
		if err := tx.Resources().SaveResource(ctx, resourceRecord, resourceRecord.Version); err != nil {
			return err
		}
		resourceRecord.Version++
		if err := tx.Operations().SaveOperation(ctx, OperationRecord{Operation: transition.Operation}, opRecord.Version); err != nil {
			return err
		}
		if err := tx.Events().Append(ctx, transition.Event); err != nil {
			return err
		}
		event := transition.Event
		result = Result{Resource: resourceRecord, Operation: transition.Operation, Event: &event}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

func (s *Service) ObserveResource(ctx context.Context, cmd ObserveResourceCommand) (Result, error) {
	if !s.eager {
		return s.schedulePassiveObservation(ctx, cmd.ID)
	}
	record, err := s.loadResource(ctx, cmd.ID)
	if err != nil {
		return Result{}, err
	}
	provider, err := s.Resolver.Resolve(ctx, record.ProvisionerRef)
	if err != nil || isNilInterface(provider) {
		return Result{}, fmt.Errorf("%w: %v", ErrProvisionerNotFound, err)
	}
	observation, err := provider.Observe(ctx, provisioning.ObservationRequest{ResourceID: record.Resource.ID(), ResourceType: record.Resource.Type(), Spec: record.Resource.Spec(), TargetGeneration: record.Resource.Generation()})
	if err != nil {
		return Result{}, err
	}
	var result Result
	err = s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		current, loadErr := tx.Resources().GetResource(ctx, cmd.ID)
		if loadErr != nil {
			return fmt.Errorf("%w: %v", ErrResourceNotFound, loadErr)
		}
		if current.Version != record.Version {
			return ErrConcurrencyConflict
		}
		status, applyErr := s.Lifecycle.ApplyObservation(current.Resource, current.Status, observation.Resource, observedAt(observation.ObservedAt, cmd.ObservedAt))
		if applyErr != nil {
			return applyErr
		}
		current.Status = status
		if saveErr := tx.Resources().SaveResource(ctx, current, current.Version); saveErr != nil {
			return saveErr
		}
		current.Version++
		result = Result{Resource: current}
		return nil
	})
	return result, err
}

func (s *Service) schedulePassiveObservation(ctx context.Context, id domain.ResourceID) (Result, error) {
	var result Result
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		record, err := tx.Resources().GetResource(ctx, id)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrResourceNotFound, err)
		}
		message := PassiveObserveMessage(id, record.Version, record.Version)
		if err := tx.Outbox().Enqueue(ctx, message); err != nil {
			return err
		}
		result = Result{Resource: record}
		return nil
	})
	return result, err
}

func (s *Service) ObserveOperation(ctx context.Context, cmd ObserveOperationCommand) (Result, error) {
	if !s.eager {
		return s.scheduleOperationObservation(ctx, cmd.OperationID)
	}
	opRecord, record, execution, err := s.loadOperationContext(ctx, cmd.OperationID)
	if err != nil {
		return Result{}, err
	}
	if err := validateExecutionContext(record, opRecord.Operation, execution); err != nil {
		return Result{}, err
	}
	if opRecord.Operation.IsTerminal() {
		if err := validatePersistedTerminalEvidence(execution, record.Resource, record.Status, opRecord.Operation); err != nil {
			return Result{}, err
		}
		if sanitizePersistedFacts(&execution) {
			if err := s.saveExecution(ctx, execution); err != nil {
				return Result{}, err
			}
			execution.Version++
		}
		return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, nil
	}
	if execution.State == AttemptSucceeded || execution.State == AttemptFailed {
		if err := validatePersistedTerminalEvidence(execution, record.Resource, record.Status, opRecord.Operation); err != nil {
			return Result{}, err
		}
		sanitizePersistedFacts(&execution)
		facts, at := persistedTerminalFacts(execution, record.Status.UpdatedAt())
		if execution.State == AttemptSucceeded {
			return s.finishSubmitted(ctx, record, opRecord, execution, true, "RecoveredTerminalOutcome", "", at, facts)
		}
		failure := normalizeExecutionFailure(persistedTerminalExecution(execution).Failure)
		execution.LastFailure = failure
		return s.finishSubmitted(ctx, record, opRecord, execution, false, failure.Reason, failure.Message, at, facts)
	}
	if execution.State == AttemptPending || execution.State == AttemptDispatching {
		return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, nil
	}
	provider, err := s.Resolver.Resolve(ctx, execution.ProvisionerRef)
	if err != nil || isNilInterface(provider) {
		return Result{}, fmt.Errorf("%w: %v", ErrProvisionerNotFound, err)
	}
	request := provisioning.ObservationRequest{OperationID: execution.OperationID, AttemptNumber: execution.CurrentAttempt, ResourceID: execution.ResourceID, ResourceType: execution.ResourceType, Spec: execution.Spec, Capability: execution.Capability, TargetGeneration: execution.TargetGeneration, Handle: execution.Handle}
	observation, observeErr := provider.Observe(ctx, request)
	if observeErr != nil && !explicitTerminalObservation(observation) {
		execution.LastFailure = normalizeExecutionFailure(failureFromError(observeErr))
		execution.State = AttemptUnknown
		if saveErr := s.saveExecution(ctx, execution); saveErr != nil {
			return Result{}, fmt.Errorf("%w: %v; recording observation failure: %v", observeErr, observeErr, saveErr)
		}
		return Result{}, provisioning.ObservationError{Failure: *execution.LastFailure}
	}
	observationAt := observedAt(observation.ObservedAt, cmd.ObservedAt)
	if err := validateObservationTime(record.Resource, record.Status, observationAt); err != nil {
		return Result{}, err
	}
	if factsErr := validateObservedFacts(observation.Resource); factsErr != nil {
		if !explicitTerminalObservation(observation) {
			return Result{}, factsErr
		}
		observation.Resource = domain.ObservedFacts{}
		execution.LastFailure = &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "MalformedObservedFacts", Message: factsErr.Error()}
	}
	previousObservedAt := execution.LastObservedAt
	if !previousObservedAt.IsZero() && !observationAt.After(previousObservedAt) {
		return Result{}, fmt.Errorf("%w: observation time precedes the previous execution observation", lifecycle.ErrInvalidTransition)
	}
	previousObservation := execution.LastObservation
	execution.LastObservation = &observation
	execution.LastObservedAt = observationAt
	execution.Correlation = observation.Correlation
	// A nil Execution only means no execution is currently active. It cannot
	// prove that this OperationID was never accepted and never authorizes an
	// in-place retry of an ambiguous submission attempt.
	if observation.Execution != nil && observation.Execution.Handle != nil {
		execution.Handle = observation.Execution.Handle
	}
	if observation.Execution != nil {
		if !validExecutionState(observation.Execution.State) {
			execution.State = AttemptUnknown
			execution.LastFailure = &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "MalformedExecutionState", Message: fmt.Sprintf("provider reported invalid execution state %q", observation.Execution.State)}
			execution.LastObservation = previousObservation
			execution.LastObservedAt = previousObservedAt
			if saveErr := s.saveExecution(ctx, execution); saveErr != nil {
				return Result{}, saveErr
			}
			return Result{}, execution.LastFailure
		}
		execution.State = attemptState(observation.Execution.State)
		if executionConfirmsAcceptance(*observation.Execution) {
			execution.AcceptanceConfirmed = true
		}
		if observation.Execution.State == provisioning.ExecutionStateFailed {
			failure := normalizeExecutionFailure(observation.Execution.Failure)
			observation.Execution.Failure = failure
			execution.LastFailure = failure
			execution.State = AttemptFailed
		}
	}
	if observation.Correlation == provisioning.RequestCorrelationNotFound && execution.State == AttemptUnknown && !execution.AcceptanceConfirmed {
		return s.scheduleResubmission(ctx, record, opRecord, execution)
	}
	if shouldComplete(observation) {
		return s.finishSubmitted(ctx, record, opRecord, execution, true, "ObservationSucceeded", "", observationAt, observation.Resource)
	}
	if shouldFail(observation) {
		failure := observation.Execution.Failure
		execution.LastFailure = failure
		return s.finishSubmitted(ctx, record, opRecord, execution, false, failure.Reason, failure.Message, observationAt, observation.Resource)
	}
	execution, err = s.saveObservation(ctx, execution)
	if err != nil {
		return Result{}, err
	}
	return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, nil
}

func (s *Service) scheduleOperationObservation(ctx context.Context, operationID domain.OperationID) (Result, error) {
	var result Result
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		operation, err := tx.Operations().GetOperation(ctx, operationID)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrOperationNotFound, err)
		}
		resource, err := tx.Resources().GetResource(ctx, operation.Operation.ResourceID())
		if err != nil {
			return fmt.Errorf("%w: %v", ErrResourceNotFound, err)
		}
		execution, err := tx.Executions().GetExecution(ctx, operationID)
		if err != nil {
			return err
		}
		if operation.Operation.IsTerminal() {
			result = Result{Resource: resource, Operation: operation.Operation, Execution: &execution}
			return nil
		}
		if execution.State != AttemptUnknown && execution.State != AttemptAccepted {
			return fmt.Errorf("%w: execution is not observable from state %s", ErrInvalidApplicationCall, execution.State)
		}
		sequence := execution.NextObservation
		if sequence == 0 {
			sequence = 1
		}
		execution.NextObservation = sequence + 1
		message := ObserveMessage(operationID, sequence, execution.Version+1)
		if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
			return err
		}
		if err := tx.Outbox().Enqueue(ctx, message); err != nil {
			return err
		}
		execution.Version++
		result = Result{Resource: resource, Operation: operation.Operation, Execution: &execution}
		return nil
	})
	return result, err
}

func (s *Service) scheduleResubmission(ctx context.Context, record ResourceRecord, opRecord OperationRecord, observed ProvisioningExecutionRecord) (Result, error) {
	var result Result
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		currentOperation, err := tx.Operations().GetOperation(ctx, observed.OperationID)
		if err != nil {
			return err
		}
		current, err := tx.Executions().GetExecution(ctx, observed.OperationID)
		if err != nil {
			return err
		}
		if currentOperation.Version != opRecord.Version || current.Version != observed.Version || current.CurrentAttempt != observed.CurrentAttempt || current.State != AttemptUnknown || current.AcceptanceConfirmed {
			return ErrConcurrencyConflict
		}
		oldAttempt, err := tx.SubmissionAttempts().GetSubmissionAttempt(ctx, current.OperationID, current.CurrentAttempt)
		if err != nil {
			return err
		}
		oldState := oldAttempt.State
		if oldState != SubmissionAttemptUnknown && oldState != SubmissionAttemptLeased {
			return ErrConcurrencyConflict
		}
		oldAttempt.State = SubmissionAttemptNotFound
		oldAttempt.ResolvedAt = observed.LastObservedAt
		if err := tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, oldAttempt, oldState); err != nil {
			return err
		}
		current.CurrentAttempt++
		current.State = AttemptPending
		current.Correlation = provisioning.RequestCorrelationNotFound
		message := DispatchMessage(current.OperationID, current.CurrentAttempt, current.Version+1)
		if err := tx.SubmissionAttempts().CreateSubmissionAttempt(ctx, SubmissionAttemptRecord{OperationID: current.OperationID, AttemptNumber: current.CurrentAttempt, State: SubmissionAttemptPending, DispatchMessage: message.ID}); err != nil {
			return err
		}
		if err := tx.Executions().SaveExecution(ctx, current, current.Version); err != nil {
			return err
		}
		if err := tx.Outbox().Enqueue(ctx, message); err != nil {
			return err
		}
		current.Version++
		result = Result{Resource: record, Operation: currentOperation.Operation, Execution: &current}
		return nil
	})
	return result, err
}

func (s *Service) RetryOperation(ctx context.Context, cmd RetryOperationCommand) (Result, error) {
	if !s.eager {
		return s.AdmitRetryOperation(ctx, cmd)
	}
	if err := validateIdempotency(cmd.IdempotencyKey, cmd.Fingerprint); err != nil {
		return Result{}, err
	}
	if err := validateExternalEventID(cmd.EventID); err != nil {
		return Result{}, err
	}
	if replay, found, err := s.replay(ctx, cmd.IdempotencyKey, cmd.Fingerprint, "retry"); err != nil {
		return Result{}, err
	} else if found {
		return replay, nil
	}
	result, err := s.persistRetryRequest(ctx, cmd)
	if err != nil {
		return Result{}, err
	}
	if result.Replay {
		return result, nil
	}
	return s.drive(ctx, result.Operation.ID())
}

func (s *Service) AdmitRetryOperation(ctx context.Context, cmd RetryOperationCommand) (Result, error) {
	if err := validateIdempotency(cmd.IdempotencyKey, cmd.Fingerprint); err != nil {
		return Result{}, err
	}
	if err := validateExternalEventID(cmd.EventID); err != nil {
		return Result{}, err
	}
	if replay, found, err := s.replay(ctx, cmd.IdempotencyKey, cmd.Fingerprint, "retry"); err != nil || found {
		return replay, err
	}
	return s.persistRetryRequest(ctx, cmd)
}

func (s *Service) persistRetryRequest(ctx context.Context, cmd RetryOperationCommand) (Result, error) {
	var result Result
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		if replay, found, err := replayWithin(ctx, tx, cmd.IdempotencyKey, cmd.Fingerprint, "retry"); err != nil {
			return err
		} else if found {
			result = replay
			return nil
		}
		failed, err := tx.Operations().GetOperation(ctx, cmd.OperationID)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrOperationNotFound, err)
		}
		if failed.Operation.State() != domain.OperationStateFailed {
			return fmt.Errorf("%w: operation is not failed", lifecycle.ErrInvalidTransition)
		}
		record, err := tx.Resources().GetResource(ctx, failed.Operation.ResourceID())
		if err != nil {
			return fmt.Errorf("%w: %v", ErrResourceNotFound, err)
		}
		if active, found, err := tx.Operations().ActiveForResource(ctx, record.Resource.ID()); err != nil {
			return err
		} else if found {
			return fmt.Errorf("%w: resource %q has operation %q", lifecycle.ErrOperationActive, record.Resource.ID(), active.Operation.ID())
		}
		resourceType, err := s.Types.Get(ctx, record.Resource.Type())
		if err != nil {
			return fmt.Errorf("%w: %v", ErrResourceTypeNotFound, err)
		}
		transition, err := s.Lifecycle.Request(record.Resource, resourceType, record.Status, &failed.Operation, failed.Operation.Capability(), cmd.NewOperationID, cmd.EventID, cmd.RequestedAt)
		if err != nil {
			return err
		}
		record.Status = transition.Status
		if err := tx.Resources().SaveResource(ctx, record, record.Version); err != nil {
			return err
		}
		record.Version++
		result, err = persistExistingTransition(ctx, tx, record, transition, cmd.IdempotencyKey, cmd.Fingerprint, "retry")
		return err
	})
	return result, err
}

func (s *Service) drive(ctx context.Context, operationID domain.OperationID) (Result, error) {
	for {
		opRecord, err := s.loadOperation(ctx, operationID)
		if err != nil {
			return Result{}, err
		}
		switch opRecord.Operation.Phase() {
		case domain.OperationPhaseRequested:
			if _, err := s.advanceOperation(ctx, AdvanceOperationCommand{OperationID: operationID, Phase: domain.OperationPhaseValidating, EventID: internalEventID(operationID, "validating"), ChangedAt: opRecord.Operation.RequestedAt().Add(time.Nanosecond)}); err != nil {
				return Result{}, err
			}
		case domain.OperationPhaseValidating:
			if _, err := s.advanceOperation(ctx, AdvanceOperationCommand{OperationID: operationID, Phase: nextPhase(opRecord.Operation), EventID: internalEventID(operationID, "next"), ChangedAt: opRecord.Operation.PhaseChangedAt().Add(time.Nanosecond)}); err != nil {
				return Result{}, err
			}
		case domain.OperationPhasePlanning:
			if _, err := s.advanceOperation(ctx, AdvanceOperationCommand{OperationID: operationID, Phase: domain.OperationPhaseApplying, EventID: internalEventID(operationID, "applying"), ChangedAt: opRecord.Operation.PhaseChangedAt().Add(time.Nanosecond)}); err != nil {
				return Result{}, err
			}
		case domain.OperationPhaseApplying, domain.OperationPhaseDestroying:
			return s.DispatchOperation(ctx, operationID)
		default:
			return Result{}, fmt.Errorf("%w: unsupported operation phase %s", lifecycle.ErrInvalidTransition, opRecord.Operation.Phase())
		}
	}
}

func nextPhase(operation domain.Operation) domain.OperationPhase {
	if operation.Capability() == domain.CapabilityDelete {
		return domain.OperationPhaseDestroying
	}
	return domain.OperationPhasePlanning
}

// DispatchOperation advances a persisted provisioning attempt without creating
// a new lifecycle Operation. Unknown attempts are observed before any future
// same-OperationID resubmission can be considered.
func (s *Service) DispatchOperation(ctx context.Context, operationID domain.OperationID) (Result, error) {
	if !s.eager {
		return Result{}, fmt.Errorf("%w: direct dispatch is disabled; use the durable worker", ErrInvalidApplicationCall)
	}
	opRecord, record, execution, err := s.loadOperationContext(ctx, operationID)
	if err != nil {
		return Result{}, err
	}
	if err := validateExecutionContext(record, opRecord.Operation, execution); err != nil {
		return Result{}, err
	}
	if opRecord.Operation.IsTerminal() || (opRecord.Operation.Phase() != domain.OperationPhaseApplying && opRecord.Operation.Phase() != domain.OperationPhaseDestroying) {
		return Result{}, fmt.Errorf("%w: operation is not dispatchable", lifecycle.ErrInvalidTransition)
	}
	if execution.State == AttemptUnknown {
		return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, nil
	}
	if execution.State == AttemptDispatching {
		return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, nil
	}
	if execution.State != AttemptPending {
		return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, nil
	}
	provider, err := s.Resolver.Resolve(ctx, execution.ProvisionerRef)
	if err != nil || isNilInterface(provider) {
		return Result{}, fmt.Errorf("%w: %v", ErrProvisionerNotFound, err)
	}
	execution, claimed, err := s.claimPendingDispatch(ctx, operationID, execution)
	if err != nil {
		return Result{}, err
	}
	if !claimed {
		return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, nil
	}
	submission, submitErr := provider.Submit(ctx, executionRequest(execution))
	if factsErr := validateObservedFacts(submission.Observation.Resource); factsErr != nil {
		if explicitTerminalExecution(submission) {
			submission.Observation.Resource = domain.ObservedFacts{}
			execution.LastFailure = &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "MalformedObservedFacts", Message: factsErr.Error()}
		} else {
			execution.State = AttemptUnknown
			execution.LastFailure = &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "MalformedObservedFacts", Message: factsErr.Error()}
			if saveErr := s.saveExecution(ctx, execution); saveErr != nil {
				return Result{}, saveErr
			}
			execution.Version++
			return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, factsErr
		}
	}
	if submission.Observation.Execution != nil {
		if !validExecutionState(submission.Observation.Execution.State) {
			execution.State = AttemptUnknown
			execution.LastFailure = &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "MalformedExecutionState", Message: fmt.Sprintf("provider reported invalid execution state %q", submission.Observation.Execution.State)}
			if saveErr := s.saveExecution(ctx, execution); saveErr != nil {
				return Result{}, saveErr
			}
			execution.Version++
			return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, execution.LastFailure
		}
		execution.State = attemptState(submission.Observation.Execution.State)
		if submission.Observation.Execution.Handle != nil {
			execution.Handle = submission.Observation.Execution.Handle
		}
		execution.AcceptanceConfirmed = executionConfirmsAcceptance(*submission.Observation.Execution)
	}
	execution.Submission = &submission
	if submission.Observation.Execution != nil {
		observationAt := observationTime(submission.Observation.ObservedAt, record.Status.UpdatedAt())
		if observationErr := validateObservationTime(record.Resource, record.Status, observationAt); observationErr != nil {
			execution.State = AttemptUnknown
			execution.LastFailure = &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "StaleSubmissionObservation", Message: observationErr.Error()}
			if saveErr := s.saveExecution(ctx, execution); saveErr != nil {
				return Result{}, saveErr
			}
			execution.Version++
			return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, observationErr
		}
		execution.LastObservedAt = observationAt
	}
	if submitErr != nil && !conclusiveNonAcceptance(submission) && !explicitTerminalExecution(submission) {
		execution.State = AttemptUnknown
		execution.LastFailure = normalizeExecutionFailure(failureFromError(submitErr))
		if err := s.saveExecution(ctx, execution); err != nil {
			return Result{}, err
		}
		execution.Version++
		return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, submitErr
	}
	if submitErr != nil {
		if submission.Observation.Execution.State == provisioning.ExecutionStateSucceeded {
			return s.finishSubmitted(ctx, record, opRecord, execution, true, "SubmissionSucceeded", "", observationTime(submission.Observation.ObservedAt, record.Status.UpdatedAt()), submission.Observation.Resource)
		}
		failure := submission.Observation.Execution.Failure
		failure = normalizeExecutionFailure(failure)
		submission.Observation.Execution.Failure = failure
		execution.Submission = &submission
		execution.LastFailure = failure
		return s.finishSubmitted(ctx, record, opRecord, execution, false, failure.Reason, failure.Message, observationTime(submission.Observation.ObservedAt, record.Status.UpdatedAt()), submission.Observation.Resource)
	}
	if submission.Observation.Execution == nil {
		failure := &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "MalformedSubmission", Message: "submission omitted execution result"}
		execution.State = AttemptUnknown
		execution.LastFailure = failure
		if err := s.saveExecution(ctx, execution); err != nil {
			return Result{}, err
		}
		execution.Version++
		return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, failure
	}
	if submission.Observation.Execution.State == provisioning.ExecutionStateSucceeded || submission.Observation.Execution.State == provisioning.ExecutionStateFailed {
		observationAt := observationTime(submission.Observation.ObservedAt, record.Status.UpdatedAt())
		if submission.Observation.Execution.State == provisioning.ExecutionStateSucceeded {
			return s.finishSubmitted(ctx, record, opRecord, execution, true, "SubmissionSucceeded", "", observationAt, submission.Observation.Resource)
		}
		failure := submission.Observation.Execution.Failure
		failure = normalizeExecutionFailure(failure)
		submission.Observation.Execution.Failure = failure
		execution.Submission = &submission
		execution.LastFailure = failure
		return s.finishSubmitted(ctx, record, opRecord, execution, false, failure.Reason, failure.Message, observationAt, submission.Observation.Resource)
	}
	if err := s.saveExecution(ctx, execution); err != nil {
		return Result{}, err
	}
	execution.Version++
	return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, nil
}

// RecoverOperation observes an Unknown attempt using an explicit application
// receipt timestamp. It never changes or recovers a Dispatching claim.
func (s *Service) RecoverOperation(ctx context.Context, operationID domain.OperationID, observedAt time.Time) (Result, error) {
	if !s.eager {
		return s.scheduleOperationObservation(ctx, operationID)
	}
	execution, err := s.loadExecution(ctx, operationID)
	if err != nil {
		return Result{}, err
	}
	if execution.State != AttemptUnknown {
		return Result{}, fmt.Errorf("%w: execution is not recoverable from state %s", ErrInvalidApplicationCall, execution.State)
	}
	return s.ObserveOperation(ctx, ObserveOperationCommand{OperationID: operationID, ObservedAt: observedAt})
}

func (s *Service) finishSubmitted(ctx context.Context, record ResourceRecord, opRecord OperationRecord, execution ProvisioningExecutionRecord, succeeded bool, reason, message string, at time.Time, facts domain.ObservedFacts) (Result, error) {
	result, err := s.finishObserved(ctx, record, opRecord, execution, succeeded, reason, message, at, facts)
	if err == nil {
		return result, nil
	}
	if succeeded {
		execution.State = AttemptSucceeded
		execution.LastFailure = failureFromError(err)
	} else {
		execution.State = AttemptFailed
		if execution.LastFailure == nil {
			execution.LastFailure = failureFromError(err)
		}
	}
	if saveErr := s.saveExecution(ctx, execution); saveErr != nil {
		return Result{}, fmt.Errorf("%w; recording rejected submission outcome: %v", err, saveErr)
	}
	execution.Version++
	return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, err
}

func (s *Service) finishObserved(ctx context.Context, record ResourceRecord, opRecord OperationRecord, execution ProvisioningExecutionRecord, succeeded bool, reason, message string, at time.Time, facts domain.ObservedFacts) (Result, error) {
	if at.IsZero() {
		at = record.Status.UpdatedAt()
	}
	var result Result
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		current, err := tx.Resources().GetResource(ctx, record.Resource.ID())
		if err != nil {
			return err
		}
		currentOperation, err := tx.Operations().GetOperation(ctx, opRecord.Operation.ID())
		if err != nil {
			return err
		}
		currentExecution, err := tx.Executions().GetExecution(ctx, currentOperation.Operation.ID())
		if err != nil {
			return err
		}
		if err := validateExecutionContext(current, currentOperation.Operation, currentExecution); err != nil {
			return err
		}
		if currentOperation.Operation.IsTerminal() {
			incomingState := AttemptFailed
			if succeeded {
				incomingState = AttemptSucceeded
			}
			if currentExecution.State != incomingState {
				return fmt.Errorf("%w: terminal execution evidence contradicts persisted operation outcome", lifecycle.ErrInvalidTransition)
			}
			result = Result{Resource: current, Operation: currentOperation.Operation, Execution: &currentExecution}
			return nil
		}
		retryingPersistedTerminal := (currentExecution.State == AttemptSucceeded || currentExecution.State == AttemptFailed) && execution.LastObservedAt.Equal(currentExecution.LastObservedAt)
		if !retryingPersistedTerminal && !currentExecution.LastObservedAt.IsZero() && !execution.LastObservedAt.IsZero() && !execution.LastObservedAt.After(currentExecution.LastObservedAt) {
			return fmt.Errorf("%w: terminal observation is not newer than persisted execution evidence", lifecycle.ErrInvalidTransition)
		}
		if currentExecution.AcceptanceConfirmed {
			execution.AcceptanceConfirmed = true
		}
		if execution.Handle == nil {
			execution.Handle = currentExecution.Handle
		}
		execution.Version = currentExecution.Version
		var transition struct {
			Operation domain.Operation
			Status    domain.ResourceStatus
			Event     domain.Event
		}
		if succeeded {
			completed, completeErr := s.Lifecycle.Complete(current.Resource, current.Status, currentOperation.Operation, internalEventID(currentOperation.Operation.ID(), "succeeded"), at)
			if completeErr != nil {
				return completeErr
			}
			transition.Operation, transition.Status, transition.Event = completed.Operation, completed.Status, completed.Event
			execution.State = AttemptSucceeded
			execution.LastFailure = nil
		} else {
			failed, failErr := s.Lifecycle.Fail(current.Resource, current.Status, currentOperation.Operation, reason, message, internalEventID(currentOperation.Operation.ID(), "failed"), at)
			if failErr != nil {
				return failErr
			}
			transition.Operation, transition.Status, transition.Event = failed.Operation, failed.Status, failed.Event
			execution.State = AttemptFailed
		}
		if hasObservedFacts(facts) {
			observedStatus, observeErr := s.Lifecycle.ApplyPostOperationObservation(current.Resource, transition.Status, facts, at)
			if observeErr != nil {
				return observeErr
			}
			transition.Status = observedStatus
		}
		current.Status = transition.Status
		if err := tx.Resources().SaveResource(ctx, current, current.Version); err != nil {
			return err
		}
		current.Version++
		if err := tx.Operations().SaveOperation(ctx, OperationRecord{Operation: transition.Operation}, currentOperation.Version); err != nil {
			return err
		}
		if err := tx.Events().Append(ctx, transition.Event); err != nil {
			return err
		}
		if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
			return err
		}
		execution.Version++
		event := transition.Event
		result = Result{Resource: current, Operation: transition.Operation, Execution: &execution, Event: &event}
		return nil
	})
	return result, err
}

type existingRequest struct {
	id                 domain.ResourceID
	expectedGeneration uint64
	spec               *domain.ResourceSpec
	capability         domain.Capability
	operationID        domain.OperationID
	eventID            domain.EventID
	requestedAt        time.Time
	idempotencyKey     string
	fingerprint        string
}

func (s *Service) persistCreateRequest(ctx context.Context, cmd CreateResourceCommand) (Result, error) {
	var result Result
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		if replay, found, err := replayWithin(ctx, tx, cmd.IdempotencyKey, cmd.Fingerprint, string(domain.CapabilityCreate)); err != nil {
			return err
		} else if found {
			result = replay
			return nil
		}
		if active, found, err := tx.Operations().ActiveForResource(ctx, cmd.ID); err != nil {
			return err
		} else if found {
			return fmt.Errorf("%w: resource %q has operation %q", lifecycle.ErrOperationActive, cmd.ID, active.Operation.ID())
		}
		resourceType, err := s.Types.Get(ctx, cmd.Type)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrResourceTypeNotFound, err)
		}
		ref, err := s.Selector.Select(ctx, cmd.Type, domain.CapabilityCreate)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrProvisionerNotFound, err)
		}
		if _, err := NewProvisionerRef(string(ref)); err != nil {
			return fmt.Errorf("%w: %v", ErrProvisionerNotFound, err)
		}
		resource, err := domain.NewResource(cmd.ID, cmd.Type, cmd.Owner, cmd.Spec, cmd.RequestedAt)
		if err != nil {
			return err
		}
		status, err := domain.NewResourceStatus(resource.ID(), 0, domain.ResourceStateUnknown, nil, cmd.RequestedAt)
		if err != nil {
			return err
		}
		transition, err := s.Lifecycle.Request(resource, resourceType, status, nil, domain.CapabilityCreate, cmd.OperationID, cmd.EventID, cmd.RequestedAt)
		if err != nil {
			return err
		}
		result, err = persistNewRequest(ctx, tx, ResourceRecord{Resource: resource, Status: transition.Status, ProvisionerRef: ref, Version: 1}, transition, cmd.IdempotencyKey, cmd.Fingerprint)
		return err
	})
	return result, err
}

func (s *Service) persistExistingRequest(ctx context.Context, request existingRequest) (Result, error) {
	if request.expectedGeneration == 0 {
		return Result{}, fmt.Errorf("%w: expected generation is required", ErrInvalidApplicationCall)
	}
	var result Result
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		if replay, found, err := replayWithin(ctx, tx, request.idempotencyKey, request.fingerprint, string(request.capability)); err != nil {
			return err
		} else if found {
			result = replay
			return nil
		}
		stored, err := tx.Resources().GetResource(ctx, request.id)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrResourceNotFound, err)
		}
		if stored.Resource.Generation() != request.expectedGeneration {
			return fmt.Errorf("%w: expected generation %d, got %d", ErrConcurrencyConflict, request.expectedGeneration, stored.Resource.Generation())
		}
		active, found, err := tx.Operations().ActiveForResource(ctx, request.id)
		if err != nil {
			return err
		}
		var latest *domain.Operation
		if found {
			latest = &active.Operation
		}
		resourceType, err := s.Types.Get(ctx, stored.Resource.Type())
		if err != nil {
			return fmt.Errorf("%w: %v", ErrResourceTypeNotFound, err)
		}
		if request.spec != nil {
			if err := stored.Resource.UpdateSpec(*request.spec, request.requestedAt); err != nil {
				return err
			}
		}
		transition, err := s.Lifecycle.Request(stored.Resource, resourceType, stored.Status, latest, request.capability, request.operationID, request.eventID, request.requestedAt)
		if err != nil {
			return err
		}
		stored.Status = transition.Status
		if err := tx.Resources().SaveResource(ctx, stored, stored.Version); err != nil {
			return err
		}
		stored.Version++
		result, err = persistExistingTransition(ctx, tx, stored, transition, request.idempotencyKey, request.fingerprint, string(request.capability))
		return err
	})
	return result, err
}

func persistNewRequest(ctx context.Context, tx UnitOfWork, record ResourceRecord, transition lifecycle.Result, key, fingerprint string) (Result, error) {
	if err := tx.Resources().CreateResource(ctx, record); err != nil {
		return Result{}, err
	}
	return persistExistingTransition(ctx, tx, record, transition, key, fingerprint, string(transition.Operation.Capability()))
}

func persistExistingTransition(ctx context.Context, tx UnitOfWork, record ResourceRecord, transition lifecycle.Result, key, fingerprint, commandKind string) (Result, error) {
	if err := tx.Operations().CreateOperation(ctx, OperationRecord{Operation: transition.Operation, Version: 1}); err != nil {
		return Result{}, err
	}
	if err := tx.Events().Append(ctx, transition.Event); err != nil {
		return Result{}, err
	}
	execution := ProvisioningExecutionRecord{
		OperationID: transition.Operation.ID(), ProvisionerRef: record.ProvisionerRef,
		ResourceID: record.Resource.ID(), ResourceType: record.Resource.Type(), Spec: record.Resource.Spec(),
		Capability: transition.Operation.Capability(), TargetGeneration: transition.Operation.TargetGeneration(),
		State: AttemptPending, Correlation: provisioning.RequestCorrelationUnknown, NextObservation: 1, Version: 1,
	}
	if err := tx.Executions().CreateExecution(ctx, execution); err != nil {
		return Result{}, err
	}
	if key != "" {
		if err := tx.Idempotency().PutIdempotency(ctx, IdempotencyRecord{Key: key, Fingerprint: fingerprint, CommandKind: commandKind, ResourceID: record.Resource.ID(), OperationID: transition.Operation.ID()}); err != nil {
			return Result{}, err
		}
	}
	if err := tx.Outbox().Enqueue(ctx, DriveMessage(transition.Operation.ID(), 1)); err != nil {
		return Result{}, err
	}
	event := transition.Event
	return Result{Resource: record, Operation: transition.Operation, Execution: &execution, Event: &event}, nil
}

func replayWithin(ctx context.Context, tx UnitOfWork, key, fingerprint, commandKind string) (Result, bool, error) {
	if key == "" {
		return Result{}, false, nil
	}
	existing, err := tx.Idempotency().GetIdempotency(ctx, key)
	if errors.Is(err, ErrIdempotencyNotFound) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	if existing.Fingerprint != fingerprint || existing.CommandKind != commandKind {
		return Result{}, false, ErrIdempotencyConflict
	}
	op, err := tx.Operations().GetOperation(ctx, existing.OperationID)
	if err != nil {
		return Result{}, false, err
	}
	resource, err := tx.Resources().GetResource(ctx, existing.ResourceID)
	if err != nil {
		return Result{}, false, err
	}
	if existing.Key != key || existing.OperationID != op.Operation.ID() || existing.ResourceID != resource.Resource.ID() || op.Operation.ResourceID() != resource.Resource.ID() {
		return Result{}, false, fmt.Errorf("%w: idempotency record associations are inconsistent", ErrInvalidApplicationCall)
	}
	execution, err := tx.Executions().GetExecution(ctx, existing.OperationID)
	if err != nil {
		return Result{}, false, err
	}
	if err := validateExecutionContext(resource, op.Operation, execution); err != nil {
		return Result{}, false, err
	}
	if op.Operation.IsTerminal() || execution.State == AttemptSucceeded || execution.State == AttemptFailed {
		if err := validatePersistedTerminalEvidence(execution, resource.Resource, resource.Status, op.Operation); err != nil {
			return Result{}, false, err
		}
	}
	return Result{Resource: resource, Operation: op.Operation, Execution: &execution, Replay: true}, true, nil
}

func (s *Service) loadResource(ctx context.Context, id domain.ResourceID) (ResourceRecord, error) {
	var record ResourceRecord
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		var err error
		record, err = tx.Resources().GetResource(ctx, id)
		return err
	})
	if err != nil {
		return ResourceRecord{}, fmt.Errorf("%w: %v", ErrResourceNotFound, err)
	}
	return record, nil
}

func (s *Service) replay(ctx context.Context, key, fingerprint, commandKind string) (Result, bool, error) {
	if key == "" {
		return Result{}, false, nil
	}
	var result Result
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		existing, err := tx.Idempotency().GetIdempotency(ctx, key)
		if errors.Is(err, ErrIdempotencyNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if existing.Fingerprint != fingerprint || existing.CommandKind != commandKind {
			return ErrIdempotencyConflict
		}
		op, err := tx.Operations().GetOperation(ctx, existing.OperationID)
		if err != nil {
			return err
		}
		resource, err := tx.Resources().GetResource(ctx, existing.ResourceID)
		if err != nil {
			return err
		}
		if existing.Key != key || existing.OperationID != op.Operation.ID() || existing.ResourceID != resource.Resource.ID() || op.Operation.ResourceID() != resource.Resource.ID() {
			return fmt.Errorf("%w: idempotency record associations are inconsistent", ErrInvalidApplicationCall)
		}
		execution, err := tx.Executions().GetExecution(ctx, existing.OperationID)
		if err != nil {
			return err
		}
		if err := validateExecutionContext(resource, op.Operation, execution); err != nil {
			return err
		}
		if op.Operation.IsTerminal() || execution.State == AttemptSucceeded || execution.State == AttemptFailed {
			if err := validatePersistedTerminalEvidence(execution, resource.Resource, resource.Status, op.Operation); err != nil {
				return err
			}
		}
		result = Result{Resource: resource, Operation: op.Operation, Execution: &execution, Replay: true}
		return nil
	})
	if err != nil {
		return Result{}, false, err
	}
	return result, result.Replay, nil
}

func (s *Service) loadOperation(ctx context.Context, id domain.OperationID) (OperationRecord, error) {
	var record OperationRecord
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		var err error
		record, err = tx.Operations().GetOperation(ctx, id)
		return err
	})
	if err != nil {
		return OperationRecord{}, fmt.Errorf("%w: %v", ErrOperationNotFound, err)
	}
	return record, nil
}

func (s *Service) loadExecution(ctx context.Context, id domain.OperationID) (ProvisioningExecutionRecord, error) {
	var record ProvisioningExecutionRecord
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		var err error
		record, err = tx.Executions().GetExecution(ctx, id)
		return err
	})
	return record, err
}

func (s *Service) loadOperationContext(ctx context.Context, id domain.OperationID) (OperationRecord, ResourceRecord, ProvisioningExecutionRecord, error) {
	var operation OperationRecord
	var resource ResourceRecord
	var execution ProvisioningExecutionRecord
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		var err error
		operation, err = tx.Operations().GetOperation(ctx, id)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrOperationNotFound, err)
		}
		resource, err = tx.Resources().GetResource(ctx, operation.Operation.ResourceID())
		if err != nil {
			return fmt.Errorf("%w: %v", ErrResourceNotFound, err)
		}
		execution, err = tx.Executions().GetExecution(ctx, id)
		return err
	})
	return operation, resource, execution, err
}

func (s *Service) saveExecution(ctx context.Context, execution ProvisioningExecutionRecord) error {
	return s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		return tx.Executions().SaveExecution(ctx, execution, execution.Version)
	})
}

func (s *Service) saveObservation(ctx context.Context, observation ProvisioningExecutionRecord) (ProvisioningExecutionRecord, error) {
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		operation, operationErr := tx.Operations().GetOperation(ctx, observation.OperationID)
		if operationErr != nil {
			return operationErr
		}
		if operation.Operation.IsTerminal() {
			return fmt.Errorf("%w: cannot persist nonterminal evidence for a terminal operation", lifecycle.ErrInvalidTransition)
		}
		current, loadErr := tx.Executions().GetExecution(ctx, observation.OperationID)
		if loadErr != nil {
			return loadErr
		}
		if !current.LastObservedAt.IsZero() && !observation.LastObservedAt.After(current.LastObservedAt) {
			return fmt.Errorf("%w: observation is not newer than persisted execution evidence", lifecycle.ErrInvalidTransition)
		}
		if current.AcceptanceConfirmed {
			observation.AcceptanceConfirmed = true
			if observation.LastObservation != nil && observation.LastObservation.Execution == nil && observation.State == AttemptPending {
				observation.State = current.State
			}
		}
		if observation.LastObservation == nil || observation.LastObservation.Execution == nil || observation.LastObservation.Execution.Handle == nil {
			observation.Handle = current.Handle
		}
		observation.Version = current.Version
		if saveErr := tx.Executions().SaveExecution(ctx, observation, current.Version); saveErr != nil {
			return saveErr
		}
		observation.Version++
		return nil
	})
	return observation, err
}

func (s *Service) claimPendingDispatch(ctx context.Context, operationID domain.OperationID, expected ProvisioningExecutionRecord) (ProvisioningExecutionRecord, bool, error) {
	var execution ProvisioningExecutionRecord
	claimed := false
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		operation, operationErr := tx.Operations().GetOperation(ctx, operationID)
		if operationErr != nil {
			return operationErr
		}
		if operation.Operation.IsTerminal() {
			return fmt.Errorf("%w: cannot dispatch a terminal operation", lifecycle.ErrInvalidTransition)
		}
		if operation.Operation.Phase() != domain.OperationPhaseApplying && operation.Operation.Phase() != domain.OperationPhaseDestroying {
			return fmt.Errorf("%w: operation is not in a dispatchable phase", lifecycle.ErrInvalidTransition)
		}
		resource, resourceErr := tx.Resources().GetResource(ctx, operation.Operation.ResourceID())
		if resourceErr != nil {
			return resourceErr
		}
		var err error
		execution, err = tx.Executions().GetExecution(ctx, operationID)
		if err != nil {
			return err
		}
		if err := validateExecutionContext(resource, operation.Operation, execution); err != nil {
			return err
		}
		if execution.Version != expected.Version || execution.ProvisionerRef != expected.ProvisionerRef {
			return ErrConcurrencyConflict
		}
		if execution.State != AttemptPending {
			return nil
		}
		if execution.CurrentAttempt == 0 {
			execution.CurrentAttempt = 1
			message := DispatchMessage(execution.OperationID, execution.CurrentAttempt, execution.Version+1)
			if err := tx.SubmissionAttempts().CreateSubmissionAttempt(ctx, SubmissionAttemptRecord{OperationID: execution.OperationID, AttemptNumber: execution.CurrentAttempt, State: SubmissionAttemptLeased, DispatchMessage: message.ID}); err != nil {
				return err
			}
		} else {
			attempt, err := tx.SubmissionAttempts().GetSubmissionAttempt(ctx, execution.OperationID, execution.CurrentAttempt)
			if err != nil {
				return err
			}
			if attempt.State != SubmissionAttemptPending {
				return nil
			}
			attempt.State = SubmissionAttemptLeased
			if err := tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, attempt, SubmissionAttemptPending); err != nil {
				return err
			}
		}
		execution.State = AttemptDispatching
		if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
			return err
		}
		execution.Version++
		claimed = true
		return nil
	})
	return execution, claimed, err
}

func executionRequest(execution ProvisioningExecutionRecord) provisioning.ExecutionRequest {
	return provisioning.ExecutionRequest{
		OperationID: execution.OperationID, AttemptNumber: execution.CurrentAttempt, ResourceID: execution.ResourceID,
		ResourceType: execution.ResourceType, Spec: execution.Spec,
		Capability: execution.Capability, TargetGeneration: execution.TargetGeneration,
	}
}

func conclusiveNonAcceptance(submission provisioning.Submission) bool {
	execution := submission.Observation.Execution
	if execution == nil || execution.State != provisioning.ExecutionStateFailed || execution.Failure == nil {
		return false
	}
	return execution.Failure.Kind == provisioning.FailureUnsupported || execution.Failure.Kind == provisioning.FailureInvalidRequest
}

func explicitTerminalExecution(submission provisioning.Submission) bool {
	execution := submission.Observation.Execution
	return execution != nil && (execution.State == provisioning.ExecutionStateSucceeded || execution.State == provisioning.ExecutionStateFailed)
}

func explicitTerminalObservation(observation provisioning.ExecutionObservation) bool {
	return observation.Execution != nil && (observation.Execution.State == provisioning.ExecutionStateSucceeded || observation.Execution.State == provisioning.ExecutionStateFailed)
}

func normalizeExecutionFailure(failure *provisioning.ExecutionFailure) *provisioning.ExecutionFailure {
	if failure == nil {
		return &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "MalformedExecutionFailure", Message: "provider reported failure without details"}
	}
	normalized := *failure
	if !validFailureKind(normalized.Kind) {
		normalized.Kind = provisioning.FailureUnknown
	}
	if strings.TrimSpace(normalized.Reason) == "" {
		normalized.Reason = "MalformedExecutionFailure"
		if normalized.Message == "" {
			normalized.Message = "provider reported failure without details"
		}
	}
	return &normalized
}

func validFailureKind(kind provisioning.ExecutionFailureKind) bool {
	switch kind {
	case provisioning.FailureInvalidRequest, provisioning.FailureUnsupported, provisioning.FailureUnavailable, provisioning.FailureTimeout, provisioning.FailureNotFound, provisioning.FailureExecution, provisioning.FailureUnknown:
		return true
	default:
		return false
	}
}

func hasObservedFacts(facts domain.ObservedFacts) bool {
	return facts.Presence != "" || facts.Readiness != "" || facts.Drift != ""
}

func validateIdempotency(key, fingerprint string) error {
	if key != "" && strings.TrimSpace(fingerprint) == "" {
		return fmt.Errorf("%w: idempotency fingerprint is required when a key is provided", ErrInvalidApplicationCall)
	}
	return nil
}

const internalEventPrefix = "liftr-internal-"

func validateExternalEventID(eventID domain.EventID) error {
	if strings.HasPrefix(string(eventID), internalEventPrefix) {
		return fmt.Errorf("%w: event ID uses reserved internal prefix", ErrInvalidApplicationCall)
	}
	return nil
}

func internalEventID(operationID domain.OperationID, transition string) domain.EventID {
	digest := sha256.Sum256([]byte(string(operationID) + "\x00" + transition))
	return domain.EventID(fmt.Sprintf("%s%x", internalEventPrefix, digest))
}

func attemptState(state provisioning.ExecutionState) ProvisioningAttemptState {
	switch state {
	case provisioning.ExecutionStateAccepted:
		return AttemptAccepted
	case provisioning.ExecutionStateRunning:
		return AttemptAccepted
	case provisioning.ExecutionStateSucceeded:
		return AttemptSucceeded
	case provisioning.ExecutionStateFailed:
		return AttemptFailed
	case provisioning.ExecutionStateUnknown:
		return AttemptUnknown
	default:
		return AttemptUnknown
	}
}

func executionConfirmsAcceptance(execution provisioning.Execution) bool {
	switch execution.State {
	case provisioning.ExecutionStateAccepted, provisioning.ExecutionStateRunning, provisioning.ExecutionStateSucceeded:
		return true
	case provisioning.ExecutionStateFailed:
		return execution.Failure != nil && execution.Failure.Kind == provisioning.FailureExecution
	default:
		return false
	}
}

func validExecutionState(state provisioning.ExecutionState) bool {
	switch state {
	case provisioning.ExecutionStateAccepted, provisioning.ExecutionStateRunning, provisioning.ExecutionStateSucceeded, provisioning.ExecutionStateFailed, provisioning.ExecutionStateUnknown:
		return true
	default:
		return false
	}
}

func failureFromError(err error) *provisioning.ExecutionFailure {
	var observationErrPointer *provisioning.ObservationError
	if errors.As(err, &observationErrPointer) {
		if observationErrPointer == nil {
			return normalizeExecutionFailure(nil)
		}
		return &observationErrPointer.Failure
	}
	var observationErr provisioning.ObservationError
	if errors.As(err, &observationErr) {
		return &observationErr.Failure
	}
	value := reflect.ValueOf(err)
	if value.Kind() == reflect.Pointer && value.IsNil() {
		return normalizeExecutionFailure(nil)
	}
	return &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "ProvisionerCallFailed", Message: err.Error()}
}

func shouldComplete(observation provisioning.ExecutionObservation) bool {
	return observation.Execution != nil && observation.Execution.State == provisioning.ExecutionStateSucceeded
}

func shouldFail(observation provisioning.ExecutionObservation) bool {
	return observation.Execution != nil && observation.Execution.State == provisioning.ExecutionStateFailed && observation.Execution.Failure != nil
}

func observationTime(observedAt, current time.Time) time.Time {
	if observedAt.IsZero() {
		return current
	}
	return observedAt
}

func observedAt(providerAt, fallback time.Time) time.Time {
	if providerAt.IsZero() {
		return fallback
	}
	return providerAt
}

func validateObservationTime(resource domain.Resource, status domain.ResourceStatus, at time.Time) error {
	if at.IsZero() || at.Before(resource.UpdatedAt()) || at.Before(status.UpdatedAt()) {
		return fmt.Errorf("%w: observation time precedes current resource state", lifecycle.ErrInvalidTransition)
	}
	return nil
}

func validateObservedFacts(facts domain.ObservedFacts) error {
	if !hasObservedFacts(facts) {
		return nil
	}
	if err := facts.Validate(); err != nil {
		return fmt.Errorf("%w: %v", lifecycle.ErrInvalidTransition, err)
	}
	return nil
}

func persistedTerminalFacts(execution ProvisioningExecutionRecord, fallback time.Time) (domain.ObservedFacts, time.Time) {
	if execution.LastObservation != nil {
		return execution.LastObservation.Resource, observedAt(execution.LastObservation.ObservedAt, observationTime(execution.LastObservedAt, fallback))
	}
	if execution.Submission != nil {
		return execution.Submission.Observation.Resource, observedAt(execution.Submission.Observation.ObservedAt, observationTime(execution.LastObservedAt, fallback))
	}
	return domain.ObservedFacts{}, observationTime(execution.LastObservedAt, fallback)
}

func sanitizePersistedFacts(execution *ProvisioningExecutionRecord) bool {
	changed := false
	if execution.LastObservation != nil {
		if validateObservedFacts(execution.LastObservation.Resource) != nil {
			observation := *execution.LastObservation
			observation.Resource = domain.ObservedFacts{}
			execution.LastObservation = &observation
			changed = true
		}
	}
	if execution.Submission != nil {
		if validateObservedFacts(execution.Submission.Observation.Resource) != nil {
			submission := *execution.Submission
			submission.Observation.Resource = domain.ObservedFacts{}
			execution.Submission = &submission
			changed = true
		}
	}
	return changed
}

func validatePersistedTerminalEvidence(execution ProvisioningExecutionRecord, resource domain.Resource, status domain.ResourceStatus, operation domain.Operation) error {
	evidence := persistedTerminalExecution(execution)
	if evidence == nil {
		return fmt.Errorf("%w: terminal attempt has no terminal execution evidence", ErrInvalidApplicationCall)
	}
	expected := provisioning.ExecutionStateFailed
	if execution.State == AttemptSucceeded {
		expected = provisioning.ExecutionStateSucceeded
	}
	if evidence.State != expected {
		return fmt.Errorf("%w: terminal attempt contradicts persisted execution evidence", ErrInvalidApplicationCall)
	}
	if execution.LastObservedAt.IsZero() {
		return fmt.Errorf("%w: terminal attempt has no effective observation timestamp", ErrInvalidApplicationCall)
	}
	if !operation.IsTerminal() && (execution.LastObservedAt.Before(resource.UpdatedAt()) || execution.LastObservedAt.Before(status.UpdatedAt())) {
		return fmt.Errorf("%w: terminal evidence predates current resource state", ErrInvalidApplicationCall)
	}
	if sourceObservedAt := persistedTerminalObservedAt(execution); !sourceObservedAt.IsZero() && !sourceObservedAt.Equal(execution.LastObservedAt) {
		return fmt.Errorf("%w: terminal source timestamp differs from effective observation time", ErrInvalidApplicationCall)
	}
	if operation.IsTerminal() && !operation.CompletedAt().Equal(execution.LastObservedAt) {
		return fmt.Errorf("%w: terminal operation and execution timestamps differ", ErrInvalidApplicationCall)
	}
	if evidence.State == provisioning.ExecutionStateFailed {
		failure := normalizeExecutionFailure(evidence.Failure)
		if execution.LastFailure == nil || execution.LastFailure.Kind != failure.Kind || execution.LastFailure.Reason != failure.Reason || execution.LastFailure.Message != failure.Message {
			return fmt.Errorf("%w: terminal failure evidence is inconsistent", ErrInvalidApplicationCall)
		}
	}
	if operation.State() == domain.OperationStateSucceeded && execution.State != AttemptSucceeded {
		return fmt.Errorf("%w: succeeded operation contradicts execution attempt", ErrInvalidApplicationCall)
	}
	if operation.State() == domain.OperationStateFailed && execution.State != AttemptFailed {
		return fmt.Errorf("%w: failed operation contradicts execution attempt", ErrInvalidApplicationCall)
	}
	if operation.State() == domain.OperationStateFailed {
		operationFailure, ok := operation.Failure()
		failure := normalizeExecutionFailure(evidence.Failure)
		if !ok || operationFailure.Reason() != failure.Reason || operationFailure.Message() != failure.Message {
			return fmt.Errorf("%w: failed operation details contradict execution evidence", ErrInvalidApplicationCall)
		}
	}
	return nil
}

func persistedTerminalExecution(execution ProvisioningExecutionRecord) *provisioning.Execution {
	if execution.LastObservation != nil && execution.LastObservation.Execution != nil {
		return execution.LastObservation.Execution
	}
	if execution.Submission != nil && execution.Submission.Observation.Execution != nil {
		return execution.Submission.Observation.Execution
	}
	return nil
}

func persistedTerminalObservedAt(execution ProvisioningExecutionRecord) time.Time {
	if execution.LastObservation != nil && execution.LastObservation.Execution != nil {
		return execution.LastObservation.ObservedAt
	}
	if execution.Submission != nil && execution.Submission.Observation.Execution != nil {
		return execution.Submission.Observation.ObservedAt
	}
	return time.Time{}
}

func validateExecutionContext(resource ResourceRecord, operation domain.Operation, execution ProvisioningExecutionRecord) error {
	if operation.ResourceID() != resource.Resource.ID() || execution.OperationID != operation.ID() || execution.ResourceID != resource.Resource.ID() || execution.ResourceType != resource.Resource.Type() || execution.Capability != operation.Capability() || execution.TargetGeneration != operation.TargetGeneration() || execution.ProvisionerRef != resource.ProvisionerRef {
		return fmt.Errorf("%w: provisioning execution associations are inconsistent", ErrInvalidApplicationCall)
	}
	if resource.Resource.Generation() == execution.TargetGeneration && !reflect.DeepEqual(execution.Spec.Values(), resource.Resource.Spec().Values()) {
		return fmt.Errorf("%w: provisioning execution intent does not match the target generation", ErrInvalidApplicationCall)
	}
	return nil
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
