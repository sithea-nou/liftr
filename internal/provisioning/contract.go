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
	ErrAmbiguousSubmission = errors.New("provisioner submission outcome is ambiguous")
	ErrObservationFailure  = errors.New("provisioner observation failed")
)

// Provisioner is the minimum contract required by Liftr. Submit sends
// provider-neutral intent; Observe reads the backend's current facts.
type Provisioner interface {
	Capabilities() []ProvisionerCapability
	Submit(context.Context, ExecutionRequest) (Submission, error)
	Observe(context.Context, ObservationRequest) (ExecutionObservation, error)
}

// ProvisionerCapability describes a ResourceType action supported by a backend.
type ProvisionerCapability struct {
	ResourceType domain.ResourceTypeRef
	Capability   domain.Capability
}

// ExecutionRequest contains only the immutable intent and correlation data a
// provisioner needs. It does not contain Liftr Operation state or phase.
type ExecutionRequest struct {
	OperationID      domain.OperationID
	ResourceID       domain.ResourceID
	ResourceType     domain.ResourceTypeRef
	Spec             domain.ResourceSpec
	Capability       domain.Capability
	TargetGeneration uint64
}

func (r ExecutionRequest) Validate() error {
	if strings.TrimSpace(string(r.OperationID)) == "" {
		return fmt.Errorf("operation ID is required")
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

// ObservationRequest identifies what the provisioner should observe. Handle
// is optional for declarative backends that observe by resource identity.
type ObservationRequest struct {
	OperationID      domain.OperationID
	ResourceID       domain.ResourceID
	ResourceType     domain.ResourceTypeRef
	Spec             domain.ResourceSpec
	TargetGeneration uint64
	Handle           *ExecutionHandle
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

type ResourcePresence string

const (
	ResourcePresencePresent  ResourcePresence = "Present"
	ResourcePresenceNotFound ResourcePresence = "NotFound"
	ResourcePresenceUnknown  ResourcePresence = "Unknown"
)

type ResourceReadiness string

const (
	ResourceReadinessReady    ResourceReadiness = "Ready"
	ResourceReadinessNotReady ResourceReadiness = "NotReady"
	ResourceReadinessUnknown  ResourceReadiness = "Unknown"
)

type ResourceDrift string

const (
	ResourceDriftInSync  ResourceDrift = "InSync"
	ResourceDriftDrifted ResourceDrift = "Drifted"
	ResourceDriftUnknown ResourceDrift = "Unknown"
)

// ResourceObservation contains normalized resource facts. It does not contain
// ResourceState, ObservedGeneration, Conditions, or backend-specific data.
type ResourceObservation struct {
	Presence  ResourcePresence
	Readiness ResourceReadiness
	Drift     ResourceDrift
}

// ExecutionObservation separates current execution from resource facts. A
// ready existing resource can therefore be observed with Execution == nil.
type ExecutionObservation struct {
	Execution  *Execution
	Resource   ResourceObservation
	ObservedAt time.Time
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
