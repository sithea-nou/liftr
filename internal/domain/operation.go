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
	requestedAt      time.Time
	startedAt        time.Time
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
		requestedAt:      requestedAt,
	}, nil
}

func (o Operation) ID() OperationID          { return o.id }
func (o Operation) ResourceID() ResourceID   { return o.resourceID }
func (o Operation) Capability() Capability   { return o.capability }
func (o Operation) TargetGeneration() uint64 { return o.targetGeneration }
func (o Operation) State() OperationState    { return o.state }
func (o Operation) RequestedAt() time.Time   { return o.requestedAt }
func (o Operation) StartedAt() time.Time     { return o.startedAt }
func (o Operation) CompletedAt() time.Time   { return o.completedAt }

func (o Operation) Failure() (OperationFailure, bool) {
	if o.failure == nil {
		return OperationFailure{}, false
	}
	return *o.failure, true
}

func (o *Operation) Start(startedAt time.Time) error {
	if o.state != OperationStatePending {
		return fmt.Errorf("%w: cannot start operation in state %s", ErrInvalidOperationTransition, o.state)
	}
	if err := o.validateTransitionTime(startedAt); err != nil {
		return err
	}
	o.state = OperationStateRunning
	o.startedAt = startedAt
	return nil
}

func (o *Operation) Succeed(completedAt time.Time) error {
	if o.state != OperationStateRunning {
		return fmt.Errorf("%w: cannot succeed operation in state %s", ErrInvalidOperationTransition, o.state)
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
	if !o.startedAt.IsZero() && at.Before(o.startedAt) {
		return fmt.Errorf("operation completion cannot precede its start")
	}
	return nil
}
