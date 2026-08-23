// SPDX-License-Identifier: Apache-2.0

package pulumi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

type Provisioner struct {
	config   Config
	programs map[domain.ResourceTypeRef]Program
	factory  automationFactory
}

var _ provisioning.Provisioner = (*Provisioner)(nil)

func New(config Config) (*Provisioner, error) {
	if isTruthy(os.Getenv("PULUMI_AUTOMATION_API_SKIP_VERSION_CHECK")) {
		return nil, fmt.Errorf("Pulumi version checking cannot be disabled")
	}
	programs, err := config.validate()
	if err != nil {
		return nil, err
	}
	factory, err := newLocalFactory(config.PulumiRoot, config.GoExecutable)
	if err != nil {
		return nil, err
	}
	if err := cleanupStaleWorkspaces(config.WorkspaceRoot, config.StaleWorkspaceAge); err != nil {
		return nil, err
	}
	return &Provisioner{config: config, programs: programs, factory: factory}, nil
}

func newProvisioner(config Config, factory automationFactory) (*Provisioner, error) {
	programs, err := config.validate()
	if err != nil {
		return nil, err
	}
	if factory == nil {
		return nil, fmt.Errorf("Automation API factory is required")
	}
	return &Provisioner{config: config, programs: programs, factory: factory}, nil
}

func (p *Provisioner) Capabilities() []provisioning.ProvisionerCapability {
	result := make([]provisioning.ProvisionerCapability, 0, len(p.programs))
	for resourceType, program := range p.programs {
		for _, capability := range program.Capabilities {
			result = append(result, provisioning.ProvisionerCapability{ResourceType: resourceType, Capability: capability})
		}
	}
	sortCapabilities(result)
	return result
}

// OutputMappingRef implements the worker's OutputMappingSource: the private
// mapping identity declared for create/update executions of a type. Delete
// executions never carry outputs.
func (p *Provisioner) OutputMappingRef(resourceType domain.ResourceTypeRef, capability domain.Capability) string {
	program, ok := p.programs[resourceType]
	if !ok || program.Outputs == nil || capability == domain.CapabilityDelete {
		return ""
	}
	return program.Outputs.Ref
}

// attachOutputs resolves the output dimension for one concluded observation.
// Extraction happens only for positively correlated create/update success,
// only through the persisted allowlisted mapping, and only via selected
// retrieval. A missing or conflicting mapping identity fails loudly instead
// of falling back to whatever is registered today.
func (p *Provisioner) attachOutputs(ctx context.Context, stack automationStack, observation provisioning.ExecutionObservation, request ObservationRequest) (provisioning.ExecutionObservation, error) {
	if request.Capability == domain.CapabilityDelete {
		return observation, nil
	}
	success := observation.Correlation == provisioning.RequestCorrelationFound &&
		observation.Execution != nil && observation.Execution.State == provisioning.ExecutionStateSucceeded
	if !success {
		return observation, nil
	}
	mappingRef := request.OutputMappingRef
	var mapping *OutputMapping
	if program, ok := p.programs[request.ResourceType]; ok && program.Outputs != nil {
		mapping = program.Outputs
	}
	switch {
	case mapping == nil && mappingRef == "":
		return observation, nil
	case mapping == nil:
		return observation, fmt.Errorf("%w: execution references output mapping %q but no such mapping is registered", provisioning.ErrObservationFailure, mappingRef)
	case mappingRef == "":
		return observation, fmt.Errorf("%w: registered output mapping %q has no durable identity on the execution", provisioning.ErrObservationFailure, mapping.Ref)
	case mapping.Ref != mappingRef:
		return observation, fmt.Errorf("%w: registered output mapping %q does not match the persisted identity %q", provisioning.ErrObservationFailure, mapping.Ref, mappingRef)
	}
	raw, err := stack.SelectedOutput(ctx, mapping.ExportName)
	if err != nil {
		observation.Outputs = &provisioning.OutputEvidence{State: provisioning.OutputsUnavailable, Reason: "OutputsUnavailable"}
		return observation, nil
	}
	values, decodeErr := decodeSelectedOutputEnvelope(raw, mapping.Ref, request.ResourceID, request.TargetGeneration)
	if decodeErr != nil {
		reason := "OutputContractViolation"
		if errors.Is(decodeErr, errOutputUnavailable) {
			reason = "OutputsUnavailable"
		}
		observation.Outputs = &provisioning.OutputEvidence{State: provisioning.OutputsInvalid, Reason: reason}
		return observation, nil
	}
	observation.Outputs = &provisioning.OutputEvidence{State: provisioning.OutputsAvailable, Values: values}
	return observation, nil
}

func (p *Provisioner) Submit(ctx context.Context, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	if err := request.Validate(); err != nil {
		return failedSubmission(provisioning.FailureInvalidRequest, "InvalidExecutionRequest"), nil
	}
	program, ok := p.programs[request.ResourceType]
	if !ok || !supportsCapability(program.Capabilities, request.Capability) {
		return failedSubmission(provisioning.FailureUnsupported, "CapabilityUnsupported"), nil
	}
	// Environment resolution happens before any Pulumi invocation. A missing
	// required variable is a platform-side misconfiguration detected by
	// Liftr itself, so it is a conclusive preflight rejection; everything
	// after a launched execution remains ambiguous regardless of evidence.
	supplied, err := resolveProgramEnvironment(ctx, p.config.Environment, program.RequiredEnvironment)
	if err != nil {
		var missing missingRequiredEnvironmentError
		if errors.As(err, &missing) {
			return failedSubmission(provisioning.FailureUnavailable, "RequiredEnvironmentMissing"), nil
		}
		return failedSubmission(provisioning.FailureUnavailable, "ExecutionEnvironmentUnavailable"), nil
	}
	encoded, err := program.EncodeInput(Input{OperationID: request.OperationID, AttemptNumber: request.AttemptNumber, ResourceID: request.ResourceID,
		ResourceType: request.ResourceType, Capability: request.Capability, Spec: request.Spec, TargetGeneration: request.TargetGeneration})
	if err != nil {
		return failedSubmission(provisioning.FailureInvalidRequest, "ProgramInputInvalid"), nil
	}
	workspace, err := createWorkspace(p.config.WorkspaceRoot, program, encoded)
	if err != nil {
		return failedSubmission(provisioning.FailureUnavailable, "WorkspaceUnavailable"), nil
	}
	defer workspace.cleanup()

	environment, err := p.environment(ctx, workspace, supplied)
	if err != nil {
		return failedSubmission(provisioning.FailureUnavailable, "ExecutionEnvironmentUnavailable"), nil
	}
	automationWorkspace, err := p.factory.Open(ctx, workspace.workDir, workspace.homeDir, environment)
	if err != nil {
		return failedSubmission(provisioning.FailureUnavailable, "WorkspaceInitializationFailed"), nil
	}
	stackName := p.stackName(program.ProjectName, request.ResourceID)
	stack, err := automationWorkspace.SelectStack(ctx, stackName)
	if err != nil && request.Capability == domain.CapabilityCreate && errors.Is(err, errStackNotFound) {
		stack, err = automationWorkspace.CreateStack(ctx, stackName)
		if err != nil && errors.Is(err, errStackExists) {
			stack, err = automationWorkspace.SelectStack(ctx, stackName)
		}
	}
	if err != nil {
		kind := provisioning.FailureUnavailable
		reason := "StackSelectionFailed"
		if errors.Is(err, errStackNotFound) {
			kind = provisioning.FailureNotFound
			reason = "ExecutionStateNotFound"
		}
		return failedSubmission(kind, reason), nil
	}

	message := correlationMessage(request.OperationID, request.AttemptNumber)
	handle := p.handle(request.OperationID, request.AttemptNumber, request.ResourceID)
	var summary updateSummary
	if request.Capability == domain.CapabilityDelete {
		summary, err = stack.Destroy(ctx, message)
	} else {
		summary, err = stack.Up(ctx, message)
	}
	if err == nil {
		observation := observationFromSummary(summary, expectedHistoryKind(request.Capability), message, handle)
		if observation.Correlation == provisioning.RequestCorrelationFound && observation.Execution != nil && observation.Execution.State != provisioning.ExecutionStateUnknown {
			observation, attachErr := p.attachOutputs(ctx, stack, observation, observationRequestFromExecution(request))
			if attachErr != nil {
				return provisioning.Submission{}, attachErr
			}
			return provisioning.Submission{Observation: observation}, nil
		}
	}
	recoveryCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	observation, observeErr := p.observeStack(recoveryCtx, stack, request.Capability, message, handle)
	if observeErr == nil && observation.Correlation == provisioning.RequestCorrelationFound && observation.Execution != nil {
		observation, attachErr := p.attachOutputs(ctx, stack, observation, observationRequestFromExecution(request))
		if attachErr != nil {
			return provisioning.Submission{}, attachErr
		}
		return provisioning.Submission{Observation: observation}, nil
	}
	unknown := unknownObservation(handle)
	return provisioning.Submission{Observation: unknown}, provisioning.ErrAmbiguousSubmission
}

func (p *Provisioner) Observe(ctx context.Context, request provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	if err := request.Validate(); err != nil {
		return provisioning.ExecutionObservation{}, provisioning.ObservationError{Failure: provisioning.ExecutionFailure{Kind: provisioning.FailureInvalidRequest, Reason: "InvalidObservationRequest", Message: "observation request is invalid"}}
	}
	if request.OperationID == "" {
		return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationUnknown, Resource: unknownFacts()}, nil
	}
	program, ok := p.programs[request.ResourceType]
	if !ok || !supportsCapability(program.Capabilities, request.Capability) {
		return provisioning.ExecutionObservation{}, provisioning.ObservationError{Failure: provisioning.ExecutionFailure{Kind: provisioning.FailureUnsupported, Reason: "CapabilityUnsupported", Message: "provisioning capability is unsupported"}}
	}
	supplied, err := resolveProgramEnvironment(ctx, p.config.Environment, program.RequiredEnvironment)
	if err != nil {
		var missing missingRequiredEnvironmentError
		if errors.As(err, &missing) {
			return observationUnavailable("RequiredEnvironmentMissing")
		}
		return observationUnavailable("ExecutionEnvironmentUnavailable")
	}
	encoded, err := program.EncodeInput(Input{OperationID: request.OperationID, AttemptNumber: request.AttemptNumber, ResourceID: request.ResourceID,
		ResourceType: request.ResourceType, Capability: request.Capability, Spec: request.Spec, TargetGeneration: request.TargetGeneration})
	if err != nil {
		return provisioning.ExecutionObservation{}, provisioning.ObservationError{Failure: provisioning.ExecutionFailure{Kind: provisioning.FailureInvalidRequest, Reason: "ProgramInputInvalid", Message: "program input is invalid"}}
	}
	workspace, err := createWorkspace(p.config.WorkspaceRoot, program, encoded)
	if err != nil {
		return observationUnavailable("WorkspaceUnavailable")
	}
	defer workspace.cleanup()
	environment, err := p.environment(ctx, workspace, supplied)
	if err != nil {
		return observationUnavailable("ExecutionEnvironmentUnavailable")
	}
	automationWorkspace, err := p.factory.Open(ctx, workspace.workDir, workspace.homeDir, environment)
	if err != nil {
		return observationUnavailable("WorkspaceInitializationFailed")
	}
	stack, err := automationWorkspace.SelectStack(ctx, p.stackName(program.ProjectName, request.ResourceID))
	if err != nil {
		if errors.Is(err, errStackNotFound) {
			return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationUnknown, Resource: unknownFacts()}, nil
		}
		return observationUnavailable("StackSelectionFailed")
	}
	observation, observeErr := p.observeStack(ctx, stack, request.Capability, correlationMessage(request.OperationID, request.AttemptNumber), p.handle(request.OperationID, request.AttemptNumber, request.ResourceID))
	if observeErr != nil {
		return provisioning.ExecutionObservation{}, observeErr
	}
	return p.attachOutputs(ctx, stack, observation, request)
}

func (p *Provisioner) observeStack(ctx context.Context, stack automationStack, capability domain.Capability, message string, handle provisioning.ExecutionHandle) (provisioning.ExecutionObservation, error) {
	var matches []updateSummary
	for page := 1; page <= p.config.HistoryMaximumPages; page++ {
		history, err := stack.History(ctx, p.config.HistoryPageSize, page)
		if err != nil {
			return observationUnavailable("HistoryUnavailable")
		}
		for _, summary := range history {
			if summary.message == message {
				matches = append(matches, summary)
			}
		}
		if len(history) < p.config.HistoryPageSize {
			break
		}
		if page == p.config.HistoryMaximumPages {
			return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationUnknown, Resource: unknownFacts()}, nil
		}
	}
	if len(matches) != 1 {
		return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationUnknown, Resource: unknownFacts()}, nil
	}
	return observationFromSummary(matches[0], expectedHistoryKind(capability), message, handle), nil
}

func (p *Provisioner) environment(ctx context.Context, workspace isolatedWorkspace, supplied map[string]string) (map[string]string, error) {
	environment := map[string]string{
		"PULUMI_BACKEND_URL":                          p.config.BackendURL,
		"PULUMI_DISABLE_AUTOMATIC_PLUGIN_ACQUISITION": "true",
		"PULUMI_DISABLE_REGISTRY_RESOLVE":             "true",
		"PULUMI_IGNORE_AMBIENT_PLUGINS":               "true",
		"PULUMI_SKIP_UPDATE_CHECK":                    "true",
		inputEnvironment:                              workspace.inputPath(),
	}
	for key, value := range supplied {
		environment[key] = value
	}
	environment["PULUMI_AUTOMATION_API_SKIP_VERSION_CHECK"] = "false"
	environment["PULUMI_BACKEND_URL"] = p.config.BackendURL
	environment["PULUMI_DISABLE_AUTOMATIC_PLUGIN_ACQUISITION"] = "true"
	environment["PULUMI_DISABLE_REGISTRY_RESOLVE"] = "true"
	environment["PULUMI_DIY_BACKEND_IGNORE_DEPRECATION_ERROR"] = "true"
	environment["PULUMI_DIY_BACKEND_NO_LEGACY_WARNING"] = "true"
	environment["PULUMI_IGNORE_AMBIENT_PLUGINS"] = "true"
	environment["PULUMI_SKIP_UPDATE_CHECK"] = "true"
	environment[inputEnvironment] = workspace.inputPath()
	return environment, nil
}

func isTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	default:
		return false
	}
}

func (p *Provisioner) stackName(project string, resourceID domain.ResourceID) string {
	switch p.config.StackNamingVersion {
	case StackNamingVersionV1:
		return stackNameV1(p.config.Identity, p.config.StackNamespace, project, resourceID)
	default:
		panic("unreachable: stack naming version is validated before execution")
	}
}

func stackNameV1(identity, stackNamespace, project string, resourceID domain.ResourceID) string {
	namespace := sha256.Sum256([]byte(identity + "\x00" + stackNamespace))
	resource := sha256.Sum256([]byte(resourceID))
	name := fmt.Sprintf("liftr-%x-%x", namespace[:6], resource[:20])
	return auto.FullyQualifiedStackName("organization", project, name)
}

func supportsCapability(capabilities []domain.Capability, capability domain.Capability) bool {
	for _, supported := range capabilities {
		if supported == capability {
			return true
		}
	}
	return false
}

func (p *Provisioner) handle(operationID domain.OperationID, attempt uint64, resourceID domain.ResourceID) provisioning.ExecutionHandle {
	digest := sha256.Sum256([]byte(fmt.Sprintf("lp1\x00%s\x00%s\x00%d\x00%s", p.config.Identity, operationID, attempt, resourceID)))
	handle, _ := provisioning.NewExecutionHandle("lp1." + base64.RawURLEncoding.EncodeToString(digest[:]))
	return handle
}

func correlationMessage(operationID domain.OperationID, attempt uint64) string {
	return fmt.Sprintf("liftr/v1/%s/%d", base64.RawURLEncoding.EncodeToString([]byte(operationID)), attempt)
}

func expectedHistoryKind(capability domain.Capability) string {
	if capability == domain.CapabilityDelete {
		return "destroy"
	}
	return "update"
}

func observationFromSummary(summary updateSummary, expectedKind, message string, handle provisioning.ExecutionHandle) provisioning.ExecutionObservation {
	observation := provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound, Resource: unknownFacts(), ObservedAt: summaryTime(summary.endTime)}
	if summary.message != message || summary.kind != expectedKind {
		observation.Correlation = provisioning.RequestCorrelationUnknown
		return observation
	}
	execution := &provisioning.Execution{Handle: &handle}
	switch summary.result {
	case "succeeded":
		execution.State = provisioning.ExecutionStateSucceeded
		if expectedKind == "destroy" {
			// A conclusively correlated successful destroy proves that the
			// resources the stack managed were removed. That justifies one
			// honest normalized fact — absence of the Liftr-managed target —
			// while readiness and drift stay Unknown. Create and update
			// success never fabricates presence, readiness, or sync facts:
			// execution evidence is not independent observation.
			observation.Resource = provisioning.ResourceObservation{
				Presence:  provisioning.ResourcePresenceNotFound,
				Readiness: provisioning.ResourceReadinessUnknown,
				Drift:     provisioning.ResourceDriftUnknown,
			}
		}
	case "failed":
		execution.State = provisioning.ExecutionStateFailed
		execution.Failure = &provisioning.ExecutionFailure{Kind: provisioning.FailureExecution, Reason: "ProgramExecutionFailed", Message: "provisioning program execution failed"}
	case "in-progress":
		execution.State = provisioning.ExecutionStateRunning
	case "":
		if summary.endTime == nil {
			execution.State = provisioning.ExecutionStateRunning
		} else {
			execution.State = provisioning.ExecutionStateUnknown
			execution.Failure = &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "ExecutionResultUnknown", Message: "provisioning execution result is unknown"}
		}
	default:
		execution.State = provisioning.ExecutionStateUnknown
		execution.Failure = &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "ExecutionResultUnknown", Message: "provisioning execution result is unknown"}
	}
	observation.Execution = execution
	return observation
}

func observationRequestFromExecution(request provisioning.ExecutionRequest) ObservationRequest {
	return ObservationRequest{
		OperationID: request.OperationID, AttemptNumber: request.AttemptNumber, ResourceID: request.ResourceID,
		ResourceType: request.ResourceType, Capability: request.Capability, TargetGeneration: request.TargetGeneration,
		OutputMappingRef: request.OutputMappingRef,
	}
}

// OutputMappingRef is the type alias-free reference to the observation request
// shape used inside this adapter.
type ObservationRequest = provisioning.ObservationRequest

func failedSubmission(kind provisioning.ExecutionFailureKind, reason string) provisioning.Submission {
	failure := &provisioning.ExecutionFailure{Kind: kind, Reason: reason, Message: "provisioning request was rejected before execution"}
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationNotFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateFailed, Failure: failure}, Resource: unknownFacts()}}
}

func unknownObservation(handle provisioning.ExecutionHandle) provisioning.ExecutionObservation {
	failure := &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "SubmissionOutcomeUnknown", Message: "submission outcome could not be determined"}
	return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationUnknown,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateUnknown, Handle: &handle, Failure: failure}, Resource: unknownFacts()}
}

func observationUnavailable(reason string) (provisioning.ExecutionObservation, error) {
	return provisioning.ExecutionObservation{}, provisioning.ObservationError{Failure: provisioning.ExecutionFailure{Kind: provisioning.FailureUnavailable, Reason: reason, Message: "provisioning observation is unavailable"}}
}
