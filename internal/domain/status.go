// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"fmt"
	"strings"
	"time"
)

type ResourceState string

const (
	ResourceStateUnknown  ResourceState = "Unknown"
	ResourceStatePending  ResourceState = "Pending"
	ResourceStateReady    ResourceState = "Ready"
	ResourceStateDeleting ResourceState = "Deleting"
	ResourceStateDeleted  ResourceState = "Deleted"
	ResourceStateFailed   ResourceState = "Failed"
)

func (s ResourceState) validate() error {
	switch s {
	case ResourceStateUnknown, ResourceStatePending, ResourceStateReady, ResourceStateDeleting, ResourceStateDeleted, ResourceStateFailed:
		return nil
	default:
		return fmt.Errorf("invalid resource state %q", s)
	}
}

type ConditionStatus string

const (
	ConditionStatusTrue    ConditionStatus = "True"
	ConditionStatusFalse   ConditionStatus = "False"
	ConditionStatusUnknown ConditionStatus = "Unknown"
)

func (s ConditionStatus) validate() error {
	switch s {
	case ConditionStatusTrue, ConditionStatusFalse, ConditionStatusUnknown:
		return nil
	default:
		return fmt.Errorf("invalid condition status %q", s)
	}
}

// Condition represents a normalized current fact such as Healthy, Drifted, or Reconciling.
type Condition struct {
	typeName           string
	status             ConditionStatus
	reason             string
	message            string
	observedGeneration uint64
	lastTransitionAt   time.Time
}

func NewCondition(typeName string, status ConditionStatus, reason, message string, observedGeneration uint64, lastTransitionAt time.Time) (Condition, error) {
	condition := Condition{
		typeName:           typeName,
		status:             status,
		reason:             reason,
		message:            message,
		observedGeneration: observedGeneration,
		lastTransitionAt:   lastTransitionAt,
	}
	if err := condition.validate(); err != nil {
		return Condition{}, err
	}
	return condition, nil
}

func (c Condition) validate() error {
	if strings.TrimSpace(c.typeName) == "" {
		return fmt.Errorf("condition type is required")
	}
	if err := c.status.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.reason) == "" {
		return fmt.Errorf("condition reason is required")
	}
	if c.lastTransitionAt.IsZero() {
		return fmt.Errorf("condition transition time is required")
	}
	return nil
}

func (c Condition) Type() string                { return c.typeName }
func (c Condition) Status() ConditionStatus     { return c.status }
func (c Condition) Reason() string              { return c.reason }
func (c Condition) Message() string             { return c.message }
func (c Condition) ObservedGeneration() uint64  { return c.observedGeneration }
func (c Condition) LastTransitionAt() time.Time { return c.lastTransitionAt }

// ResourceStatus is Liftr's normalized observation of a Resource.
// ObservedGeneration is the highest desired generation Liftr has evaluated
// during lifecycle processing; it does not imply successful reconciliation.
type ResourceStatus struct {
	resourceID         ResourceID
	observedGeneration uint64
	state              ResourceState
	conditions         []Condition
	updatedAt          time.Time
}

func NewResourceStatus(resourceID ResourceID, observedGeneration uint64, state ResourceState, conditions []Condition, updatedAt time.Time) (ResourceStatus, error) {
	if strings.TrimSpace(string(resourceID)) == "" {
		return ResourceStatus{}, fmt.Errorf("resource ID is required")
	}
	if err := state.validate(); err != nil {
		return ResourceStatus{}, err
	}
	if updatedAt.IsZero() {
		return ResourceStatus{}, fmt.Errorf("resource status update time is required")
	}

	seen := make(map[string]struct{}, len(conditions))
	cloned := make([]Condition, len(conditions))
	for i, condition := range conditions {
		if err := condition.validate(); err != nil {
			return ResourceStatus{}, fmt.Errorf("condition %d: %w", i, err)
		}
		if _, exists := seen[condition.typeName]; exists {
			return ResourceStatus{}, fmt.Errorf("condition type %q is duplicated", condition.typeName)
		}
		if condition.observedGeneration > observedGeneration {
			return ResourceStatus{}, fmt.Errorf("condition %q observes a generation newer than the resource status", condition.typeName)
		}
		if condition.lastTransitionAt.After(updatedAt) {
			return ResourceStatus{}, fmt.Errorf("condition %q transition cannot follow the resource status update", condition.typeName)
		}
		seen[condition.typeName] = struct{}{}
		cloned[i] = condition
	}

	return ResourceStatus{
		resourceID:         resourceID,
		observedGeneration: observedGeneration,
		state:              state,
		conditions:         cloned,
		updatedAt:          updatedAt,
	}, nil
}

func (s ResourceStatus) ResourceID() ResourceID     { return s.resourceID }
func (s ResourceStatus) ObservedGeneration() uint64 { return s.observedGeneration }
func (s ResourceStatus) State() ResourceState       { return s.state }
func (s ResourceStatus) UpdatedAt() time.Time       { return s.updatedAt }

func (s ResourceStatus) Conditions() []Condition {
	return append([]Condition(nil), s.conditions...)
}
