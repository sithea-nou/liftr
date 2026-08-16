// SPDX-License-Identifier: Apache-2.0

// Package lifecycle implements Liftr-owned, provisioner-neutral lifecycle rules.
package lifecycle

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
)

const (
	ConditionReady           = "Ready"
	ConditionReconciled      = "Reconciled"
	ConditionReconciling     = "Reconciling"
	ConditionOperationFailed = "OperationFailed"
	ConditionDeleted         = "Deleted"

	EventLifecycleRequested = "LifecycleRequested"
	EventPhaseChanged       = "LifecyclePhaseChanged"
	EventLifecycleSucceeded = "LifecycleSucceeded"
	EventLifecycleFailed    = "LifecycleFailed"
)

var (
	ErrInvalidTransition = errors.New("invalid lifecycle transition")
	ErrOperationActive   = errors.New("operation already active")
)

// Result contains the complete output of one accepted lifecycle transition.
type Result struct {
	Operation domain.Operation
	Status    domain.ResourceStatus
	Event     domain.Event
}

// Engine is a stateless lifecycle policy service. IDs and time are supplied by its caller.
type Engine struct{}

func (Engine) Request(
	resource domain.Resource,
	resourceType domain.ResourceType,
	status domain.ResourceStatus,
	latest *domain.Operation,
	capability domain.Capability,
	operationID domain.OperationID,
	eventID domain.EventID,
	requestedAt time.Time,
) (Result, error) {
	if err := validateResourceContext(resource, resourceType, status, requestedAt); err != nil {
		return Result{}, err
	}
	if !resourceType.Supports(capability) {
		return Result{}, fmt.Errorf("%w: resource type does not support capability %q", ErrInvalidTransition, capability)
	}
	if capability != domain.CapabilityCreate && capability != domain.CapabilityUpdate && capability != domain.CapabilityDelete {
		return Result{}, fmt.Errorf("%w: capability %q has no lifecycle flow", ErrInvalidTransition, capability)
	}
	if err := validateOperationSnapshot(resource, status, latest); err != nil {
		return Result{}, err
	}

	retry := false
	if latest != nil {
		if latest.ResourceID() != resource.ID() {
			return Result{}, fmt.Errorf("%w: latest operation belongs to a different resource", ErrInvalidTransition)
		}
		if latest.ID() == operationID {
			return Result{}, fmt.Errorf("%w: operation ID %q has already been used", ErrInvalidTransition, operationID)
		}
		if !latest.IsTerminal() {
			return Result{}, fmt.Errorf("%w: resource %q has operation %q", ErrOperationActive, resource.ID(), latest.ID())
		}
		retry = latest.State() == domain.OperationStateFailed && latest.Capability() == capability
	}

	if err := validateRequestPrecondition(resource, status, capability, retry); err != nil {
		return Result{}, err
	}

	operation, err := domain.NewOperation(operationID, resource.ID(), capability, resource.Generation(), requestedAt)
	if err != nil {
		return Result{}, err
	}

	conditions := status.Conditions()
	if status.State() == domain.ResourceStateReady {
		conditions, err = ensurePreviouslyReady(conditions, status.ObservedGeneration(), requestedAt)
		if err != nil {
			return Result{}, err
		}
	}
	conditions, err = setCondition(conditions, ConditionReconciling, domain.ConditionStatusTrue, requestReason(capability, retry), "", operation.TargetGeneration(), requestedAt)
	if err != nil {
		return Result{}, err
	}
	conditions, err = setCondition(conditions, ConditionReconciled, domain.ConditionStatusFalse, "LifecycleRequested", "", operation.TargetGeneration(), requestedAt)
	if err != nil {
		return Result{}, err
	}
	conditions, err = setCondition(conditions, ConditionOperationFailed, domain.ConditionStatusFalse, "NoFailure", "", operation.TargetGeneration(), requestedAt)
	if err != nil {
		return Result{}, err
	}

	state := status.State()
	switch capability {
	case domain.CapabilityCreate:
		state = domain.ResourceStatePending
		conditions, err = setCondition(conditions, ConditionReady, domain.ConditionStatusFalse, "CreatePending", "", operation.TargetGeneration(), requestedAt)
		if err == nil {
			conditions, err = setCondition(conditions, ConditionDeleted, domain.ConditionStatusFalse, "CreatePending", "", operation.TargetGeneration(), requestedAt)
		}
	case domain.CapabilityDelete:
		state = domain.ResourceStateDeleting
		conditions, err = setCondition(conditions, ConditionDeleted, domain.ConditionStatusFalse, "DeletePending", "", operation.TargetGeneration(), requestedAt)
	}
	if err != nil {
		return Result{}, err
	}

	updatedStatus, err := domain.NewResourceStatus(resource.ID(), operation.TargetGeneration(), state, conditions, requestedAt)
	if err != nil {
		return Result{}, err
	}
	event, err := domain.NewEvent(eventID, resource.ID(), operation.ID(), operation.TargetGeneration(), EventLifecycleRequested, requestReason(capability, retry), "", requestedAt)
	if err != nil {
		return Result{}, err
	}

	return Result{Operation: operation, Status: updatedStatus, Event: event}, nil
}

func (Engine) Advance(
	resource domain.Resource,
	status domain.ResourceStatus,
	operation domain.Operation,
	next domain.OperationPhase,
	eventID domain.EventID,
	changedAt time.Time,
) (Result, error) {
	if err := validateOperationContext(resource, status, operation, changedAt); err != nil {
		return Result{}, err
	}

	updatedOperation := operation
	if err := updatedOperation.AdvancePhase(next, changedAt); err != nil {
		return Result{}, err
	}

	conditions, err := setCondition(status.Conditions(), ConditionReconciling, domain.ConditionStatusTrue, string(next), "", operation.TargetGeneration(), changedAt)
	if err != nil {
		return Result{}, err
	}
	conditions, err = setCondition(conditions, ConditionReconciled, domain.ConditionStatusFalse, string(next), "", operation.TargetGeneration(), changedAt)
	if err != nil {
		return Result{}, err
	}
	updatedStatus, err := domain.NewResourceStatus(resource.ID(), status.ObservedGeneration(), status.State(), conditions, changedAt)
	if err != nil {
		return Result{}, err
	}
	event, err := domain.NewEvent(eventID, resource.ID(), operation.ID(), operation.TargetGeneration(), EventPhaseChanged, string(next), "", changedAt)
	if err != nil {
		return Result{}, err
	}

	return Result{Operation: updatedOperation, Status: updatedStatus, Event: event}, nil
}

func (Engine) Complete(
	resource domain.Resource,
	status domain.ResourceStatus,
	operation domain.Operation,
	eventID domain.EventID,
	completedAt time.Time,
) (Result, error) {
	if err := validateOperationContext(resource, status, operation, completedAt); err != nil {
		return Result{}, err
	}

	updatedOperation := operation
	if err := updatedOperation.Succeed(completedAt); err != nil {
		return Result{}, err
	}

	conditions := status.Conditions()
	conditions, err := setCondition(conditions, ConditionReconciled, domain.ConditionStatusTrue, "LifecycleSucceeded", "", operation.TargetGeneration(), completedAt)
	if err != nil {
		return Result{}, err
	}
	conditions, err = setCondition(conditions, ConditionOperationFailed, domain.ConditionStatusFalse, "NoFailure", "", operation.TargetGeneration(), completedAt)
	if err != nil {
		return Result{}, err
	}

	state := domain.ResourceStateReady
	switch operation.Capability() {
	case domain.CapabilityCreate:
		conditions, err = setCondition(conditions, ConditionReady, domain.ConditionStatusTrue, "LifecycleSucceeded", "", operation.TargetGeneration(), completedAt)
		if err == nil {
			conditions, err = setCondition(conditions, ConditionDeleted, domain.ConditionStatusFalse, "ResourceCreated", "", operation.TargetGeneration(), completedAt)
		}
	case domain.CapabilityUpdate:
		conditions, err = setCondition(conditions, ConditionReady, domain.ConditionStatusTrue, "LifecycleSucceeded", "", operation.TargetGeneration(), completedAt)
	case domain.CapabilityDelete:
		state = domain.ResourceStateDeleted
		conditions, err = setCondition(conditions, ConditionReady, domain.ConditionStatusFalse, "Deleted", "", operation.TargetGeneration(), completedAt)
		if err == nil {
			conditions, err = setCondition(conditions, ConditionDeleted, domain.ConditionStatusTrue, "DestructionSucceeded", "", operation.TargetGeneration(), completedAt)
		}
	default:
		return Result{}, fmt.Errorf("%w: capability %q has no completion semantics", ErrInvalidTransition, operation.Capability())
	}
	if err != nil {
		return Result{}, err
	}

	if resource.Generation() > operation.TargetGeneration() {
		conditions, err = setCondition(conditions, ConditionReconciled, domain.ConditionStatusFalse, "NewerGenerationPending", "", operation.TargetGeneration(), completedAt)
		if err == nil {
			conditions, err = setCondition(conditions, ConditionReconciling, domain.ConditionStatusFalse, "NewerGenerationPending", "", operation.TargetGeneration(), completedAt)
		}
	} else {
		conditions, err = setCondition(conditions, ConditionReconciling, domain.ConditionStatusFalse, "LifecycleSucceeded", "", operation.TargetGeneration(), completedAt)
	}
	if err != nil {
		return Result{}, err
	}

	updatedStatus, err := domain.NewResourceStatus(resource.ID(), status.ObservedGeneration(), state, conditions, completedAt)
	if err != nil {
		return Result{}, err
	}
	event, err := domain.NewEvent(eventID, resource.ID(), operation.ID(), operation.TargetGeneration(), EventLifecycleSucceeded, successReason(operation.Capability()), "", completedAt)
	if err != nil {
		return Result{}, err
	}

	return Result{Operation: updatedOperation, Status: updatedStatus, Event: event}, nil
}

func (Engine) Fail(
	resource domain.Resource,
	status domain.ResourceStatus,
	operation domain.Operation,
	reason string,
	message string,
	eventID domain.EventID,
	failedAt time.Time,
) (Result, error) {
	if err := validateOperationContext(resource, status, operation, failedAt); err != nil {
		return Result{}, err
	}

	updatedOperation := operation
	if err := updatedOperation.Fail(reason, message, failedAt); err != nil {
		return Result{}, err
	}

	conditions := status.Conditions()
	conditions, err := setCondition(conditions, ConditionReconciled, domain.ConditionStatusFalse, reason, message, operation.TargetGeneration(), failedAt)
	if err != nil {
		return Result{}, err
	}
	conditions, err = setCondition(conditions, ConditionOperationFailed, domain.ConditionStatusTrue, reason, message, operation.TargetGeneration(), failedAt)
	if err != nil {
		return Result{}, err
	}

	state := domain.ResourceStateFailed
	switch operation.Capability() {
	case domain.CapabilityCreate:
		conditions, err = setCondition(conditions, ConditionReady, domain.ConditionStatusFalse, reason, message, operation.TargetGeneration(), failedAt)
	case domain.CapabilityUpdate:
		state = domain.ResourceStateReady
	case domain.CapabilityDelete:
		state = domain.ResourceStateReady
		conditions, err = setCondition(conditions, ConditionDeleted, domain.ConditionStatusFalse, reason, message, operation.TargetGeneration(), failedAt)
	default:
		return Result{}, fmt.Errorf("%w: capability %q has no failure semantics", ErrInvalidTransition, operation.Capability())
	}
	if err != nil {
		return Result{}, err
	}

	if resource.Generation() > operation.TargetGeneration() {
		conditions, err = setCondition(conditions, ConditionReconciled, domain.ConditionStatusFalse, "NewerGenerationPending", "", operation.TargetGeneration(), failedAt)
		if err == nil {
			conditions, err = setCondition(conditions, ConditionReconciling, domain.ConditionStatusFalse, "NewerGenerationPending", "", operation.TargetGeneration(), failedAt)
		}
	} else {
		conditions, err = setCondition(conditions, ConditionReconciling, domain.ConditionStatusFalse, "OperationFailed", message, operation.TargetGeneration(), failedAt)
	}
	if err != nil {
		return Result{}, err
	}

	updatedStatus, err := domain.NewResourceStatus(resource.ID(), status.ObservedGeneration(), state, conditions, failedAt)
	if err != nil {
		return Result{}, err
	}
	event, err := domain.NewEvent(eventID, resource.ID(), operation.ID(), operation.TargetGeneration(), EventLifecycleFailed, reason, message, failedAt)
	if err != nil {
		return Result{}, err
	}

	return Result{Operation: updatedOperation, Status: updatedStatus, Event: event}, nil
}

func validateResourceContext(resource domain.Resource, resourceType domain.ResourceType, status domain.ResourceStatus, at time.Time) error {
	if resource.Type() != resourceType.Ref() {
		return fmt.Errorf("%w: resource type does not match its definition", ErrInvalidTransition)
	}
	if status.ResourceID() != resource.ID() {
		return fmt.Errorf("%w: status belongs to a different resource", ErrInvalidTransition)
	}
	if status.ObservedGeneration() > resource.Generation() {
		return fmt.Errorf("%w: observed generation exceeds desired generation", ErrInvalidTransition)
	}
	if at.IsZero() || at.Before(resource.UpdatedAt()) || at.Before(status.UpdatedAt()) {
		return fmt.Errorf("%w: transition time precedes current resource state", ErrInvalidTransition)
	}
	return nil
}

func validateOperationContext(resource domain.Resource, status domain.ResourceStatus, operation domain.Operation, at time.Time) error {
	if status.ResourceID() != resource.ID() || operation.ResourceID() != resource.ID() {
		return fmt.Errorf("%w: resource, status, and operation do not match", ErrInvalidTransition)
	}
	if operation.TargetGeneration() > resource.Generation() {
		return fmt.Errorf("%w: operation targets a future generation", ErrInvalidTransition)
	}
	if status.ObservedGeneration() != operation.TargetGeneration() {
		return fmt.Errorf("%w: active operation target does not match the evaluated generation", ErrInvalidTransition)
	}
	if at.IsZero() || at.Before(status.UpdatedAt()) {
		return fmt.Errorf("%w: transition time precedes current status", ErrInvalidTransition)
	}
	if !operation.IsTerminal() {
		statusActive, expectedCapability := activeStatusOperation(status)
		if !statusActive {
			return fmt.Errorf("%w: active operation %q is not represented by the resource status", ErrInvalidTransition, operation.ID())
		}
		if operation.Capability() != expectedCapability {
			return fmt.Errorf("%w: active operation capability %q does not match status capability %q", ErrInvalidTransition, operation.Capability(), expectedCapability)
		}
	}
	return nil
}

func validateRequestPrecondition(resource domain.Resource, status domain.ResourceStatus, capability domain.Capability, retry bool) error {
	switch capability {
	case domain.CapabilityCreate:
		if status.State() == domain.ResourceStateUnknown || status.State() == domain.ResourceStatePending {
			return nil
		}
		if status.State() == domain.ResourceStateFailed && retry {
			return nil
		}
		if status.State() == domain.ResourceStateDeleted && resource.Generation() > status.ObservedGeneration() {
			return nil
		}
	case domain.CapabilityUpdate:
		if status.State() == domain.ResourceStateReady && resource.Generation() > status.ObservedGeneration() {
			return nil
		}
		if status.State() == domain.ResourceStateReady && retry {
			return nil
		}
	case domain.CapabilityDelete:
		if status.State() == domain.ResourceStateReady {
			return nil
		}
	}
	return fmt.Errorf("%w: cannot request %q from state %s at generations desired=%d observed=%d", ErrInvalidTransition, capability, status.State(), resource.Generation(), status.ObservedGeneration())
}

func validateOperationSnapshot(resource domain.Resource, status domain.ResourceStatus, latest *domain.Operation) error {
	statusActive, expectedCapability := activeStatusOperation(status)
	operationActive := latest != nil && !latest.IsTerminal()

	if statusActive != operationActive {
		return fmt.Errorf("%w: status active=%t but operation active=%t", ErrInvalidTransition, statusActive, operationActive)
	}
	if latest == nil {
		return nil
	}
	if latest.ResourceID() != resource.ID() {
		return fmt.Errorf("%w: latest operation belongs to a different resource", ErrInvalidTransition)
	}
	if !operationActive {
		return nil
	}
	if latest.Capability() != expectedCapability {
		return fmt.Errorf("%w: active operation capability %q does not match expected capability %q", ErrInvalidTransition, latest.Capability(), expectedCapability)
	}
	if latest.TargetGeneration() != status.ObservedGeneration() {
		return fmt.Errorf("%w: active operation target generation %d does not match observed generation %d", ErrInvalidTransition, latest.TargetGeneration(), status.ObservedGeneration())
	}
	return nil
}

func activeStatusOperation(status domain.ResourceStatus) (bool, domain.Capability) {
	switch status.State() {
	case domain.ResourceStatePending:
		return true, domain.CapabilityCreate
	case domain.ResourceStateDeleting:
		return true, domain.CapabilityDelete
	case domain.ResourceStateReady:
		if condition := findCondition(status.Conditions(), ConditionReconciling); condition != nil && condition.Status() == domain.ConditionStatusTrue {
			return true, domain.CapabilityUpdate
		}
	}
	return false, ""
}

func ensurePreviouslyReady(conditions []domain.Condition, generation uint64, at time.Time) ([]domain.Condition, error) {
	ready := findCondition(conditions, ConditionReady)
	if ready != nil && ready.Status() == domain.ConditionStatusTrue {
		return conditions, nil
	}
	return setCondition(conditions, ConditionReady, domain.ConditionStatusTrue, "PreviouslyReady", "", generation, at)
}

func setCondition(conditions []domain.Condition, typeName string, status domain.ConditionStatus, reason, message string, generation uint64, at time.Time) ([]domain.Condition, error) {
	condition, err := domain.NewCondition(typeName, status, reason, message, generation, at)
	if err != nil {
		return nil, err
	}

	updated := append([]domain.Condition(nil), conditions...)
	for i := range updated {
		if updated[i].Type() == typeName {
			updated[i] = condition
			return updated, nil
		}
	}
	return append(updated, condition), nil
}

func findCondition(conditions []domain.Condition, typeName string) *domain.Condition {
	for i := range conditions {
		if conditions[i].Type() == typeName {
			condition := conditions[i]
			return &condition
		}
	}
	return nil
}

func requestReason(capability domain.Capability, retry bool) string {
	prefix := strings.ToUpper(string(capability[:1])) + string(capability[1:])
	if retry {
		return prefix + "RetryRequested"
	}
	return prefix + "Requested"
}

func successReason(capability domain.Capability) string {
	prefix := strings.ToUpper(string(capability[:1])) + string(capability[1:])
	return prefix + "Succeeded"
}
