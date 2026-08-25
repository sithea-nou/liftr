// SPDX-License-Identifier: Apache-2.0

// Package opentofu implements the Liftr provisioner contract with exactly
// OpenTofu 1.12.6 and private, fenced state evidence.
package opentofu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

type Provisioner struct{ config validatedConfig }

var _ provisioning.Provisioner = (*Provisioner)(nil)
var _ provisioning.FencedProvisioner = (*Provisioner)(nil)
var _ provisioning.ExpiredDispatchRedeliverer = (*Provisioner)(nil)
var _ provisioning.OutputMappingSource = (*Provisioner)(nil)
var _ provisioning.OutputRecoveryMappingSelector = (*Provisioner)(nil)

func New(config Config) (*Provisioner, error) {
	validated, err := config.validate(context.Background())
	if err != nil {
		return nil, err
	}
	return &Provisioner{config: validated}, nil
}

func (p *Provisioner) Capabilities() []provisioning.ProvisionerCapability {
	return append([]provisioning.ProvisionerCapability(nil), p.config.capabilities...)
}

func (*Provisioner) CanRedeliverExpiredDispatch() bool { return true }

func (p *Provisioner) OutputMappingRef(resourceType domain.ResourceTypeRef, capability domain.Capability) string {
	if resourceType != p.config.program.ResourceType || capability == domain.CapabilityDelete || !p.supports(capability) {
		return ""
	}
	return p.config.program.CurrentOutputMappingRef
}

func (p *Provisioner) SelectOutputRecoveryMapping(resourceType domain.ResourceTypeRef, capability domain.Capability, source string) (string, bool) {
	if resourceType != p.config.program.ResourceType || capability == domain.CapabilityDelete || !p.supports(capability) {
		return "", false
	}
	for _, mapping := range p.config.mappings {
		if mapping.Ref != source && mapping.CompatibleSourceMappingRef == source {
			return mapping.Ref, true
		}
	}
	return "", false
}

// Submit without ownership cannot safely mutate the private journal. The
// worker advertises ownership through SubmitFenced; direct callers receive a
// conclusive private-contract rejection before any engine command.
func (p *Provisioner) Submit(context.Context, provisioning.ExecutionRequest) (provisioning.Submission, error) {
	return failedSubmission("ExecutionFenceRequired", provisioning.FailureInvalidRequest), nil
}

func (p *Provisioner) Observe(ctx context.Context, request provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return p.observe(ctx, request, provisioning.ExecutionFence{Passive: true})
}

func (p *Provisioner) SubmitFenced(ctx context.Context, request provisioning.ExecutionRequest, ownership provisioning.ExecutionFence) (submission provisioning.Submission, returnErr error) {
	if err := request.Validate(); err != nil {
		return failedSubmission("InvalidExecutionRequest", provisioning.FailureInvalidRequest), nil
	}
	if err := ownership.Validate(); err != nil || ownership.Passive {
		return failedSubmission("ExecutionFenceRequired", provisioning.FailureInvalidRequest), nil
	}
	if err := p.validateRequest(request.ResourceType, request.Capability, request.OutputMappingRef); err != nil {
		return failedSubmission("RegistrationMismatch", provisioning.FailureInvalidRequest), nil
	}
	key := p.attemptKey(request.ResourceID, request.OperationID, request.AttemptNumber)
	fence := LeaseFence{MessageID: ownership.MessageID, Token: ownership.LeaseToken}
	attempt, err := p.prepareAttempt(ctx, key, fence)
	if err != nil {
		return p.evidenceFailure(err)
	}
	if attempt.Phase != AttemptPrepared {
		return ambiguousSubmission(p.handle(key)), provisioning.ErrAmbiguousSubmission
	}
	binding, err := p.ensureBinding(ctx, key, fence)
	if err != nil {
		return p.evidenceFailure(err)
	}
	input := Input{OperationID: request.OperationID, AttemptNumber: request.AttemptNumber, ResourceID: request.ResourceID, ResourceType: request.ResourceType, Capability: request.Capability, Spec: request.Spec, TargetGeneration: request.TargetGeneration, DesiredPresent: desiredPresent(request.Capability)}
	call, err := p.assemble(ctx, input)
	if err != nil {
		if errors.Is(err, errTransient) {
			return provisioning.Submission{}, notAttempted("OpenTofuPreflightUnavailable")
		}
		return failedSubmission("OpenTofuAssemblyRejected", provisioning.FailureInvalidRequest), nil
	}
	defer func() {
		if closeErr := call.workspace.close(); closeErr != nil && returnErr == nil {
			returnErr = fmt.Errorf("OpenTofu workspace disposition failed")
		}
	}()
	if err := p.init(ctx, call); err != nil {
		if !errors.Is(err, errTransient) {
			return failedSubmission("OpenTofuInitializationRejected", provisioning.FailureInvalidRequest), nil
		}
		return provisioning.Submission{}, notAttempted("OpenTofuInitializationUnavailable")
	}
	if _, err := p.plan(ctx, call, call.planFile); err != nil {
		if errors.Is(err, errTransient) {
			return provisioning.Submission{}, notAttempted("OpenTofuPlanningUnavailable")
		}
		return failedSubmission("OpenTofuPlanRejected", provisioning.FailureInvalidRequest), nil
	}
	show, err := p.command(ctx, call, "show", "-json", call.planFile)
	if err != nil || show.ExitCode != 0 || show.Overflow {
		return provisioning.Submission{}, notAttempted("OpenTofuPlanInspectionUnavailable")
	}
	document, err := decodePlan(show.Stdout)
	if err != nil || validatePlan(document, p.config.program, input, false) != nil {
		return failedSubmission("OpenTofuPlanRejected", provisioning.FailureInvalidRequest), nil
	}
	attempt, err = p.config.Evidence.AdvanceAttempt(ctx, key, fence, AttemptPrepared, attempt.Version, AttemptApplyMayStart)
	if err != nil {
		call.workspace.quarantine()
		return ambiguousSubmission(p.handle(key)), provisioning.ErrAmbiguousSubmission
	}
	call.workspace.quarantine()
	if p.config.BeforeApply != nil {
		if err := p.config.BeforeApply(); err != nil {
			return ambiguousSubmission(p.handle(key)), provisioning.ErrAmbiguousSubmission
		}
	}
	apply, applyErr := p.command(ctx, call, "apply", "-json", "-input=false", "-lock-timeout="+p.config.LockTimeout.String(), call.planFile)
	if applyErr != nil || apply.ExitCode != 0 || apply.Overflow || validateMachineUI(apply.Stdout) != nil {
		_, _ = p.config.Evidence.AdvanceAttempt(ctx, key, fence, AttemptApplyMayStart, attempt.Version, AttemptApplyOutcomeUnknown)
		return ambiguousSubmission(p.handle(key)), provisioning.ErrAmbiguousSubmission
	}
	attempt, err = p.config.Evidence.AdvanceAttempt(ctx, key, fence, AttemptApplyMayStart, attempt.Version, AttemptApplyExited)
	if err != nil {
		return ambiguousSubmission(p.handle(key)), provisioning.ErrAmbiguousSubmission
	}
	observation, err := p.verifyConvergence(ctx, call, input, request.OutputMappingRef)
	if err != nil {
		return ambiguousSubmission(p.handle(key)), provisioning.ErrAmbiguousSubmission
	}
	stateRaw, err := p.pullState(ctx, call)
	if err != nil {
		return ambiguousSubmission(p.handle(key)), provisioning.ErrAmbiguousSubmission
	}
	state, err := parseState(stateRaw)
	if err != nil || !validStateEvidence(state) || binding.State != nil && !stateCompatible(*binding.State, state) {
		return ambiguousSubmission(p.handle(key)), provisioning.ErrAmbiguousSubmission
	}
	binding, err = p.config.Evidence.UpdateState(ctx, key, fence, binding.Version, state)
	if err != nil {
		return ambiguousSubmission(p.handle(key)), provisioning.ErrAmbiguousSubmission
	}
	if _, err := p.config.Evidence.AdvanceAttempt(ctx, key, fence, AttemptApplyExited, attempt.Version, AttemptObservedConverged); err != nil {
		return ambiguousSubmission(p.handle(key)), provisioning.ErrAmbiguousSubmission
	}
	_ = binding
	call.workspace.uncertain = false
	observation.Execution.Handle = pointerHandle(p.handle(key))
	return provisioning.Submission{Observation: observation}, nil
}

func (p *Provisioner) ObserveFenced(ctx context.Context, request provisioning.ObservationRequest, ownership provisioning.ExecutionFence) (provisioning.ExecutionObservation, error) {
	if err := ownership.Validate(); err != nil {
		return provisioning.ExecutionObservation{}, provisioning.ObservationError{Failure: safeFailure(provisioning.FailureInvalidRequest, "ExecutionFenceRequired")}
	}
	return p.observe(ctx, request, ownership)
}

func (p *Provisioner) observe(ctx context.Context, request provisioning.ObservationRequest, ownership provisioning.ExecutionFence) (observation provisioning.ExecutionObservation, returnErr error) {
	if err := request.Validate(); err != nil {
		return failedObservation("InvalidObservationRequest"), nil
	}
	if request.ResourceType != p.config.program.ResourceType {
		return failedObservation("RegistrationMismatch"), nil
	}
	if request.Capability != "" && !p.supports(request.Capability) {
		return failedObservation("RegistrationMismatch"), nil
	}
	if request.Capability != domain.CapabilityDelete && request.OperationID != "" {
		mapping, ok := p.config.mappings[request.OutputMappingRef]
		if request.OutputMappingRef != "" && !ok {
			return failedObservation("OutputMappingMismatch"), nil
		}
		if request.OutputSourceMappingRef != "" && (!ok || mapping.CompatibleSourceMappingRef != request.OutputSourceMappingRef) {
			return failedObservation("OutputMappingMismatch"), nil
		}
	}
	var key AttemptKey
	var attempt AttemptEvidence
	if !ownership.Passive && request.OperationID != "" {
		key = p.attemptKey(request.ResourceID, request.OperationID, request.AttemptNumber)
		var err error
		attempt, err = p.config.Evidence.LoadAttempt(ctx, key)
		if err != nil {
			if errors.Is(err, ErrEvidenceNotFound) {
				return failedObservation("AttemptNotPrepared"), nil
			}
			return unknownObservation(provisioning.RequestCorrelationUnknown, nil), provisioning.ObservationError{Failure: safeFailure(provisioning.FailureUnavailable, "EvidenceUnavailable")}
		}
		if attempt.Phase == AttemptPrepared {
			return failedObservation("ApplyNotStarted"), nil
		}
		switch attempt.Phase {
		case AttemptApplyMayStart, AttemptApplyExited, AttemptApplyOutcomeUnknown, AttemptObservedConverged:
		default:
			return unknownObservation(provisioning.RequestCorrelationFound, pointerHandle(p.handle(key))), nil
		}
	}
	binding, err := p.config.Evidence.LoadStateBinding(ctx, request.ResourceID)
	if err != nil {
		if errors.Is(err, ErrEvidenceNotFound) {
			return unknownObservation(provisioning.RequestCorrelationUnknown, nil), nil
		}
		return unknownObservation(provisioning.RequestCorrelationUnknown, nil), provisioning.ObservationError{Failure: safeFailure(provisioning.FailureUnavailable, "EvidenceUnavailable")}
	}
	if binding.Identity != p.bindingIdentity(request.ResourceID) {
		return unknownObservation(provisioning.RequestCorrelationUnknown, nil), nil
	}
	if ownership.Passive || request.OperationID == "" {
		if binding.State == nil {
			return unknownObservation(provisioning.RequestCorrelationUnknown, nil), nil
		}
		return p.passiveStateObservation(ctx, request, binding)
	}
	input := Input{OperationID: request.OperationID, AttemptNumber: request.AttemptNumber, ResourceID: request.ResourceID, ResourceType: request.ResourceType, Capability: request.Capability, Spec: request.Spec, TargetGeneration: request.TargetGeneration, DesiredPresent: desiredPresent(request.Capability)}
	call, err := p.assemble(ctx, input)
	if err != nil {
		return unknownObservation(provisioning.RequestCorrelationFound, pointerHandle(p.handle(key))), nil
	}
	defer func() {
		if closeErr := call.workspace.close(); closeErr != nil && returnErr == nil {
			returnErr = fmt.Errorf("OpenTofu workspace disposition failed")
		}
	}()
	if err := p.init(ctx, call); err != nil {
		call.workspace.quarantine()
		return unknownObservation(provisioning.RequestCorrelationFound, pointerHandle(p.handle(key))), nil
	}
	stateRaw, err := p.pullState(ctx, call)
	if err != nil {
		call.workspace.quarantine()
		return unknownObservation(provisioning.RequestCorrelationFound, pointerHandle(p.handle(key))), nil
	}
	state, err := parseState(stateRaw)
	if err != nil || !validStateEvidence(state) || binding.State != nil && !stateCompatible(*binding.State, state) {
		call.workspace.quarantine()
		return unknownObservation(provisioning.RequestCorrelationFound, pointerHandle(p.handle(key))), nil
	}
	observation, err = p.verifyConvergence(ctx, call, input, request.OutputMappingRef)
	if err != nil {
		call.workspace.quarantine()
		return unknownObservation(provisioning.RequestCorrelationFound, pointerHandle(p.handle(key))), nil
	}
	latestRaw, err := p.pullState(ctx, call)
	if err != nil {
		call.workspace.quarantine()
		return unknownObservation(provisioning.RequestCorrelationFound, pointerHandle(p.handle(key))), nil
	}
	latest, err := parseState(latestRaw)
	if err != nil || !validStateEvidence(latest) || !stateCompatible(state, latest) || binding.State != nil && !stateCompatible(*binding.State, latest) {
		call.workspace.quarantine()
		return unknownObservation(provisioning.RequestCorrelationFound, pointerHandle(p.handle(key))), nil
	}
	state = latest
	fence := LeaseFence{MessageID: ownership.MessageID, Token: ownership.LeaseToken}
	if binding.State == nil || state != *binding.State {
		binding, err = p.config.Evidence.UpdateState(ctx, key, fence, binding.Version, state)
		if err != nil {
			return unknownObservation(provisioning.RequestCorrelationFound, pointerHandle(p.handle(key))), p.observationEvidenceError(err)
		}
	}
	if attempt.Phase == AttemptApplyMayStart {
		attempt, err = p.config.Evidence.AdvanceAttempt(ctx, key, fence, AttemptApplyMayStart, attempt.Version, AttemptApplyOutcomeUnknown)
		if err != nil {
			return unknownObservation(provisioning.RequestCorrelationFound, pointerHandle(p.handle(key))), p.observationEvidenceError(err)
		}
	}
	if attempt.Phase == AttemptApplyExited || attempt.Phase == AttemptApplyOutcomeUnknown {
		if _, err := p.config.Evidence.AdvanceAttempt(ctx, key, fence, attempt.Phase, attempt.Version, AttemptObservedConverged); err != nil {
			return unknownObservation(provisioning.RequestCorrelationFound, pointerHandle(p.handle(key))), p.observationEvidenceError(err)
		}
	}
	_ = binding
	call.workspace.uncertain = false
	observation.Execution.Handle = pointerHandle(p.handle(key))
	return observation, nil
}

func (p *Provisioner) passiveStateObservation(ctx context.Context, request provisioning.ObservationRequest, binding StateBinding) (provisioning.ExecutionObservation, error) {
	input := Input{ResourceID: request.ResourceID, ResourceType: request.ResourceType, Spec: request.Spec, TargetGeneration: request.TargetGeneration, DesiredPresent: true}
	call, err := p.assemble(ctx, input)
	if err != nil {
		return unknownObservation(provisioning.RequestCorrelationUnknown, nil), nil
	}
	defer func() { _ = call.workspace.close() }()
	if p.init(ctx, call) != nil {
		call.workspace.quarantine()
		return unknownObservation(provisioning.RequestCorrelationUnknown, nil), nil
	}
	raw, err := p.pullState(ctx, call)
	if err != nil {
		call.workspace.quarantine()
		return unknownObservation(provisioning.RequestCorrelationUnknown, nil), nil
	}
	state, err := parseState(raw)
	if err != nil || binding.State == nil || !stateCompatible(*binding.State, state) {
		call.workspace.quarantine()
		return unknownObservation(provisioning.RequestCorrelationUnknown, nil), nil
	}
	call.workspace.uncertain = false
	return unknownObservation(provisioning.RequestCorrelationUnknown, nil), nil
}

func (p *Provisioner) verifyConvergence(ctx context.Context, call *assembledCall, input Input, mappingRef string) (provisioning.ExecutionObservation, error) {
	verifyPlan := filepath.Join(call.workspace.path, "verify.tfplan")
	planned, err := p.plan(ctx, call, verifyPlan)
	if err != nil || planned.ExitCode != 0 {
		return provisioning.ExecutionObservation{}, fmt.Errorf("OpenTofu convergence is unavailable")
	}
	show, err := p.command(ctx, call, "show", "-json", verifyPlan)
	if err != nil || show.ExitCode != 0 || show.Overflow {
		return provisioning.ExecutionObservation{}, fmt.Errorf("OpenTofu convergence is unavailable")
	}
	document, err := decodePlan(show.Stdout)
	if err != nil || validatePlan(document, p.config.program, input, true) != nil {
		return provisioning.ExecutionObservation{}, fmt.Errorf("OpenTofu convergence is invalid")
	}
	facts := unknownFacts()
	if input.Capability == domain.CapabilityDelete {
		facts.Presence = provisioning.ResourcePresenceNotFound
	}
	result := provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound, Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded}, Resource: facts}
	if input.Capability == domain.CapabilityDelete || mappingRef == "" {
		return result, nil
	}
	mapping, ok := p.config.mappings[mappingRef]
	if !ok {
		return provisioning.ExecutionObservation{}, fmt.Errorf("OpenTofu output mapping is unavailable")
	}
	outputs, err := p.command(ctx, call, "output", "-json")
	if err != nil || outputs.ExitCode != 0 || outputs.Overflow {
		result.Outputs = &provisioning.OutputEvidence{State: provisioning.OutputsUnavailable, OutputMappingRef: mappingRef}
		return result, nil
	}
	values, err := decodeOutputs(outputs.Stdout, mapping, input.ResourceID, input.TargetGeneration)
	if err != nil {
		result.Outputs = &provisioning.OutputEvidence{State: provisioning.OutputsInvalid, OutputMappingRef: mappingRef, Reason: "PrivateOutputContractViolation"}
		return result, nil
	}
	result.Outputs = &provisioning.OutputEvidence{State: provisioning.OutputsAvailable, OutputMappingRef: mappingRef, Values: values}
	return result, nil
}

func (p *Provisioner) pullState(ctx context.Context, call *assembledCall) ([]byte, error) {
	result, err := p.config.Runner.Run(ctx, Command{Path: p.config.Executable, Args: []string{"state", "pull"}, Env: call.env, Dir: call.dir, MaxOutputBytes: p.config.MaxStateBytes})
	if err != nil || result.ExitCode != 0 || result.Overflow || len(result.Stdout) == 0 {
		return nil, fmt.Errorf("OpenTofu control state is unavailable")
	}
	return result.Stdout, nil
}

func (p *Provisioner) prepareAttempt(ctx context.Context, key AttemptKey, fence LeaseFence) (AttemptEvidence, error) {
	attempt, err := p.config.Evidence.PrepareAttempt(ctx, key, fence)
	if errors.Is(err, ErrEvidenceConflict) {
		attempt, err = p.config.Evidence.LoadAttempt(ctx, key)
	}
	return attempt, err
}

func (p *Provisioner) ensureBinding(ctx context.Context, key AttemptKey, fence LeaseFence) (StateBinding, error) {
	identity := p.bindingIdentity(key.ResourceID)
	binding, err := p.config.Evidence.CreateStateBinding(ctx, key, fence, identity)
	if errors.Is(err, ErrEvidenceConflict) {
		binding, err = p.config.Evidence.LoadStateBinding(ctx, key.ResourceID)
		if err == nil && binding.Identity != identity {
			return StateBinding{}, ErrEvidenceConflict
		}
	}
	return binding, err
}

func (p *Provisioner) bindingIdentity(resourceID domain.ResourceID) StateBindingIdentity {
	return StateBindingIdentity{ResourceID: resourceID, ProvisionerRef: p.config.Registration.ProvisionerRef, Engine: "opentofu@" + EngineVersion, Program: p.config.program.Ref, Backend: p.config.Registration.Backend.Ref, StateKey: p.stateKey(resourceID)}
}

func (p *Provisioner) stateKey(resourceID domain.ResourceID) string {
	return StateKey(p.config.Registration.Identity, p.config.Registration.ProvisionerRef, p.config.program.ResourceType, resourceID)
}
func (p *Provisioner) attemptKey(resourceID domain.ResourceID, operationID domain.OperationID, attempt uint64) AttemptKey {
	return AttemptKey{ResourceID: resourceID, OperationID: operationID, AttemptNumber: attempt, ProvisionerRef: p.config.Registration.ProvisionerRef}
}
func (p *Provisioner) supports(capability domain.Capability) bool {
	for _, value := range p.config.program.Capabilities {
		if value == capability {
			return true
		}
	}
	return false
}

func (p *Provisioner) validateRequest(resourceType domain.ResourceTypeRef, capability domain.Capability, mappingRef string) error {
	if resourceType != p.config.program.ResourceType || !p.supports(capability) {
		return fmt.Errorf("registration mismatch")
	}
	if capability == domain.CapabilityDelete {
		if mappingRef != "" {
			return fmt.Errorf("delete cannot select outputs")
		}
		return nil
	}
	if mappingRef != p.config.program.CurrentOutputMappingRef {
		return fmt.Errorf("output mapping mismatch")
	}
	return nil
}

func (p *Provisioner) handle(key AttemptKey) provisioning.ExecutionHandle {
	hash := sha256.Sum256([]byte(string(key.ResourceID) + "\x00" + string(key.OperationID) + "\x00" + fmt.Sprint(key.AttemptNumber) + "\x00" + key.ProvisionerRef))
	handle, _ := provisioning.NewExecutionHandle("otf1:" + hex.EncodeToString(hash[:]))
	return handle
}

func stateCompatible(previous, current StateEvidence) bool {
	if !validStateEvidence(previous) || !validStateEvidence(current) || previous.Lineage != current.Lineage || current.Serial < previous.Serial {
		return false
	}
	if current.Serial == previous.Serial && current.Digest != previous.Digest {
		return false
	}
	return true
}

func validStateEvidence(state StateEvidence) bool {
	return state.Lineage != "" && state.Digest != (StateDigest{})
}

var errTransient = errors.New("transient OpenTofu pre-apply failure")

type transientFailure struct{ reason string }

func (e transientFailure) Error() string { return errTransient.Error() }
func (e transientFailure) Unwrap() error { return errTransient }
func transientError(reason string) error { return transientFailure{reason: reason} }

func notAttempted(reason string) error {
	return provisioning.SubmissionNotAttemptedError{Failure: safeFailure(provisioning.FailureUnavailable, reason)}
}
func safeFailure(kind provisioning.ExecutionFailureKind, reason string) provisioning.ExecutionFailure {
	return provisioning.ExecutionFailure{Kind: kind, Reason: reason, Message: "OpenTofu could not establish a safe execution result"}
}
func unknownFacts() provisioning.ResourceObservation {
	return provisioning.ResourceObservation{Presence: provisioning.ResourcePresenceUnknown, Readiness: provisioning.ResourceReadinessUnknown, Drift: provisioning.ResourceDriftUnknown}
}
func failedSubmission(reason string, kind provisioning.ExecutionFailureKind) provisioning.Submission {
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationNotFound, Execution: &provisioning.Execution{State: provisioning.ExecutionStateFailed, Failure: pointerFailure(safeFailure(kind, reason))}, Resource: unknownFacts()}}
}
func failedObservation(reason string) provisioning.ExecutionObservation {
	return failedSubmission(reason, provisioning.FailureInvalidRequest).Observation
}
func ambiguousSubmission(handle provisioning.ExecutionHandle) provisioning.Submission {
	return provisioning.Submission{Observation: unknownObservation(provisioning.RequestCorrelationFound, &handle)}
}
func unknownObservation(correlation provisioning.RequestCorrelation, handle *provisioning.ExecutionHandle) provisioning.ExecutionObservation {
	return provisioning.ExecutionObservation{Correlation: correlation, Execution: &provisioning.Execution{State: provisioning.ExecutionStateUnknown, Handle: handle, Failure: pointerFailure(safeFailure(provisioning.FailureUnknown, "ControlStateUnavailable"))}, Resource: unknownFacts()}
}
func pointerFailure(value provisioning.ExecutionFailure) *provisioning.ExecutionFailure {
	return &value
}
func pointerHandle(value provisioning.ExecutionHandle) *provisioning.ExecutionHandle { return &value }

func (p *Provisioner) evidenceFailure(err error) (provisioning.Submission, error) {
	if errors.Is(err, ErrFenceRejected) {
		return provisioning.Submission{}, provisioning.ErrAmbiguousSubmission
	}
	if errors.Is(err, ErrEvidenceConflict) {
		return provisioning.Submission{Observation: unknownObservation(provisioning.RequestCorrelationUnknown, nil)}, provisioning.ErrAmbiguousSubmission
	}
	return provisioning.Submission{}, notAttempted("EvidenceUnavailable")
}

func (p *Provisioner) observationEvidenceError(err error) error {
	reason := "EvidenceUnavailable"
	if errors.Is(err, ErrFenceRejected) {
		reason = "ExecutionFenceRejected"
	}
	return provisioning.ObservationError{Failure: safeFailure(provisioning.FailureUnavailable, reason)}
}
