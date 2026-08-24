// SPDX-License-Identifier: Apache-2.0

package crossplane

import (
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube"
)

// Curated failure reasons. Condition messages and other raw control-plane
// text never cross into these values.
const (
	reasonTargetIdentityConflict  = "TargetIdentityConflict"
	reasonTargetIdentityChanged   = "TargetIdentityChanged"
	reasonManagedTargetAbsent     = "ManagedTargetAbsent"
	reasonReconciliationFailed    = "TerminalReconciliationFailure"
	reasonControlPlaneUnavailable = "ControlPlaneUnavailable"
	reasonAccessDenied            = "AccessDenied"
	reasonAdmissionRejected       = "AdmissionRejected"
	reasonTargetKindUnregistered  = "TargetKindUnregistered"
)

// objectEvaluation is the normalized reading of one live XR. Presence is
// always Present here: evaluation happens only for objects that physically
// exist. Request correlation is a separate dimension handled by callers.
type objectEvaluation struct {
	readiness       domain.ResourceReadiness
	terminalFailure *provisioning.ExecutionFailure
	observedAt      time.Time
}

func presentFacts(readiness domain.ResourceReadiness) provisioning.ResourceObservation {
	return provisioning.ResourceObservation{
		Presence:  provisioning.ResourcePresencePresent,
		Readiness: readiness,
		Drift:     provisioning.ResourceDriftUnknown,
	}
}

func absentFacts() provisioning.ResourceObservation {
	return provisioning.ResourceObservation{
		Presence:  provisioning.ResourcePresenceNotFound,
		Readiness: provisioning.ResourceReadinessUnknown,
		Drift:     provisioning.ResourceDriftUnknown,
	}
}

func unknownFacts() provisioning.ResourceObservation {
	return provisioning.ResourceObservation{
		Presence:  provisioning.ResourcePresenceUnknown,
		Readiness: provisioning.ResourceReadinessUnknown,
		Drift:     provisioning.ResourceDriftUnknown,
	}
}

// evaluateObject derives normalized facts from a live, ownership-verified XR.
//
// Readiness is Ready only when every required condition reports True with
// condition-level freshness (observedGeneration equal to metadata.generation)
// AND the stamped Liftr target generation equals the requesting generation.
// Stale Crossplane evidence and stale Liftr generations are both honest
// Unknown values; active progress on the current generation is NotReady;
// termination is NotReady. Drift is never inferred from Synced=True.
func evaluateObject(object *kube.Object, binding *resolvedBinding, requestGeneration uint64) objectEvaluation {
	evaluation := objectEvaluation{
		readiness: domain.ResourceReadinessUnknown,
	}
	if object.Terminating() {
		evaluation.readiness = domain.ResourceReadinessNotReady
		return evaluation
	}
	stampedGeneration, ok := annotationTargetGeneration(object)
	if !ok || stampedGeneration != requestGeneration {
		// The XR's health evidence describes an older Liftr generation. It
		// must never mark the current generation Ready (execution or passive).
		return evaluation
	}
	generation := object.Generation()
	var newest time.Time
	for _, rule := range binding.readiness {
		if !rule.Required {
			continue
		}
		condition, found := conditionByType(object.Conditions(), rule.Type)
		if !found {
			return evaluation
		}
		switch conditionStatus(condition) {
		case "True":
			observed, fresh := conditionObservedGeneration(condition)
			if !fresh || observed != generation {
				// The condition's evidence predates the current desired
				// state; it says nothing usable about this generation.
				return evaluation
			}
			if transition := conditionLastTransition(condition); transition.After(newest) {
				newest = transition
			}
		case "False":
			if reason := conditionReason(condition); binding.isTerminalReason(reason) {
				evaluation.terminalFailure = &provisioning.ExecutionFailure{
					Kind:    provisioning.FailureExecution,
					Reason:  reasonReconciliationFailed,
					Message: "crossplane reported a terminal reconciliation failure reason",
				}
				evaluation.observedAt = conditionLastTransition(condition)
				return evaluation
			}
			evaluation.readiness = domain.ResourceReadinessNotReady
			if transition := conditionLastTransition(condition); transition.After(newest) {
				newest = transition
			}
		default:
			return evaluation
		}
	}
	evaluation.readiness = domain.ResourceReadinessReady
	evaluation.observedAt = newest
	return evaluation
}

func (b *resolvedBinding) isTerminalReason(reason string) bool {
	_, terminal := b.terminalReasons[reason]
	return terminal
}

func conditionByType(conditions []map[string]any, conditionType string) (map[string]any, bool) {
	for _, condition := range conditions {
		if conditionTypeOf(condition) == conditionType {
			return condition, true
		}
	}
	return nil, false
}

func conditionTypeOf(condition map[string]any) string {
	value, _ := condition["type"].(string)
	return value
}

func conditionStatus(condition map[string]any) string {
	value, _ := condition["status"].(string)
	return value
}

func conditionReason(condition map[string]any) string {
	value, _ := condition["reason"].(string)
	return value
}

// conditionObservedGeneration reads the standard condition freshness field.
// A missing or non-numeric field is reported as unfresh: without an explicit
// generation marker Liftr refuses to treat the condition as current.
func conditionObservedGeneration(condition map[string]any) (uint64, bool) {
	value, exists := condition["observedGeneration"]
	if !exists {
		return 0, false
	}
	switch typed := value.(type) {
	case float64:
		if typed < 0 || typed != float64(uint64(typed)) {
			return 0, false
		}
		return uint64(typed), true
	case int64:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	case int:
		if typed < 0 {
			return 0, false
		}
		return uint64(typed), true
	default:
		return 0, false
	}
}

func conditionLastTransition(condition map[string]any) time.Time {
	value, _ := condition["lastTransitionTime"].(string)
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}
