// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type OperationID string

type OperationState string

const (
	OperationStatePending   OperationState = "Pending"
	OperationStateRunning   OperationState = "Running"
	OperationStateSucceeded OperationState = "Succeeded"
	OperationStateFailed    OperationState = "Failed"
	OperationStateCanceled  OperationState = "Canceled"
)

type OperationPhase string

const (
	OperationPhaseRequested  OperationPhase = "Requested"
	OperationPhaseValidating OperationPhase = "Validating"
	OperationPhasePlanning   OperationPhase = "Planning"
	OperationPhaseApplying   OperationPhase = "Applying"
	OperationPhaseDestroying OperationPhase = "Destroying"
)

var ErrInvalidOperationTransition = errors.New("invalid operation state transition")

type OperationFailure struct {
	reason  string
	message string
}

func (f OperationFailure) Reason() string  { return f.reason }
func (f OperationFailure) Message() string { return f.message }

// Operation represents one asynchronous action against a specific desired-state generation.
type Operation struct {
	id               OperationID
	resourceID       ResourceID
	capability       Capability
	targetGeneration uint64
	state            OperationState
	phase            OperationPhase
	requestedAt      time.Time
	startedAt        time.Time
	phaseChangedAt   time.Time
	completedAt      time.Time
	failure          *OperationFailure
}

func NewOperation(id OperationID, resourceID ResourceID, capability Capability, targetGeneration uint64, requestedAt time.Time) (Operation, error) {
	if strings.TrimSpace(string(id)) == "" {
		return Operation{}, fmt.Errorf("operation ID is required")
	}
	if strings.TrimSpace(string(resourceID)) == "" {
		return Operation{}, fmt.Errorf("resource ID is required")
	}
	if err := capability.validate(); err != nil {
		return Operation{}, err
	}
	if targetGeneration == 0 {
		return Operation{}, fmt.Errorf("operation target generation must be greater than zero")
	}
	if requestedAt.IsZero() {
		return Operation{}, fmt.Errorf("operation request time is required")
	}

	return Operation{
		id:               id,
		resourceID:       resourceID,
		capability:       capability,
		targetGeneration: targetGeneration,
		state:            OperationStatePending,
		phase:            OperationPhaseRequested,
		requestedAt:      requestedAt,
		phaseChangedAt:   requestedAt,
	}, nil
}

func (o Operation) ID() OperationID           { return o.id }
func (o Operation) ResourceID() ResourceID    { return o.resourceID }
func (o Operation) Capability() Capability    { return o.capability }
func (o Operation) TargetGeneration() uint64  { return o.targetGeneration }
func (o Operation) State() OperationState     { return o.state }
func (o Operation) Phase() OperationPhase     { return o.phase }
func (o Operation) RequestedAt() time.Time    { return o.requestedAt }
func (o Operation) StartedAt() time.Time      { return o.startedAt }
func (o Operation) PhaseChangedAt() time.Time { return o.phaseChangedAt }
func (o Operation) CompletedAt() time.Time    { return o.completedAt }

func (o Operation) IsTerminal() bool {
	switch o.state {
	case OperationStateSucceeded, OperationStateFailed, OperationStateCanceled:
		return true
	default:
		return false
	}
}

func (o Operation) Failure() (OperationFailure, bool) {
	if o.failure == nil {
		return OperationFailure{}, false
	}
	return *o.failure, true
}

func (o *Operation) Start(startedAt time.Time) error {
	return o.AdvancePhase(OperationPhaseValidating, startedAt)
}

// AdvancePhase applies a legal capability-specific execution transition.
func (o *Operation) AdvancePhase(next OperationPhase, changedAt time.Time) error {
	if o.IsTerminal() {
		return fmt.Errorf("%w: cannot change phase of operation in state %s", ErrInvalidOperationTransition, o.state)
	}
	if !o.canAdvanceTo(next) {
		return fmt.Errorf("%w: cannot move %s operation from phase %s to %s", ErrInvalidOperationTransition, o.capability, o.phase, next)
	}
	if err := o.validateTransitionTime(changedAt); err != nil {
		return err
	}

	if o.state == OperationStatePending {
		o.state = OperationStateRunning
		o.startedAt = changedAt
	}
	o.phase = next
	o.phaseChangedAt = changedAt
	return nil
}

func (o *Operation) Succeed(completedAt time.Time) error {
	if o.state != OperationStateRunning {
		return fmt.Errorf("%w: cannot succeed operation in state %s", ErrInvalidOperationTransition, o.state)
	}
	if !o.isFinalPhase() {
		return fmt.Errorf("%w: cannot succeed %s operation in phase %s", ErrInvalidOperationTransition, o.capability, o.phase)
	}
	return o.complete(OperationStateSucceeded, completedAt, nil)
}

func (o *Operation) Fail(reason, message string, completedAt time.Time) error {
	if o.state != OperationStatePending && o.state != OperationStateRunning {
		return fmt.Errorf("%w: cannot fail operation in state %s", ErrInvalidOperationTransition, o.state)
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("operation failure reason is required")
	}
	return o.complete(OperationStateFailed, completedAt, &OperationFailure{reason: reason, message: message})
}

func (o *Operation) Cancel(completedAt time.Time) error {
	if o.state != OperationStatePending && o.state != OperationStateRunning {
		return fmt.Errorf("%w: cannot cancel operation in state %s", ErrInvalidOperationTransition, o.state)
	}
	return o.complete(OperationStateCanceled, completedAt, nil)
}

func (o *Operation) complete(state OperationState, completedAt time.Time, failure *OperationFailure) error {
	if err := o.validateTransitionTime(completedAt); err != nil {
		return err
	}
	o.state = state
	o.completedAt = completedAt
	o.failure = failure
	return nil
}

func (o Operation) validateTransitionTime(at time.Time) error {
	if at.IsZero() {
		return fmt.Errorf("operation transition time is required")
	}
	if at.Before(o.requestedAt) {
		return fmt.Errorf("operation transition cannot precede its request")
	}
	if at.Before(o.phaseChangedAt) {
		return fmt.Errorf("operation transition cannot precede its current phase")
	}
	return nil
}

func (o Operation) canAdvanceTo(next OperationPhase) bool {
	if o.state == OperationStatePending {
		return o.phase == OperationPhaseRequested && next == OperationPhaseValidating
	}
	if o.state != OperationStateRunning {
		return false
	}

	switch o.capability {
	case CapabilityCreate, CapabilityUpdate:
		return (o.phase == OperationPhaseValidating && next == OperationPhasePlanning) ||
			(o.phase == OperationPhasePlanning && next == OperationPhaseApplying)
	case CapabilityDelete:
		return o.phase == OperationPhaseValidating && next == OperationPhaseDestroying
	default:
		return false
	}
}

func (o Operation) isFinalPhase() bool {
	switch o.capability {
	case CapabilityCreate, CapabilityUpdate:
		return o.phase == OperationPhaseApplying
	case CapabilityDelete:
		return o.phase == OperationPhaseDestroying
	default:
		return false
	}
}
