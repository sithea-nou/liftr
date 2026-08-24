// SPDX-License-Identifier: Apache-2.0

package crossplane

import (
	"context"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube"
)

// maxConditionalRetries bounds re-verification loops after precondition
// conflicts. Every retry starts from a fresh verified read; there is never a
// blind rewrite or re-delete by name.
const maxConditionalRetries = 3

// mutationOutcome classifies one identity-safe desired-state mutation.
type mutationOutcome int

const (
	mutationSucceeded mutationOutcome = iota
	mutationIdentityConflict
	mutationRejected
	mutationUncertain
)

func (p *Provisioner) Submit(ctx context.Context, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	if err := request.Validate(); err != nil {
		return failedSubmission(provisioning.FailureInvalidRequest, "InvalidExecutionRequest"), nil
	}
	binding, failure := p.resolveBinding(executionRequestView{request})
	if failure != nil {
		return failedSubmission(failure.kind, failure.reason), nil
	}
	switch request.Capability {
	case domain.CapabilityDelete:
		return p.submitDelete(ctx, binding, request)
	default:
		return p.submitCreateOrUpdate(ctx, binding, request)
	}
}

// submitCreateOrUpdate persists desired state for create and update. The
// write is Accepted only when the API server confirms persistence;
// reconciliation is never assumed from a successful write.
func (p *Provisioner) submitCreateOrUpdate(ctx context.Context, binding *resolvedBinding, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	manifest, pair := p.buildExecutionManifest(binding, request)
	if pair != nil {
		return failedSubmission(pair.kind, pair.reason), nil
	}
	name := p.targetName(binding, request.ResourceID)
	handle := encodeHandle(binding, name, "")
	existing, err := p.client.Get(ctx, binding.gvr, binding.namespace, name)
	if err != nil {
		if kube.IsNotFound(err) {
			if request.Capability == domain.CapabilityUpdate {
				// Update cannot proceed without a target, but a 404 alone is
				// ambiguous. Only a live-served GVR makes the absence
				// conclusive; an unserved kind is a control-plane failure
				// with its own curated classification, and uncertain
				// discovery stays unavailable.
				switch p.resolveAbsence(ctx, binding) {
				case kindNotServed:
					return failedSubmission(provisioning.FailureUnsupported, reasonTargetKindUnregistered), nil
				case absenceUncertain:
					return failedSubmission(provisioning.FailureUnavailable, reasonControlPlaneUnavailable), nil
				}
				return failedSubmission(provisioning.FailureNotFound, reasonManagedTargetAbsent), nil
			}
			return p.createMissingObject(ctx, binding, request, manifest, handle)
		}
		// The preflight read failed before any write was attempted, so no
		// infrastructure effect is possible either way.
		pair := classifyAPIError(err)
		if !kube.IsAPIError(err) || kube.IsUnavailable(err) {
			pair = failurePair{provisioning.FailureUnavailable, reasonControlPlaneUnavailable}
		}
		return failedSubmission(pair.kind, pair.reason), nil
	}
	owner := p.identityFor(request.ResourceType, request.ResourceID)
	if !owner.verify(existing) {
		// A physical object exists under Liftr's deterministic name but does
		// not belong to this Resource. Presence stays Present and the
		// submission fails closed; this is never NotFound and never adopted.
		return conflictedSubmission(presentFacts(domain.ResourceReadinessUnknown), identityConflictFailure(reasonTargetIdentityConflict)), nil
	}
	outcome, applyErr := p.applyDesiredState(ctx, binding, name, manifest, existing.UID(), existing.ResourceVersion())
	return p.mutationSubmission(handle, outcome, applyErr)
}

func (p *Provisioner) createMissingObject(ctx context.Context, binding *resolvedBinding, request provisioning.ExecutionRequest, manifest *kube.Object, handle provisioning.ExecutionHandle) (provisioning.Submission, error) {
	_, err := p.client.Create(ctx, binding.gvr, binding.namespace, manifest)
	switch {
	case err == nil:
		return acceptedSubmission(handle, presentFacts(domain.ResourceReadinessUnknown)), nil
	case kube.IsAlreadyExists(err):
		// Another writer won the race for the name. Re-read and re-evaluate:
		// only an object Liftr owns may be reasserted onto.
		raced, getErr := p.client.Get(ctx, binding.gvr, binding.namespace, p.targetName(binding, request.ResourceID))
		if getErr == nil {
			owner := p.identityFor(request.ResourceType, request.ResourceID)
			if owner.verify(raced) {
				outcome, applyErr := p.applyDesiredState(ctx, binding, raced.Name(), manifest, raced.UID(), raced.ResourceVersion())
				return p.mutationSubmission(handle, outcome, applyErr)
			}
			return conflictedSubmission(presentFacts(domain.ResourceReadinessUnknown), identityConflictFailure(reasonTargetIdentityConflict)), nil
		}
		return unknownSubmission(handle, err)
	case isTransportUncertain(err):
		return unknownSubmission(handle, err)
	default:
		pair := classifyAPIError(err)
		return failedSubmission(pair.kind, pair.reason), nil
	}
}

// mutationSubmission converts one identity-safe mutation outcome into its
// provider-neutral submission shape.
func (p *Provisioner) mutationSubmission(handle provisioning.ExecutionHandle, outcome mutationOutcome, cause error) (provisioning.Submission, error) {
	switch outcome {
	case mutationSucceeded:
		return acceptedSubmission(handle, presentFacts(domain.ResourceReadinessUnknown)), nil
	case mutationIdentityConflict:
		return conflictedSubmission(presentFacts(domain.ResourceReadinessUnknown), identityConflictFailure(reasonTargetIdentityConflict)), nil
	case mutationUncertain:
		return unknownSubmission(handle, cause)
	default:
		pair := failurePair{provisioning.FailureExecution, "DesiredStateUpdateRejected"}
		return failedSubmission(pair.kind, pair.reason), nil
	}
}

// submitDelete verifies ownership and issues a UID-preconditioned delete. A
// precondition conflict forces a fresh read and re-evaluation; Liftr never
// deletes a replacement object merely because it previously verified the
// original. Kubernetes DELETE acceptance is Accepted evidence, not
// completion.
func (p *Provisioner) submitDelete(ctx context.Context, binding *resolvedBinding, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	name := p.targetName(binding, request.ResourceID)
	handle := encodeHandle(binding, name, "")
	existing, err := p.client.Get(ctx, binding.gvr, binding.namespace, name)
	if err != nil {
		if kube.IsNotFound(err) {
			// A 404 here would complete destruction through the conclusive
			// managed-absence path, so it demands positive proof that the
			// API kind is currently served. A vanished CRD is a control-plane
			// failure — never evidence that cloud infrastructure was cleaned
			// up.
			switch p.resolveAbsence(ctx, binding) {
			case absenceProven:
				failure := &provisioning.ExecutionFailure{
					Kind: provisioning.FailureNotFound, Reason: reasonManagedTargetAbsent,
					Message: "the managed target is conclusively absent",
				}
				return provisioning.Submission{Observation: provisioning.ExecutionObservation{
					Correlation: provisioning.RequestCorrelationNotFound,
					Execution:   &provisioning.Execution{State: provisioning.ExecutionStateFailed, Failure: failure},
					Resource:    absentFacts(),
				}}, nil
			case kindNotServed:
				return failedSubmission(provisioning.FailureUnsupported, reasonTargetKindUnregistered), nil
			default:
				return failedSubmission(provisioning.FailureUnavailable, reasonControlPlaneUnavailable), nil
			}
		}
		pair := classifyAPIError(err)
		if !kube.IsAPIError(err) || kube.IsUnavailable(err) {
			pair = failurePair{provisioning.FailureUnavailable, reasonControlPlaneUnavailable}
		}
		return failedSubmission(pair.kind, pair.reason), nil
	}
	owner := p.identityFor(request.ResourceType, request.ResourceID)
	if !owner.verify(existing) {
		return conflictedSubmission(presentFacts(domain.ResourceReadinessUnknown), identityConflictFailure(reasonTargetIdentityConflict)), nil
	}
	deleteErr := p.client.Delete(ctx, binding.gvr, binding.namespace, name, existing.UID())
	switch {
	case deleteErr == nil:
		return acceptedSubmission(handle, presentFacts(domain.ResourceReadinessNotReady)), nil
	case kube.IsConflict(deleteErr):
		// Preconditions rejected the delete: whatever sits under this name
		// now is not the object Liftr just verified. Never repeat by name;
		// fail closed on the replacement regardless of its metadata.
		if _, getErr := p.client.Get(ctx, binding.gvr, binding.namespace, name); getErr == nil {
			return conflictedSubmission(presentFacts(domain.ResourceReadinessUnknown), identityConflictFailure(reasonTargetIdentityConflict)), nil
		} else if kube.IsNotFound(getErr) {
			// The verified object was destroyed concurrently and nothing
			// replaced it; the deletion provably took effect.
			return deletionObservedComplete(handle), nil
		}
		return unknownSubmission(handle, deleteErr)
	case kube.IsNotFound(deleteErr):
		// The object existed moments ago under our verified identity; a 404
		// on the delete now could still mean the API kind vanished in
		// between. Only a live-served GVR lets this count as completed
		// destruction; anything else stays ambiguous and is observed.
		switch p.resolveAbsence(ctx, binding) {
		case absenceProven:
			return deletionObservedComplete(handle), nil
		case kindNotServed:
			return conflictedSubmission(presentFacts(domain.ResourceReadinessUnknown), kindUnavailableFailure()), nil
		default:
			return unknownSubmission(handle, deleteErr)
		}
	case isTransportUncertain(deleteErr):
		return unknownSubmission(handle, deleteErr)
	default:
		pair := classifyAPIError(deleteErr)
		return failedSubmission(pair.kind, pair.reason), nil
	}
}

// deletionObservedComplete reports positively correlated success with
// genuine physical absence after a delete that provably took effect.
func deletionObservedComplete(handle provisioning.ExecutionHandle) provisioning.Submission {
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle},
		Resource:    absentFacts(),
	}}
}

func (p *Provisioner) buildExecutionManifest(binding *resolvedBinding, request provisioning.ExecutionRequest) (*kube.Object, *failurePair) {
	input := Input{
		OperationID:      request.OperationID,
		AttemptNumber:    request.AttemptNumber,
		ResourceID:       request.ResourceID,
		ResourceType:     request.ResourceType,
		Capability:       request.Capability,
		Spec:             request.Spec,
		TargetGeneration: request.TargetGeneration,
	}
	manifest, err := buildManifest(binding, p.identityFor(request.ResourceType, request.ResourceID), p.targetName(binding, request.ResourceID), input)
	if err != nil {
		// Manifest assembly failures are deterministic input problems
		// detected strictly before any write boundary.
		return nil, &failurePair{provisioning.FailureInvalidRequest, "ProgramInputInvalid"}
	}
	return manifest, nil
}

// applyDesiredState performs the identity-safe desired-state mutation. The
// patch carries the resourceVersion Liftr just verified; on conflict it
// re-reads, re-verifies ownership and physical UID, and only then may retry.
// A replacement object under the same name is never adopted or overwritten.
func (p *Provisioner) applyDesiredState(ctx context.Context, binding *resolvedBinding, name string, manifest *kube.Object, verifiedUID, resourceVersion string) (mutationOutcome, error) {
	currentVersion := resourceVersion
	for attempt := 0; attempt < maxConditionalRetries; attempt++ {
		_, err := p.client.Update(ctx, binding.gvr, binding.namespace, name, conditionalUpdate(manifest, currentVersion), currentVersion)
		if err == nil {
			return mutationSucceeded, nil
		}
		switch {
		case kube.IsConflict(err):
			fresh, getErr := p.client.Get(ctx, binding.gvr, binding.namespace, name)
			if getErr != nil {
				if kube.IsNotFound(getErr) {
					// The verified object vanished mid-flight and no write
					// landed; the identity Liftr verified is gone.
					return mutationIdentityConflict, nil
				}
				return mutationUncertain, nil
			}
			owner := p.identityFor(binding.binding.ResourceType, domain.ResourceID(resourceIDOfManifest(manifest)))
			if !owner.verify(fresh) || (verifiedUID != "" && fresh.UID() != verifiedUID) {
				return mutationIdentityConflict, nil
			}
			currentVersion = fresh.ResourceVersion()
			continue
		case isTransportUncertain(err):
			// The apply may or may not have been persisted; the answer is
			// genuinely ambiguous and belongs to Observe.
			return mutationUncertain, nil
		default:
			return mutationRejected, nil
		}
	}
	// Persistent churn: every rejected write provably did not land because
	// each carried a stale precondition, but progress requires a calmer
	// control plane. Fail closed instead of spinning indefinitely.
	return mutationRejected, nil
}

func resourceIDOfManifest(manifest *kube.Object) string {
	value, _ := manifest.AnnotationString(annotationResourceID)
	return value
}

func identityConflictFailure(reason string) *provisioning.ExecutionFailure {
	message := "a foreign object occupies the managed target name"
	if reason == reasonTargetIdentityChanged {
		message = "the managed target's physical identity changed during the execution"
	}
	return &provisioning.ExecutionFailure{Kind: provisioning.FailureExecution, Reason: reason, Message: message}
}

// kindUnavailableFailure reports that the target API kind is no longer
// served while a physical execution dimension was in flight. It fails the
// submission closed without ever claiming managed absence.
func kindUnavailableFailure() *provisioning.ExecutionFailure {
	return &provisioning.ExecutionFailure{
		Kind:    provisioning.FailureUnsupported,
		Reason:  reasonTargetKindUnregistered,
		Message: "the target API kind is no longer served by the control plane",
	}
}

// isTransportUncertain reports whether an error leaves persistence unknown:
// transport failures and server-side faults are ambiguous, while structured
// client-side rejections are conclusive.
func isTransportUncertain(err error) bool {
	if err == nil {
		return false
	}
	if !kube.IsAPIError(err) {
		return true
	}
	return kube.IsUnavailable(err)
}
