// SPDX-License-Identifier: Apache-2.0

package application

import (
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

// This file implements the pure, application-owned RecoveryPlanner
// (ADR-0021). Given a diagnostic snapshot it deterministically answers what
// is currently known and which recovery actions, if any, are safe. It has no
// repositories, no auth, no HTTP, no clock, no policy, no provisioner calls,
// and no mutation: once supplied a snapshot it is repository-free. Mutation
// paths rebuild the snapshot under locks and re-run these functions inside
// the admission transaction; client-presented assessments are never trusted.

// RecoveryState is the closed planner outcome vocabulary.
type RecoveryState string

const (
	RecoveryNoActionNeeded           RecoveryState = "no_action_needed"
	RecoverySafeObserve              RecoveryState = "safe_observe"
	RecoverySafePassiveObserve       RecoveryState = "safe_passive_observe"
	RecoverySafeRecoverDeadWork      RecoveryState = "safe_recover_dead_work"
	RecoveryUserRetryAvailable       RecoveryState = "user_retry_available"
	RecoveryAmbiguousWaitingEvidence RecoveryState = "ambiguous_waiting_for_evidence"
	RecoveryManualInterventionNeeded RecoveryState = "manual_intervention_required"
	RecoveryUnsupportedRepair        RecoveryState = "unsupported_repair"
	RecoveryTerminal                 RecoveryState = "terminal"
	RecoverySuperseded               RecoveryState = "superseded"
)

// DiagnosticReason is a stable runbook code. Raw provider diagnostics never
// become reasons (ADR-0021).
type DiagnosticReason string

const (
	ReasonOperationTerminal          DiagnosticReason = "OPERATION_TERMINAL"
	ReasonExecutionRecordMissing     DiagnosticReason = "EXECUTION_RECORD_MISSING"
	ReasonExecutionNotObservable     DiagnosticReason = "EXECUTION_NOT_OBSERVABLE"
	ReasonExecutionEvidencePending   DiagnosticReason = "EXECUTION_TERMINAL_EVIDENCE_PENDING"
	ReasonAmbiguousExecution         DiagnosticReason = "AMBIGUOUS_EXECUTION"
	ReasonObservationInFlight        DiagnosticReason = "OBSERVATION_IN_FLIGHT"
	ReasonUserRetryAvailable         DiagnosticReason = "USER_RETRY_AVAILABLE"
	ReasonResourceDeleted            DiagnosticReason = "RESOURCE_DELETED"
	ReasonActiveOperationPresent     DiagnosticReason = "ACTIVE_OPERATION_PRESENT"
	ReasonDeadDispatchPreSubmit      DiagnosticReason = "DEAD_DISPATCH_PRE_SUBMIT"
	ReasonDeadDispatchViaObserve     DiagnosticReason = "DEAD_DISPATCH_RECOVERY_VIA_OBSERVE"
	ReasonRegistrationMissing        DiagnosticReason = "HISTORICAL_REGISTRATION_MISSING"
	ReasonWorkAlreadyActive          DiagnosticReason = "EQUIVALENT_WORK_ALREADY_ACTIVE"
	ReasonWorkNotDead                DiagnosticReason = "WORK_NOT_DEAD"
	ReasonOutputRecoveryPending      DiagnosticReason = "OUTPUT_RESOLUTION_PENDING"
	ReasonManualInterventionRequired DiagnosticReason = "MANUAL_INTERVENTION_REQUIRED"
)

// OperatorActionKind is the closed set of recovery actions the planner may
// allow.
type OperatorActionKind string

const (
	ActionKindTriggerObserve        OperatorActionKind = "trigger_observe"
	ActionKindTriggerPassiveObserve OperatorActionKind = "trigger_passive_observe"
	ActionKindRecoverDeadWork       OperatorActionKind = "recover_dead_work"
)

// RecoveryAssessment is the closed planner result. It is a value, not a bool
// or free-form string (ADR-0021).
type RecoveryAssessment struct {
	State          RecoveryState
	Reasons        []DiagnosticReason
	AllowedActions []OperatorActionKind
}

func assessment(state RecoveryState, reasons []DiagnosticReason, actions ...OperatorActionKind) RecoveryAssessment {
	if len(reasons) == 0 {
		reasons = []DiagnosticReason{}
	}
	if len(actions) == 0 {
		actions = []OperatorActionKind{}
	}
	return RecoveryAssessment{State: state, Reasons: reasons, AllowedActions: actions}
}

// executionObservationSummary is the curated execution view the planner may
// reason about. It deliberately excludes spec, handles, observations, and
// failure messages.
type executionObservationSummary struct {
	State               ProvisioningAttemptState
	Correlation         provisioning.RequestCorrelation
	OutputResolution    OutputResolution
	IsOutputRecovery    bool
	AcceptanceConfirmed bool
}

// observableForObserve reports whether canonical Observe scheduling is legal
// for this execution state, exactly mirroring the durable scheduling gate in
// scheduleOperationObservation. Only states whose backend truth is actually
// outstanding are observable; Pending/Dispatching work has not been submitted
// yet and terminal evidence needs no fresh observation to schedule.
func (s executionObservationSummary) observableForObserve() bool {
	switch s.State {
	case AttemptUnknown, AttemptAccepted:
		return true
	case AttemptSucceeded:
		return s.OutputResolution == OutputResolutionPending
	default:
		return false
	}
}

// OperationRecoverySnapshot is everything the planner may know about one
// Operation for trigger-observe decisions.
type OperationRecoverySnapshot struct {
	OperationState domain.OperationState
	HasExecution   bool
	Execution      executionObservationSummary
	// ActiveObserveWork reports that an equivalent Observe work item is
	// currently Pending or Leased for this Operation.
	ActiveObserveWork bool
	// RegistrationAvailable reports that the execution's ProvisionerRef still
	// resolves against current composition. Unresolvable registrations make
	// scheduled observation spin forever, so they are manual interventions.
	RegistrationAvailable bool
}

// operationStateIsTerminal mirrors domain.Operation.IsTerminal for state-only
// planner inputs.
func operationStateIsTerminal(state domain.OperationState) bool {
	switch state {
	case domain.OperationStateSucceeded, domain.OperationStateFailed, domain.OperationStateCanceled:
		return true
	default:
		return false
	}
}

// PlanOperationObserve classifies whether scheduling one fresh Observe for an
// existing admitted Operation is safe.
func PlanOperationObserve(s OperationRecoverySnapshot) RecoveryAssessment {
	if operationStateIsTerminal(s.OperationState) {
		return assessment(RecoveryTerminal, []DiagnosticReason{ReasonOperationTerminal})
	}
	if !s.HasExecution {
		return assessment(RecoveryManualInterventionNeeded, []DiagnosticReason{ReasonExecutionRecordMissing})
	}
	if !s.RegistrationAvailable {
		return assessment(RecoveryManualInterventionNeeded, []DiagnosticReason{ReasonRegistrationMissing})
	}
	exec := s.Execution
	switch {
	case exec.observableForObserve():
		state := RecoveryState(RecoverySafeObserve)
		reasons := []DiagnosticReason{}
		if exec.Correlation == provisioning.RequestCorrelationUnknown {
			state = RecoveryAmbiguousWaitingEvidence
			reasons = append(reasons, ReasonAmbiguousExecution)
		}
		if exec.State == AttemptSucceeded {
			reasons = append(reasons, ReasonOutputRecoveryPending)
		}
		if s.ActiveObserveWork {
			reasons = append(reasons, ReasonObservationInFlight)
			return assessment(state, reasons)
		}
		return assessment(state, reasons, ActionKindTriggerObserve)
	case exec.State == AttemptFailed:
		// Terminal backend evidence exists while the Operation is somehow still
		// nonterminal. Settling requires fenced worker machinery, not operator
		// observation scheduling.
		return assessment(RecoveryManualInterventionNeeded, []DiagnosticReason{ReasonExecutionEvidencePending})
	default:
		// Pending or Dispatching: nothing has been submitted yet; the dispatch
		// chain owns observation scheduling.
		return assessment(RecoveryNoActionNeeded, []DiagnosticReason{ReasonExecutionNotObservable})
	}
}

// ResourcePassiveObserveSnapshot is what the planner may know about one
// Resource for passive-observe decisions.
type ResourcePassiveObserveSnapshot struct {
	ResourceState domain.ResourceState
	// ActiveOperation reports any nonterminal Operation for the Resource.
	ActiveOperation bool
	// ActivePassiveObserveWork reports an equivalent PassiveObserve item that
	// is currently Pending or Leased.
	ActivePassiveObserveWork bool
	RegistrationAvailable    bool
}

// PlanResourcePassiveObserve classifies scheduling one fresh PassiveObserve.
// Deleted tombstones are refused unconditionally: there must be no provider
// activity after lifecycle-terminal deletion (ADR-0021).
func PlanResourcePassiveObserve(s ResourcePassiveObserveSnapshot) RecoveryAssessment {
	if s.ResourceState == domain.ResourceStateDeleted {
		return assessment(RecoveryUnsupportedRepair, []DiagnosticReason{ReasonResourceDeleted})
	}
	if !s.RegistrationAvailable {
		return assessment(RecoveryManualInterventionNeeded, []DiagnosticReason{ReasonRegistrationMissing})
	}
	if s.ActiveOperation {
		return assessment(RecoveryNoActionNeeded, []DiagnosticReason{ReasonActiveOperationPresent})
	}
	if s.ActivePassiveObserveWork {
		return assessment(RecoveryNoActionNeeded, []DiagnosticReason{ReasonObservationInFlight})
	}
	return assessment(RecoverySafePassiveObserve, nil, ActionKindTriggerPassiveObserve)
}

// DeadWorkKindSnapshot carries the current-truth inputs for one Dead work row
// classification. The Dead row itself contributes only its Kind: recovery
// always derives from CURRENT durable aggregate state and never replays the
// dead payload (ADR-0021).
type DeadWorkKindSnapshot struct {
	Kind  OutboxKind
	State OutboxState
	// OperationTarget marks work bound to an Operation (Drive, Dispatch,
	// Observe). PassiveObserve targets a Resource instead.
	OperationTarget bool
	// OperationState is meaningful only when OperationTarget.
	OperationState domain.OperationState
	Execution      executionObservationSummary
	HasExecution   bool
	// ResourceState is meaningful only when !OperationTarget.
	ResourceState domain.ResourceState
	// ActiveEquivalentWork reports an active (Pending/Leased) row of the same
	// kind for the same aggregate.
	ActiveEquivalentWork  bool
	ActiveOperation       bool
	Superseded            bool
	RegistrationAvailable bool
	TargetVersion         uint64
	ExecutionVersion      uint64
}

// PlanDeadWorkRecovery classifies recovering one Dead outbox row through NEW
// current-state work. The mappings are kind-specific by design:
//
//   - Dead Dispatch NEVER mints a Dispatch. If the execution is safely
//     observable the recovery schedules a fresh Observe (the same outcome as
//     expired-Dispatch ambiguity recovery); if the attempt was provably or
//     possibly pre-submit with nothing observable, no automated action exists
//     and this is manual intervention (accepted M20 gap).
//   - Dead Observe yields a fresh Observe while execution stays observable.
//   - Dead PassiveObserve yields a fresh PassiveObserve for nonterminal
//     Resources.
//   - Dead Drive yields ONE fresh Drive evaluation from CURRENT authoritative
//     state. Drive is safe to re-evaluate because its handler reloads the
//     aggregate, settles stale versions, and derives next work from current
//     state; its message carries no decision-bearing payload. The pinned
//     architectural test TestDriveMessageCarriesNoDecisionBearingPayload
//     fails if that assumption ever changes.
func PlanDeadWorkRecovery(s DeadWorkKindSnapshot) RecoveryAssessment {
	if s.State != OutboxDead {
		return assessment(RecoveryNoActionNeeded, []DiagnosticReason{ReasonWorkNotDead})
	}
	if s.OperationTarget {
		if operationStateIsTerminal(s.OperationState) {
			return assessment(RecoverySuperseded, []DiagnosticReason{ReasonOperationTerminal})
		}
		if s.Superseded {
			return assessment(RecoverySuperseded, []DiagnosticReason{ReasonManualInterventionRequired})
		}
		if (s.Kind == OutboxDispatch || s.Kind == OutboxObserve) && !s.HasExecution {
			return assessment(RecoveryManualInterventionNeeded, []DiagnosticReason{ReasonExecutionRecordMissing})
		}
		if !s.RegistrationAvailable {
			return assessment(RecoveryManualInterventionNeeded, []DiagnosticReason{ReasonRegistrationMissing})
		}
		if s.ActiveEquivalentWork && s.Kind != OutboxDispatch {
			// An equivalent active item already owns the aggregate. Dispatch is
			// excluded because dispatch recovery never creates Dispatch work;
			// active-dispatch presence is handled by the kind branches below.
			return assessment(RecoveryNoActionNeeded, []DiagnosticReason{ReasonWorkAlreadyActive})
		}
		switch s.Kind {
		case OutboxDispatch:
			if !s.Execution.observableForObserve() {
				// Pre-submit or unsettled boundary with nothing observable: the
				// conservative accepted M20 gap. No restart primitive exists.
				return assessment(RecoveryManualInterventionNeeded, []DiagnosticReason{
					ReasonDeadDispatchPreSubmit, ReasonManualInterventionRequired,
				})
			}
			state := RecoverySafeRecoverDeadWork
			reasons := []DiagnosticReason{ReasonDeadDispatchViaObserve}
			if s.Execution.Correlation == provisioning.RequestCorrelationUnknown {
				state = RecoveryAmbiguousWaitingEvidence
				reasons = append(reasons, ReasonAmbiguousExecution)
			}
			if s.ActiveEquivalentWork {
				// An active observe/dispatch chain already runs for the attempt.
				reasons = append(reasons, ReasonObservationInFlight)
				return assessment(RecoveryNoActionNeeded, reasons)
			}
			return assessment(state, reasons, ActionKindRecoverDeadWork)
		case OutboxObserve:
			if !s.Execution.observableForObserve() {
				return assessment(RecoveryNoActionNeeded, []DiagnosticReason{ReasonExecutionNotObservable})
			}
			if s.ActiveEquivalentWork {
				return assessment(RecoveryNoActionNeeded, []DiagnosticReason{ReasonObservationInFlight})
			}
			return assessment(RecoverySafeRecoverDeadWork, nil, ActionKindRecoverDeadWork)
		case OutboxDrive:
			if s.ActiveEquivalentWork {
				return assessment(RecoveryNoActionNeeded, []DiagnosticReason{ReasonWorkAlreadyActive})
			}
			// Fresh evaluation from current state is safe regardless of the
			// execution phase: the drive handler re-checks version, terminality,
			// and phase legality transactionally before emitting anything.
			return assessment(RecoverySafeRecoverDeadWork, nil, ActionKindRecoverDeadWork)
		default:
			return assessment(RecoveryUnsupportedRepair, []DiagnosticReason{ReasonManualInterventionRequired})
		}
	}
	// Resource-targeted dead work.
	if s.Kind != OutboxPassiveObserve {
		return assessment(RecoveryUnsupportedRepair, []DiagnosticReason{ReasonManualInterventionRequired})
	}
	if s.ActiveOperation {
		return assessment(RecoveryNoActionNeeded, []DiagnosticReason{ReasonActiveOperationPresent})
	}
	if s.ResourceState == domain.ResourceStateDeleted {
		return assessment(RecoveryUnsupportedRepair, []DiagnosticReason{ReasonResourceDeleted})
	}
	if !s.RegistrationAvailable {
		return assessment(RecoveryManualInterventionNeeded, []DiagnosticReason{ReasonRegistrationMissing})
	}
	if s.ActiveEquivalentWork {
		return assessment(RecoveryNoActionNeeded, []DiagnosticReason{ReasonWorkAlreadyActive})
	}
	return assessment(RecoverySafeRecoverDeadWork, nil, ActionKindRecoverDeadWork)
}
