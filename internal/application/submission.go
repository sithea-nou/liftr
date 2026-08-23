// SPDX-License-Identifier: Apache-2.0

package application

import (
	"fmt"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/lifecycle"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

// SubmissionOutcome classifies how a provider submission resolves. The
// worker's dispatch recording and its observation recording share this
// interpretation so an in-flight execution cannot diverge across the two
// paths.
type SubmissionOutcome int

const (
	// SubmissionOutcomeAmbiguous means the attempt stays Unknown and is
	// re-observed because the outcome cannot be determined conclusively.
	SubmissionOutcomeAmbiguous SubmissionOutcome = iota
	// SubmissionOutcomeRejected is a conclusive preflight rejection.
	SubmissionOutcomeRejected
	// SubmissionOutcomeAccepted is a positively correlated nonterminal state.
	SubmissionOutcomeAccepted
	// SubmissionOutcomeSucceeded is a positively correlated terminal success.
	SubmissionOutcomeSucceeded
	// SubmissionOutcomeFailed is a positively correlated terminal failure.
	SubmissionOutcomeFailed
)

// ObservationOutcome classifies how an execution observation resolves.
type ObservationOutcome int

const (
	// ObservationOutcomeStale means the observation does not advance the
	// persisted evidence timeline and must be settled without application.
	ObservationOutcomeStale ObservationOutcome = iota
	// ObservationOutcomeRejected is a conclusive preflight rejection.
	ObservationOutcomeRejected
	// ObservationOutcomeRetry means the execution was not found and a fresh
	// attempt must be dispatched.
	ObservationOutcomeRetry
	// ObservationOutcomeObserve means the execution is still nonterminal and a
	// follow-up observation is scheduled.
	ObservationOutcomeObserve
	// ObservationOutcomeSucceeded is a positively correlated terminal success.
	ObservationOutcomeSucceeded
	// ObservationOutcomeFailed is a positively correlated terminal failure.
	ObservationOutcomeFailed
)

// Finish carries the terminal evidence needed to complete or fail an
// operation. BuildFinishEvidence turns it into a lifecycle result.
type Finish struct {
	Succeeded bool
	Reason    string
	Message   string
	Facts     domain.ObservedFacts
}

// EvidenceFresh reports whether provider evidence at evidenceAt advances the
// persisted observation timeline. Evidence at or before the last recorded
// observation is stale and must never regress the timeline or re-derive
// state from an older instant. Callers apply it only when the provider
// supplied an explicit observation timestamp; without one there is no
// evidence-time claim to protect.
func EvidenceFresh(persistedObservedAt, evidenceAt time.Time) bool {
	return persistedObservedAt.IsZero() || evidenceAt.After(persistedObservedAt)
}

// ConclusiveManagedAbsence reports whether a positively uncorrelated,
// pre-acceptance NotFound proves that the managed target of a cleanup delete
// is already absent. It requires every ambiguity guard at once: delete
// capability, fresh NotFound correlation, a conclusive NotFound execution
// failure, and no confirmed acceptance. Anything less — Unknown evidence,
// post-launch loss, or an accepted attempt — stays ambiguous and can never
// satisfy destruction.
func ConclusiveManagedAbsence(capability domain.Capability, correlation provisioning.RequestCorrelation, execution *provisioning.Execution, acceptanceConfirmed bool) bool {
	return capability == domain.CapabilityDelete &&
		correlation == provisioning.RequestCorrelationNotFound &&
		!acceptanceConfirmed &&
		execution != nil && execution.State == provisioning.ExecutionStateFailed &&
		execution.Failure != nil && execution.Failure.Kind == provisioning.FailureNotFound
}

// InterpretSubmission interprets a provider submission against the leased
// execution and attempt without touching storage. The worker persists the
// returned snapshots and, for terminal outcomes, builds the finish evidence.
func InterpretSubmission(execution ProvisioningExecutionRecord, attempt SubmissionAttemptRecord, submission provisioning.Submission, submitErr error, observedAt time.Time) (ProvisioningExecutionRecord, SubmissionAttemptRecord, SubmissionOutcome, *Finish, error) {
	backendExecution := submission.Observation.Execution
	if backendExecution != nil && providerEvidenceStale(execution, submission.Observation.ObservedAt) {
		execution.State = AttemptUnknown
		execution.Correlation = provisioning.RequestCorrelationUnknown
		execution.LastFailure = &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "StaleSubmissionEvidence", Message: "submission evidence does not advance the persisted observation timeline"}
		attempt.State = SubmissionAttemptUnknown
		attempt.Failure = execution.LastFailure
		return execution, attempt, SubmissionOutcomeAmbiguous, nil, nil
	}
	terminalEvidence := backendExecution != nil && (backendExecution.State == provisioning.ExecutionStateSucceeded || backendExecution.State == provisioning.ExecutionStateFailed)
	if factsErr := validateObservedFacts(submission.Observation.Resource); factsErr != nil {
		if terminalEvidence {
			submission.Observation.Resource = domain.ObservedFacts{}
		} else {
			execution.State = AttemptUnknown
			execution.Correlation = provisioning.RequestCorrelationUnknown
			execution.LastFailure = &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "MalformedObservedFacts", Message: factsErr.Error()}
			attempt.State = SubmissionAttemptUnknown
			attempt.Failure = execution.LastFailure
			return execution, attempt, SubmissionOutcomeAmbiguous, nil, nil
		}
	}
	execution.Submission = &submission
	execution.LastObservation = &submission.Observation
	execution.Correlation = submission.Observation.Correlation
	if !ValidCorrelation(execution.Correlation) {
		execution.Correlation = provisioning.RequestCorrelationUnknown
	}
	if backendExecution != nil && backendExecution.Handle != nil {
		execution.Handle = backendExecution.Handle
	}
	if backendExecution != nil && EvidenceFresh(execution.LastObservedAt, observedAt) {
		execution.LastObservedAt = observedAt
	}
	acceptProviderEvidence(&execution, submission.Observation.ObservedAt)
	validState := backendExecution != nil && ValidExecutionState(backendExecution.State)
	preflightRejected := validState && backendExecution.State == provisioning.ExecutionStateFailed && backendExecution.Failure != nil && submission.Observation.Correlation == provisioning.RequestCorrelationNotFound
	acceptedRequest := validState && submission.Observation.Correlation == provisioning.RequestCorrelationFound
	if (submitErr != nil && !terminalEvidence) || !validState || backendExecution.State == provisioning.ExecutionStateUnknown || (!preflightRejected && !acceptedRequest) {
		execution.State = AttemptUnknown
		if submission.Observation.Correlation == provisioning.RequestCorrelationFound {
			execution.AcceptanceConfirmed = true
		}
		execution.LastFailure = ExecutionFailureFrom(submitErr, backendExecution)
		attempt.State = SubmissionAttemptUnknown
		attempt.Failure = execution.LastFailure
		return execution, attempt, SubmissionOutcomeAmbiguous, nil, nil
	}
	attempt.ResolvedAt = observedAt
	if preflightRejected {
		// A conclusive pre-launch absence for a delete proves the destruction
		// objective is already satisfied: there is no managed target. This is
		// the only path that completes a lifecycle from NotFound evidence, and
		// it requires the full ConclusiveManagedAbsence guard.
		if ConclusiveManagedAbsence(execution.Capability, provisioning.RequestCorrelationNotFound, backendExecution, execution.AcceptanceConfirmed) {
			attempt.State = SubmissionAttemptRejected
			attempt.Failure = ExecutionFailureFrom(nil, backendExecution)
			execution.Correlation = provisioning.RequestCorrelationNotFound
			execution.AcceptanceConfirmed = false
			execution.State = AttemptSucceeded
			return execution, attempt, SubmissionOutcomeSucceeded,
				&Finish{Succeeded: true, Reason: ManagedTargetAbsentReason,
					Facts: domain.ObservedFacts{Presence: domain.ResourcePresenceNotFound, Readiness: domain.ResourceReadinessUnknown, Drift: domain.ResourceDriftUnknown}}, nil
		}
		attempt.State = SubmissionAttemptRejected
		attempt.Failure = ExecutionFailureFrom(nil, backendExecution)
		execution.Correlation = provisioning.RequestCorrelationNotFound
		execution.AcceptanceConfirmed = false
	} else {
		attempt.State = SubmissionAttemptAccepted
		execution.Correlation = provisioning.RequestCorrelationFound
		execution.AcceptanceConfirmed = true
	}
	switch backendExecution.State {
	case provisioning.ExecutionStateAccepted, provisioning.ExecutionStateRunning:
		execution.State = AttemptAccepted
		return execution, attempt, SubmissionOutcomeAccepted, nil, nil
	case provisioning.ExecutionStateSucceeded:
		execution.State = AttemptSucceeded
		return execution, attempt, SubmissionOutcomeSucceeded, &Finish{Succeeded: true, Reason: "SubmissionSucceeded", Facts: submission.Observation.Resource}, nil
	case provisioning.ExecutionStateFailed:
		execution.State = AttemptFailed
		failure := ExecutionFailureFrom(nil, backendExecution)
		execution.LastFailure = failure
		return execution, attempt, SubmissionOutcomeFailed, &Finish{Succeeded: false, Reason: failure.Reason, Message: failure.Message, Facts: submission.Observation.Resource}, nil
	default:
		return ProvisioningExecutionRecord{}, SubmissionAttemptRecord{}, 0, nil, fmt.Errorf("invalid execution state %q", backendExecution.State)
	}
}

// InterpretObservation interprets a provider observation against the leased
// execution and attempt without touching storage. A contradictory observation
// returns an error so the caller can surface it; stale evidence is reported as
// ObservationOutcomeStale so the caller can settle the message without acting.
func InterpretObservation(execution ProvisioningExecutionRecord, attempt SubmissionAttemptRecord, observation provisioning.ExecutionObservation, observedAt time.Time) (ProvisioningExecutionRecord, SubmissionAttemptRecord, ObservationOutcome, *Finish, error) {
	if observation.Correlation == provisioning.RequestCorrelationNotFound && observation.Execution != nil && observation.Execution.State != provisioning.ExecutionStateFailed {
		return ProvisioningExecutionRecord{}, SubmissionAttemptRecord{}, 0, nil, fmt.Errorf("contradictory observation reports NotFound with an execution")
	}
	stale := providerEvidenceStale(execution, observation.ObservedAt)
	if observation.ObservedAt.IsZero() && !execution.LastObservedAt.IsZero() && !observedAt.After(execution.LastObservedAt) {
		stale = true
	}
	acceptProviderEvidence(&execution, observation.ObservedAt)
	if stale {
		return execution, attempt, ObservationOutcomeStale, nil, nil
	}
	if factsErr := validateObservedFacts(observation.Resource); factsErr != nil {
		if observation.Execution != nil && (observation.Execution.State == provisioning.ExecutionStateSucceeded || observation.Execution.State == provisioning.ExecutionStateFailed) {
			observation.Resource = domain.ObservedFacts{}
		} else {
			return ProvisioningExecutionRecord{}, SubmissionAttemptRecord{}, 0, nil, fmt.Errorf("malformed observed facts: %v", factsErr)
		}
	}
	execution.LastObservation = &observation
	execution.LastObservedAt = observedAt
	execution.Correlation = observation.Correlation
	if observation.Correlation == provisioning.RequestCorrelationNotFound && observation.Execution != nil && observation.Execution.State == provisioning.ExecutionStateFailed {
		failure := ExecutionFailureFrom(nil, observation.Execution)
		execution.State = AttemptFailed
		execution.AcceptanceConfirmed = false
		execution.LastFailure = failure
		attempt.State = SubmissionAttemptRejected
		attempt.Failure = failure
		attempt.ResolvedAt = observedAt
		return execution, attempt, ObservationOutcomeRejected, &Finish{Succeeded: false, Reason: failure.Reason, Message: failure.Message, Facts: observation.Resource}, nil
	}
	if observation.Correlation == provisioning.RequestCorrelationNotFound && execution.State == AttemptUnknown && !execution.AcceptanceConfirmed {
		attempt.State = SubmissionAttemptNotFound
		attempt.ResolvedAt = observedAt
		execution.CurrentAttempt++
		execution.State = AttemptPending
		return execution, attempt, ObservationOutcomeRetry, nil, nil
	}
	if observation.Execution == nil {
		if observation.Correlation == provisioning.RequestCorrelationFound {
			execution.AcceptanceConfirmed = true
		}
		execution.State = AttemptUnknown
		return execution, attempt, ObservationOutcomeObserve, nil, nil
	}
	if observation.Correlation != provisioning.RequestCorrelationFound {
		execution.State = AttemptUnknown
		execution.LastFailure = &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "ExecutionCorrelationUnknown", Message: "terminal execution evidence was not positively correlated"}
		return execution, attempt, ObservationOutcomeObserve, nil, nil
	}
	if observation.Execution.Handle != nil {
		execution.Handle = observation.Execution.Handle
	}
	execution.AcceptanceConfirmed = true
	acceptProviderEvidence(&execution, observation.ObservedAt)
	switch observation.Execution.State {
	case provisioning.ExecutionStateAccepted, provisioning.ExecutionStateRunning, provisioning.ExecutionStateUnknown:
		execution.State = AttemptAccepted
		if observation.Execution.State == provisioning.ExecutionStateUnknown {
			execution.State = AttemptUnknown
		}
		return execution, attempt, ObservationOutcomeObserve, nil, nil
	case provisioning.ExecutionStateSucceeded:
		execution.State = AttemptSucceeded
		return execution, attempt, ObservationOutcomeSucceeded, &Finish{Succeeded: true, Reason: "ObservationSucceeded", Facts: observation.Resource}, nil
	case provisioning.ExecutionStateFailed:
		execution.State = AttemptFailed
		failure := ExecutionFailureFrom(nil, observation.Execution)
		execution.LastFailure = failure
		return execution, attempt, ObservationOutcomeFailed, &Finish{Succeeded: false, Reason: failure.Reason, Message: failure.Message, Facts: observation.Resource}, nil
	default:
		return ProvisioningExecutionRecord{}, SubmissionAttemptRecord{}, 0, nil, fmt.Errorf("invalid execution state %q", observation.Execution.State)
	}
}

// BuildFinishEvidence turns terminal evidence into a lifecycle result,
// including the post-operation observation when the provider reported facts.
// Malformed facts are sanitized so they can never strand or poison a terminal
// transition; the terminal outcome still applies without them.
func BuildFinishEvidence(engine lifecycle.Engine, operation domain.Operation, resource domain.Resource, status domain.ResourceStatus, finish Finish, at time.Time) (lifecycle.Result, error) {
	var result lifecycle.Result
	var err error
	if finish.Succeeded {
		result, err = engine.Complete(resource, status, operation, InternalEventID(operation.ID(), "succeeded"), at)
	} else {
		result, err = engine.Fail(resource, status, operation, finish.Reason, finish.Message, InternalEventID(operation.ID(), "failed"), at)
	}
	if err != nil {
		return lifecycle.Result{}, err
	}
	facts := sanitizeObservedFacts(finish.Facts)
	if hasObservedFacts(facts) {
		result.Status, err = engine.ApplyPostOperationObservation(resource, result.Status, facts, at)
		if err != nil {
			return lifecycle.Result{}, err
		}
	}
	return result, nil
}

// sanitizeObservedFacts drops malformed observed facts so terminal evidence
// never reaches the lifecycle engine in an unusable form.
func sanitizeObservedFacts(facts domain.ObservedFacts) domain.ObservedFacts {
	if validateObservedFacts(facts) != nil {
		return domain.ObservedFacts{}
	}
	return facts
}

// providerEvidenceStale reports whether backend-supplied evidence regresses
// the backend's own evidence timeline. Liftr receipt instants are a separate
// dimension and never gate backend evidence: provider clocks may be coarser
// than Liftr's, and a receipt recorded after a backend completion must not
// strand later correlated observations of that same completion.
func providerEvidenceStale(execution ProvisioningExecutionRecord, providerAt time.Time) bool {
	if providerAt.IsZero() {
		return false
	}
	return !execution.LastProviderObservedAt.IsZero() && !providerAt.After(execution.LastProviderObservedAt)
}

// acceptProviderEvidence records backend-supplied evidence time on the
// backend dimension of the execution timeline.
func acceptProviderEvidence(execution *ProvisioningExecutionRecord, providerAt time.Time) {
	if providerAt.IsZero() {
		return
	}
	if execution.LastProviderObservedAt.Before(providerAt) {
		execution.LastProviderObservedAt = providerAt
	}
}

// ValidCorrelation reports whether a provider request correlation is one of the
// supported contract values.
func ValidCorrelation(correlation provisioning.RequestCorrelation) bool {
	switch correlation {
	case provisioning.RequestCorrelationFound, provisioning.RequestCorrelationNotFound, provisioning.RequestCorrelationUnknown:
		return true
	default:
		return false
	}
}

// ValidExecutionState reports whether a provider execution state is one of the
// supported contract values.
func ValidExecutionState(state provisioning.ExecutionState) bool {
	switch state {
	case provisioning.ExecutionStateAccepted, provisioning.ExecutionStateRunning, provisioning.ExecutionStateSucceeded, provisioning.ExecutionStateFailed, provisioning.ExecutionStateUnknown:
		return true
	default:
		return false
	}
}

// ExecutionFailureFrom normalizes a provider failure, defaulting missing reason
// and message fields so failure evidence is always auditable.
func ExecutionFailureFrom(err error, execution *provisioning.Execution) *provisioning.ExecutionFailure {
	if execution != nil && execution.Failure != nil {
		failure := *execution.Failure
		if failure.Reason == "" {
			failure.Reason = "ExecutionFailed"
		}
		return &failure
	}
	reason := "SubmissionUnknown"
	message := "submission outcome is ambiguous"
	if err != nil {
		message = err.Error()
	}
	return &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: reason, Message: message}
}
