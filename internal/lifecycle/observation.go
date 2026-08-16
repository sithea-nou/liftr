// SPDX-License-Identifier: Apache-2.0

package lifecycle

import (
	"fmt"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
)

// ApplyObservation applies passive normalized facts without creating or
// completing an Operation. Operation outcomes require an Operation context.
func (Engine) ApplyObservation(resource domain.Resource, status domain.ResourceStatus, facts domain.ObservedFacts, observedAt time.Time) (domain.ResourceStatus, error) {
	return applyObservation(resource, status, facts, observedAt, false)
}

// ApplyPostOperationObservation records facts accompanying an explicit terminal
// execution without replacing the lifecycle-authoritative top-level outcome.
func (Engine) ApplyPostOperationObservation(resource domain.Resource, status domain.ResourceStatus, facts domain.ObservedFacts, observedAt time.Time) (domain.ResourceStatus, error) {
	return applyObservation(resource, status, facts, observedAt, true)
}

func applyObservation(resource domain.Resource, status domain.ResourceStatus, facts domain.ObservedFacts, observedAt time.Time, preserveState bool) (domain.ResourceStatus, error) {
	if err := facts.Validate(); err != nil {
		return domain.ResourceStatus{}, fmt.Errorf("%w: %v", ErrInvalidTransition, err)
	}
	if status.ResourceID() != resource.ID() {
		return domain.ResourceStatus{}, fmt.Errorf("%w: status belongs to a different resource", ErrInvalidTransition)
	}
	if observedAt.IsZero() || observedAt.Before(status.UpdatedAt()) || observedAt.Before(resource.UpdatedAt()) {
		return domain.ResourceStatus{}, fmt.Errorf("%w: observation time precedes current state", ErrInvalidTransition)
	}
	if active, _ := activeStatusOperation(status); active {
		return domain.ResourceStatus{}, fmt.Errorf("%w: passive observation cannot replace an active operation", ErrInvalidTransition)
	}
	preserveState = preserveState || status.State() == domain.ResourceStateDeleted || status.State() == domain.ResourceStateFailed

	state := status.State()
	conditions := status.Conditions()
	if facts.Presence != domain.ResourcePresenceUnknown || facts.Readiness != domain.ResourceReadinessUnknown || findCondition(conditions, ConditionReady) == nil {
		readyStatus, readyReason := readinessCondition(facts)
		var err error
		conditions, err = setCondition(conditions, ConditionReady, readyStatus, readyReason, "", status.ObservedGeneration(), observedAt)
		if err != nil {
			return domain.ResourceStatus{}, err
		}
	}
	driftStatus, driftReason := driftCondition(facts)
	conditions, err := setCondition(conditions, "Drifted", driftStatus, driftReason, "", status.ObservedGeneration(), observedAt)
	if err != nil {
		return domain.ResourceStatus{}, err
	}
	if facts.Drift == domain.ResourceDriftDrifted {
		reconciled := findCondition(conditions, ConditionReconciled)
		if reconciled == nil || reconciled.Status() == domain.ConditionStatusTrue {
			conditions, err = setCondition(conditions, ConditionReconciled, domain.ConditionStatusFalse, "DriftDetected", "", status.ObservedGeneration(), observedAt)
			if err != nil {
				return domain.ResourceStatus{}, err
			}
		}
	}

	if !preserveState {
		switch {
		case facts.Presence == domain.ResourcePresencePresent && facts.Readiness == domain.ResourceReadinessReady:
			state = domain.ResourceStateReady
		case facts.Presence == domain.ResourcePresenceNotFound || (facts.Presence == domain.ResourcePresencePresent && facts.Readiness == domain.ResourceReadinessNotReady):
			state = domain.ResourceStateUnknown
		}
	}

	return domain.NewResourceStatus(resource.ID(), status.ObservedGeneration(), state, conditions, observedAt)
}

func readinessCondition(facts domain.ObservedFacts) (domain.ConditionStatus, string) {
	switch {
	case facts.Presence == domain.ResourcePresenceNotFound:
		return domain.ConditionStatusFalse, "ResourceNotFound"
	case facts.Readiness == domain.ResourceReadinessReady:
		return domain.ConditionStatusTrue, "ResourceReady"
	case facts.Readiness == domain.ResourceReadinessNotReady:
		return domain.ConditionStatusFalse, "ResourceNotReady"
	default:
		return domain.ConditionStatusUnknown, "ReadinessUnknown"
	}
}

func driftCondition(facts domain.ObservedFacts) (domain.ConditionStatus, string) {
	switch facts.Drift {
	case domain.ResourceDriftInSync:
		return domain.ConditionStatusFalse, "InSync"
	case domain.ResourceDriftDrifted:
		return domain.ConditionStatusTrue, "DriftDetected"
	default:
		return domain.ConditionStatusUnknown, "DriftUnknown"
	}
}
