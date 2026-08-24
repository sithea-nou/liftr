// SPDX-License-Identifier: Apache-2.0

package crossplane

import (
	"context"
	"fmt"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube"
)

func failedSubmission(kind provisioning.ExecutionFailureKind, reason string) provisioning.Submission {
	failure := &provisioning.ExecutionFailure{Kind: kind, Reason: reason, Message: "provisioning request was rejected before any infrastructure effect"}
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationNotFound,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateFailed, Failure: failure},
		Resource:    unknownFacts(),
	}}
}

func conflictedSubmission(facts provisioning.ResourceObservation, failure *provisioning.ExecutionFailure) provisioning.Submission {
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateFailed, Failure: failure},
		Resource:    facts,
	}}
}

func acceptedSubmission(handle provisioning.ExecutionHandle, facts provisioning.ResourceObservation) provisioning.Submission {
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle},
		Resource:    facts,
	}}
}

// unknownSubmission keeps the lease intact through ErrAmbiguousSubmission so
// lease-expiry recovery moves the attempt to Unknown and schedules Observe;
// it never re-executes anything.
func unknownSubmission(handle provisioning.ExecutionHandle, _ error) (provisioning.Submission, error) {
	failure := &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "SubmissionOutcomeUnknown", Message: "submission outcome could not be determined"}
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationUnknown,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateUnknown, Handle: &handle, Failure: failure},
		Resource:    unknownFacts(),
	}}, provisioning.ErrAmbiguousSubmission
}

func conflictedObservation(facts provisioning.ResourceObservation, failure *provisioning.ExecutionFailure, observedAt time.Time) provisioning.ExecutionObservation {
	return provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateFailed, Failure: failure},
		Resource:    facts,
		ObservedAt:  observedAt,
	}
}

func runningObservation(facts provisioning.ResourceObservation, handle provisioning.ExecutionHandle, observedAt time.Time) provisioning.ExecutionObservation {
	return provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateRunning, Handle: &handle},
		Resource:    facts,
		ObservedAt:  observedAt,
	}
}

// Observe reads backend truth and normalizes it. Physical presence, Liftr
// ownership, and request correlation are three distinct dimensions: an
// object that exists is Present even when it belongs to nobody, to another
// installation, or to an earlier Operation, and conclusive absence is
// reported only when the deterministic target is genuinely not there.
func (p *Provisioner) Observe(ctx context.Context, request provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	if err := request.Validate(); err != nil {
		return provisioning.ExecutionObservation{}, provisioning.ObservationError{Failure: provisioning.ExecutionFailure{
			Kind: provisioning.FailureInvalidRequest, Reason: "InvalidObservationRequest", Message: "observation request is invalid",
		}}
	}
	view := observationRequestView{request}
	passive := request.OperationID == ""
	binding, failure := p.resolveBinding(view)
	if failure != nil {
		return provisioning.ExecutionObservation{}, provisioning.ObservationError{Failure: provisioning.ExecutionFailure{
			Kind: failure.kind, Reason: failure.reason, Message: "observation capability is unsupported",
		}}
	}
	name := p.targetName(binding, request.ResourceID)
	handle := encodeHandle(binding, name, uidFromHandle(view))
	object, err := p.client.Get(ctx, binding.gvr, binding.namespace, name)
	if err != nil {
		if kube.IsNotFound(err) {
			// A 404 is ambiguous: object absent, or API kind unserved.
			// Absence conclusions are always state-changing (resubmission
			// authorization or deletion completion), so verify live that the
			// target API resource is currently served before reporting
			// anything definitive.
			switch p.resolveAbsence(ctx, binding) {
			case kindNotServed:
				if passive {
					return provisioning.ExecutionObservation{
						Correlation: provisioning.RequestCorrelationUnknown,
						Resource:    unknownFacts(),
					}, nil
				}
				// The API kind itself is gone: deletion cannot be proven and
				// resubmission must not fire. This is a control-plane
				// failure with a curated classification; the worker's
				// retryable observation-error path keeps the operation alive
				// for operator action.
				return observationUnavailable(reasonTargetKindUnregistered)
			case absenceUncertain:
				if passive {
					return provisioning.ExecutionObservation{
						Correlation: provisioning.RequestCorrelationUnknown,
						Resource:    unknownFacts(),
					}, nil
				}
				return observationUnavailable(reasonControlPlaneUnavailable)
			}
			if !passive && request.Capability == domain.CapabilityDelete {
				// Genuine physical absence with a served GVR: the destruction
				// objective is satisfied and there is nothing left to
				// correlate against.
				return deletionCompleteObservation(handle), nil
			}
			// Genuine physical absence. Passive observation reports bare
			// facts; execution observation reports an uncorrelated request
			// with no current execution, which authorizes the existing safe
			// resubmission rules only where the application allows them.
			facts := absentFacts()
			correlation := provisioning.RequestCorrelationUnknown
			if !passive {
				correlation = provisioning.RequestCorrelationNotFound
			}
			return provisioning.ExecutionObservation{Correlation: correlation, Resource: facts}, nil
		}
		reason := reasonControlPlaneUnavailable
		if kube.IsAPIError(err) && !kube.IsUnavailable(err) {
			reason = classifyAPIError(err).reason
		}
		return observationUnavailable(reason)
	}
	owner := p.identityFor(request.ResourceType, request.ResourceID)
	if !owner.verify(object) {
		if passive {
			// Passive messages complete after one shot and have no failure
			// channel that terminates safely, so foreign objects surface as
			// uncertain facts; every mutating path fails closed on the same
			// collision instead.
			return provisioning.ExecutionObservation{
				Correlation: provisioning.RequestCorrelationUnknown,
				Resource:    presentFacts(domain.ResourceReadinessUnknown),
			}, nil
		}
		return conflictedObservation(presentFacts(domain.ResourceReadinessUnknown), identityConflictFailure(reasonTargetIdentityConflict), time.Time{}), nil
	}
	expectedUID := uidFromHandle(view)
	if expectedUID != "" && object.UID() != expectedUID {
		// A UID learned from confirmed evidence no longer matches physical
		// reality: identity changed under this execution. Never adopt.
		return conflictedObservation(presentFacts(domain.ResourceReadinessUnknown), identityConflictFailure(reasonTargetIdentityChanged), time.Time{}), nil
	}
	// Record the now-confirmed physical identity on the returned handle so
	// every later observation of this execution is bound to exactly this
	// object instance.
	handle = encodeHandle(binding, name, object.UID())
	evaluation := evaluateObject(object, binding, request.TargetGeneration)
	if passive {
		// Passive observation verifies ownership and Liftr-generation
		// freshness, skips only OperationID correlation, and never claims
		// drift. A stale generation can never advance readiness.
		return provisioning.ExecutionObservation{
			Correlation: provisioning.RequestCorrelationUnknown,
			Resource:    presentFacts(evaluation.readiness),
			ObservedAt:  evaluation.observedAt,
		}, nil
	}
	if request.Capability == domain.CapabilityDelete {
		// Deletion completion is purely physical: termination stays Running
		// and only genuine absence completes the execution. Operation
		// correlation is deliberately ignored here so a stale annotation can
		// never be misread as a destroyed target.
		if object.Terminating() {
			return runningObservation(presentFacts(domain.ResourceReadinessNotReady), handle, evaluation.observedAt), nil
		}
		return runningObservation(presentFacts(evaluation.readiness), handle, evaluation.observedAt), nil
	}
	if !operationCorrelated(object, request.OperationID, request.TargetGeneration) {
		// Correction 1A: the XR physically exists and belongs to this
		// Resource, but the requesting Operation is not recorded on it. The
		// request is conclusively uncorrelated while presence stays Present;
		// the application's existing resubmission rules decide what may run.
		return provisioning.ExecutionObservation{
			Correlation: provisioning.RequestCorrelationNotFound,
			Resource:    presentFacts(evaluation.readiness),
			ObservedAt:  evaluation.observedAt,
		}, nil
	}
	if evaluation.terminalFailure != nil {
		return conflictedObservation(presentFacts(evaluation.readiness), evaluation.terminalFailure, evaluation.observedAt), nil
	}
	if evaluation.readiness == domain.ResourceReadinessReady {
		observation := provisioning.ExecutionObservation{
			Correlation: provisioning.RequestCorrelationFound,
			Execution:   &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle},
			Resource:    presentFacts(domain.ResourceReadinessReady),
			ObservedAt:  evaluation.observedAt,
		}
		return p.attachOutputs(binding, request, object, observation)
	}
	return runningObservation(presentFacts(evaluation.readiness), handle, evaluation.observedAt), nil
}

// deletionCompleteObservation reports positively correlated success with
// genuine physical absence: the only delete outcome that proves destruction.
func deletionCompleteObservation(handle provisioning.ExecutionHandle) provisioning.ExecutionObservation {
	return provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle},
		Resource:    absentFacts(),
	}
}

func uidFromHandle(request observationRequestView) string {
	payload, ok := decodeHandle(request.request.Handle)
	if !ok {
		return ""
	}
	return payload.UID
}

// attachOutputs resolves the output dimension for one concluded observation.
// Extraction happens only for positively correlated create/update success,
// only through the persisted allowlisted mapping, and only from the single
// registered status path. Missing or conflicting mapping identities fail
// loudly instead of falling back to whatever is registered today.
func (p *Provisioner) attachOutputs(binding *resolvedBinding, request provisioning.ObservationRequest, object *kube.Object, observation provisioning.ExecutionObservation) (provisioning.ExecutionObservation, error) {
	if request.Capability == domain.CapabilityDelete || request.Capability == "" {
		return observation, nil
	}
	success := observation.Correlation == provisioning.RequestCorrelationFound &&
		observation.Execution != nil && observation.Execution.State == provisioning.ExecutionStateSucceeded
	if !success {
		return observation, nil
	}
	selectedMappingRef := request.OutputMappingRef
	switch {
	case len(binding.outputs) == 0 && selectedMappingRef == "" && request.OutputSourceMappingRef == "":
		return observation, nil
	case len(binding.outputs) == 0:
		return observation, fmt.Errorf("%w: execution references output mapping %q but no such mapping is registered", provisioning.ErrObservationFailure, selectedMappingRef)
	case selectedMappingRef == "":
		return observation, fmt.Errorf("%w: registered output mapping %q has no durable identity on the execution", provisioning.ErrObservationFailure, binding.binding.CurrentOutputMappingRef)
	}
	mapping, ok := binding.outputs[selectedMappingRef]
	if !ok {
		return observation, fmt.Errorf("%w: requested output mapping %q is not registered", provisioning.ErrObservationFailure, selectedMappingRef)
	}
	envelopeRef := mapping.Ref
	if request.OutputSourceMappingRef != "" {
		if mapping.CompatibleSourceMappingRef != request.OutputSourceMappingRef {
			return observation, fmt.Errorf("%w: selected output mapping %q is not compatible with source mapping %q", provisioning.ErrObservationFailure, mapping.Ref, request.OutputSourceMappingRef)
		}
		// Recovery decodes against the exact source envelope identity the
		// old mapping wrote; the repair mapping only reads it.
		envelopeRef = request.OutputSourceMappingRef
	}
	evidence := extractOutputEvidence(object, mapping, envelopeRef, request.ResourceID, request.TargetGeneration)
	switch evidence.state {
	case evidenceAvailable:
		observation.Outputs = &provisioning.OutputEvidence{State: provisioning.OutputsAvailable, Values: evidence.values, OutputMappingRef: mapping.Ref}
	case evidenceInvalid:
		observation.Outputs = &provisioning.OutputEvidence{State: provisioning.OutputsInvalid, OutputMappingRef: mapping.Ref, Reason: "OutputContractViolation"}
	default:
		observation.Outputs = &provisioning.OutputEvidence{State: provisioning.OutputsUnavailable, OutputMappingRef: mapping.Ref, Reason: "OutputsUnavailable"}
	}
	return observation, nil
}

func observationUnavailable(reason string) (provisioning.ExecutionObservation, error) {
	return provisioning.ExecutionObservation{}, provisioning.ObservationError{Failure: provisioning.ExecutionFailure{
		Kind: provisioning.FailureUnavailable, Reason: reason, Message: "provisioning observation is unavailable",
	}}
}
