// SPDX-License-Identifier: Apache-2.0

// Package provisioning defines the provider-neutral boundary between Liftr and
// infrastructure provisioning backends.
package provisioning

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
)

var (
	ErrAmbiguousSubmission    = errors.New("provisioner submission outcome is ambiguous")
	ErrSubmissionNotAttempted = errors.New("provisioner submission was not attempted")
	ErrObservationFailure     = errors.New("provisioner observation failed")
)

// Provisioner is the minimum contract required by Liftr. Submit sends
// provider-neutral intent; Observe reads the backend's current facts.
type Provisioner interface {
	Capabilities() []ProvisionerCapability
	Submit(context.Context, ExecutionRequest) (Submission, error)
	Observe(context.Context, ObservationRequest) (ExecutionObservation, error)
}

// ExecutionFence is the caller's current ownership of one durable work item.
// It is intentionally opaque to ordinary provisioners. Stateful adapters may
// use it to fence private execution evidence against the same lease that
// authorizes the worker call. Passive calls may cancel work on ownership loss
// but must not mutate execution evidence.
type ExecutionFence struct {
	MessageID  string
	LeaseToken string
	Passive    bool
}

func (f ExecutionFence) Validate() error {
	if strings.TrimSpace(f.MessageID) == "" || strings.TrimSpace(f.LeaseToken) == "" {
		return fmt.Errorf("message ID and lease token are required")
	}
	return nil
}

// FencedProvisioner is an optional private execution seam for stateful
// adapters. Provisioner remains the public minimum contract; the worker falls
// back to its ordinary methods when this capability is absent.
type FencedProvisioner interface {
	SubmitFenced(context.Context, ExecutionRequest, ExecutionFence) (Submission, error)
	ObserveFenced(context.Context, ObservationRequest, ExecutionFence) (ExecutionObservation, error)
}

// ExpiredDispatchRedeliverer is an optional private capability for an adapter
// whose durable submission evidence makes same-attempt redelivery safe. A
// fence alone does not imply this property.
type ExpiredDispatchRedeliverer interface {
	CanRedeliverExpiredDispatch() bool
}

// OutputMappingSource optionally declares the private, immutable output
// mapping selected for an execution. Liftr persists the returned identity
// before calling Submit.
type OutputMappingSource interface {
	OutputMappingRef(domain.ResourceTypeRef, domain.Capability) string
}

// OutputRecoveryMappingSelector optionally selects an explicitly compatible
// output mapping for recovery of a previously successful execution.
type OutputRecoveryMappingSelector interface {
	SelectOutputRecoveryMapping(domain.ResourceTypeRef, domain.Capability, string) (string, bool)
}

// ProvisionerCapability describes a ResourceType action supported by a backend.
type ProvisionerCapability struct {
	ResourceType domain.ResourceTypeRef
	Capability   domain.Capability
}

// ExecutionRequest contains only the immutable intent and correlation data a
// provisioner needs. It does not contain Liftr Operation state or phase.
// OutputMappingRef is the durable, private output-mapping identity persisted
// for this execution at dispatch time; implementations must interpret outputs
// through exactly this mapping and never fall back to whatever happens to be
// registered today.
type ExecutionRequest struct {
	OperationID      domain.OperationID
	AttemptNumber    uint64
	ResourceID       domain.ResourceID
	ResourceType     domain.ResourceTypeRef
	Spec             domain.ResourceSpec
	Capability       domain.Capability
	TargetGeneration uint64
	OutputMappingRef string
}

func (r ExecutionRequest) Validate() error {
	if strings.TrimSpace(string(r.OperationID)) == "" {
		return fmt.Errorf("operation ID is required")
	}
	if r.AttemptNumber == 0 {
		return fmt.Errorf("attempt number must be greater than zero")
	}
	if strings.TrimSpace(string(r.ResourceID)) == "" {
		return fmt.Errorf("resource ID is required")
	}
	if strings.TrimSpace(r.ResourceType.Name) == "" || strings.TrimSpace(r.ResourceType.Version) == "" {
		return fmt.Errorf("resource type reference is required")
	}
	if strings.TrimSpace(string(r.Capability)) == "" {
		return fmt.Errorf("capability is required")
	}
	if r.TargetGeneration == 0 {
		return fmt.Errorf("target generation must be greater than zero")
	}
	return nil
}

// ExecutionHandle is an opaque backend reference used by the provisioner to
// correlate subsequent observations with submitted intent. Liftr must never
// inspect or branch on its contents.
type ExecutionHandle struct {
	token string
}

func NewExecutionHandle(token string) (ExecutionHandle, error) {
	if strings.TrimSpace(token) == "" {
		return ExecutionHandle{}, fmt.Errorf("execution handle is required")
	}
	return ExecutionHandle{token: token}, nil
}

func (h ExecutionHandle) IsZero() bool { return h.token == "" }

// String is for transport and persistence only. The token has no Liftr semantics.
func (h ExecutionHandle) String() string { return h.token }

// Submission contains the backend's immediate result. Execution is normally
// present after Submit, including when the backend accepted asynchronous work.
type Submission struct {
	Observation ExecutionObservation
}

// IsZero reports whether Submit returned no backend facts at all. A
// SubmissionNotAttemptedError may authorize redispatch only with this exact
// absence of evidence.
func (s Submission) IsZero() bool {
	observation := s.Observation
	return observation.Correlation == "" && observation.Execution == nil &&
		observation.Resource == (ResourceObservation{}) && observation.ObservedAt.IsZero() && observation.Outputs == nil
}

// ObservationRequest identifies what the provisioner should observe. Handle
// is optional for declarative backends that observe by resource identity.
// OutputMappingRef is the durable mapping identity selected for this
// execution. Ordinary observations decode envelopes against that mapping's
// own identity. OutputSourceMappingRef is set only for output recovery and
// names the source execution's persisted envelope identity; the selected
// mapping must explicitly declare exact compatibility with it.
type ObservationRequest struct {
	OperationID            domain.OperationID
	AttemptNumber          uint64
	ResourceID             domain.ResourceID
	ResourceType           domain.ResourceTypeRef
	Spec                   domain.ResourceSpec
	Capability             domain.Capability
	TargetGeneration       uint64
	Handle                 *ExecutionHandle
	OutputMappingRef       string
	OutputSourceMappingRef string
}

func (r ObservationRequest) Validate() error {
	if strings.TrimSpace(string(r.ResourceID)) == "" {
		return fmt.Errorf("resource ID is required")
	}
	if strings.TrimSpace(r.ResourceType.Name) == "" || strings.TrimSpace(r.ResourceType.Version) == "" {
		return fmt.Errorf("resource type reference is required")
	}
	if r.TargetGeneration == 0 {
		return fmt.Errorf("target generation must be greater than zero")
	}
	hasOperation := strings.TrimSpace(string(r.OperationID)) != ""
	hasAttempt := r.AttemptNumber != 0
	hasCapability := strings.TrimSpace(string(r.Capability)) != ""
	if hasOperation != hasAttempt || hasOperation != hasCapability {
		return fmt.Errorf("operation ID, attempt number, and capability must be provided together")
	}
	if strings.TrimSpace(r.OutputSourceMappingRef) != "" && (!hasOperation || strings.TrimSpace(r.OutputMappingRef) == "") {
		return fmt.Errorf("output source mapping requires an operation and selected output mapping")
	}
	return nil
}

type ExecutionState string

const (
	ExecutionStateAccepted  ExecutionState = "Accepted"
	ExecutionStateRunning   ExecutionState = "Running"
	ExecutionStateSucceeded ExecutionState = "Succeeded"
	ExecutionStateFailed    ExecutionState = "Failed"
	ExecutionStateUnknown   ExecutionState = "Unknown"
)

// RequestCorrelation reports whether the backend can correlate the submitted
// OperationID or durable handle. It is independent from current execution and
// resource presence.
type RequestCorrelation string

const (
	RequestCorrelationFound    RequestCorrelation = "Found"
	RequestCorrelationNotFound RequestCorrelation = "NotFound"
	RequestCorrelationUnknown  RequestCorrelation = "Unknown"
)

// Execution is present when a current backend execution exists. A nil
// Execution in ExecutionObservation means there is no current execution, not
// that the execution state is unknown.
type Execution struct {
	State   ExecutionState
	Handle  *ExecutionHandle
	Failure *ExecutionFailure
}

type ExecutionFailureKind string

const (
	FailureInvalidRequest ExecutionFailureKind = "InvalidRequest"
	FailureUnsupported    ExecutionFailureKind = "Unsupported"
	FailureUnavailable    ExecutionFailureKind = "Unavailable"
	FailureTimeout        ExecutionFailureKind = "Timeout"
	FailureNotFound       ExecutionFailureKind = "NotFound"
	FailureExecution      ExecutionFailureKind = "ExecutionFailed"
	FailureUnknown        ExecutionFailureKind = "Unknown"
)

// ExecutionFailure is a provider-neutral failure classification. Liftr owns
// the policy that interprets it.
type ExecutionFailure struct {
	Kind    ExecutionFailureKind
	Reason  string
	Message string
}

func (f ExecutionFailure) Error() string {
	if f.Message == "" {
		return fmt.Sprintf("%s: %s", f.Kind, f.Reason)
	}
	return fmt.Sprintf("%s: %s: %s", f.Kind, f.Reason, f.Message)
}

// SubmissionNotAttemptedError explicitly reports that Submit returned before
// making any backend execution attempt. Only transient availability failures
// are legal: this error authorizes Liftr to retry the same durable attempt.
// All other Submit errors remain potentially attempted and therefore
// ambiguous unless accompanied by conclusive submission evidence.
type SubmissionNotAttemptedError struct {
	Failure ExecutionFailure
}

func (e SubmissionNotAttemptedError) Error() string {
	return fmt.Sprintf("%s: %s", ErrSubmissionNotAttempted, e.Failure.Error())
}

func (e SubmissionNotAttemptedError) Unwrap() error { return ErrSubmissionNotAttempted }

// AsSubmissionNotAttempted extracts either the value or pointer form so
// adapters can follow their normal typed-error convention without changing
// worker semantics.
func AsSubmissionNotAttempted(err error) (SubmissionNotAttemptedError, bool) {
	var value SubmissionNotAttemptedError
	if errors.As(err, &value) {
		return value, true
	}
	var pointer *SubmissionNotAttemptedError
	if errors.As(err, &pointer) && pointer != nil {
		return *pointer, true
	}
	return SubmissionNotAttemptedError{}, false
}

// Validate ensures the error carries complete, retryable provider-neutral
// failure evidence.
func (e SubmissionNotAttemptedError) Validate() error {
	if e.Failure.Kind != FailureUnavailable && e.Failure.Kind != FailureTimeout {
		return fmt.Errorf("submission not attempted failure kind must be Unavailable or Timeout")
	}
	if strings.TrimSpace(e.Failure.Reason) == "" {
		return fmt.Errorf("submission not attempted failure reason is required")
	}
	return nil
}

// ResourceObservation contains normalized resource facts. It does not contain
// ResourceState, ObservedGeneration, Conditions, or backend-specific data.
type ResourceObservation = domain.ObservedFacts

type ResourcePresence = domain.ResourcePresence
type ResourceReadiness = domain.ResourceReadiness
type ResourceDrift = domain.ResourceDrift

const (
	ResourcePresencePresent   = domain.ResourcePresencePresent
	ResourcePresenceNotFound  = domain.ResourcePresenceNotFound
	ResourcePresenceUnknown   = domain.ResourcePresenceUnknown
	ResourceReadinessReady    = domain.ResourceReadinessReady
	ResourceReadinessNotReady = domain.ResourceReadinessNotReady
	ResourceReadinessUnknown  = domain.ResourceReadinessUnknown
	ResourceDriftInSync       = domain.ResourceDriftInSync
	ResourceDriftDrifted      = domain.ResourceDriftDrifted
	ResourceDriftUnknown      = domain.ResourceDriftUnknown
)

// OutputEvidenceState classifies provider output evidence for one execution.
type OutputEvidenceState string

const (
	// OutputsUnavailable means the declared outputs could not be extracted or
	// decoded right now. The condition is transient: Liftr must keep the
	// operation active and retry extraction without re-executing the backend.
	OutputsUnavailable OutputEvidenceState = "Unavailable"
	// OutputsAvailable carries candidate values that passed the private
	// mapping boundary. They are implementation input to Liftr and remain
	// untrusted until the ResourceType contract validates them.
	OutputsAvailable OutputEvidenceState = "Available"
	// OutputsInvalid means extraction produced a permanent, deterministic
	// contract violation — undeclared fields, wrong types, wrong identity,
	// secret-marked material, or malformed envelopes. Retrying can never
	// succeed; no raw offending value may appear in Reason.
	OutputsInvalid OutputEvidenceState = "Invalid"
)

func (s OutputEvidenceState) valid() bool {
	switch s {
	case OutputsUnavailable, OutputsAvailable, OutputsInvalid:
		return true
	default:
		return false
	}
}

// OutputEvidence is the normalized output dimension of an ExecutionObservation.
// Values are flat scalars only. Secret material never crosses this boundary:
// implementations extract only explicitly registered non-secret outputs.
type OutputEvidence struct {
	State  OutputEvidenceState
	Values map[string]any
	// OutputMappingRef is the private mapping implementation that produced the
	// candidate values. It identifies the selected execution mapping, never a
	// source envelope identity supplied for recovery.
	OutputMappingRef string
	// Reason is a curated, client-safe classification for Invalid evidence.
	Reason string
}

func (e OutputEvidence) validate() error {
	if !e.State.valid() {
		return fmt.Errorf("invalid output evidence state %q", e.State)
	}
	if e.State == OutputsAvailable && len(e.Values) == 0 {
		return fmt.Errorf("available output evidence carries no values")
	}
	if e.State == OutputsInvalid && strings.TrimSpace(e.Reason) == "" {
		return fmt.Errorf("invalid output evidence requires a curated reason")
	}
	return nil
}

// ExecutionObservation separates current execution from resource facts. A
// ready existing resource can therefore be observed with Execution == nil.
type ExecutionObservation struct {
	Correlation RequestCorrelation
	Execution   *Execution
	Resource    ResourceObservation
	// ObservedAt is an optional backend source timestamp. When absent, the
	// application uses its caller-supplied observation receipt timestamp.
	ObservedAt time.Time
	// Outputs carries the optional realized-value evidence for create/update
	// executions of ResourceTypes that declare an output contract. A nil
	// Outputs means this observation makes no output claim at all — not that
	// outputs are unavailable.
	Outputs *OutputEvidence
}

// ObservationError carries a normalized failure for an observation call. It
// is distinct from an observed execution failure.
type ObservationError struct {
	Failure ExecutionFailure
}

func (e ObservationError) Error() string {
	return fmt.Sprintf("%s: %s", ErrObservationFailure, e.Failure.Error())
}
func (e ObservationError) Unwrap() error { return ErrObservationFailure }
