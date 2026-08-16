// SPDX-License-Identifier: Apache-2.0

// Package fake provides a deterministic contract test provisioner. It does
// not model any real infrastructure backend.
package fake

import (
	"context"
	"fmt"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

type Mode string

const (
	ModeSynchronous        Mode = "Synchronous"
	ModeAsynchronous       Mode = "Asynchronous"
	ModeDeclarative        Mode = "Declarative"
	ModeExisting           Mode = "Existing"
	ModeFailure            Mode = "Failure"
	ModeObservationFailure Mode = "ObservationFailure"
	ModeAmbiguous          Mode = "Ambiguous"
	ModeDrift              Mode = "Drift"
)

var resourceType = domain.ResourceTypeRef{Name: "FakeResource", Version: "v1"}

type executionRecord struct {
	handle        provisioning.ExecutionHandle
	observations  int
	submission    provisioning.Submission
	submissionErr error
}

type Provisioner struct {
	mode        Mode
	executions  map[domain.OperationID]*executionRecord
	submissions map[domain.OperationID]int
}

func New(mode Mode) *Provisioner {
	return &Provisioner{
		mode:        mode,
		executions:  make(map[domain.OperationID]*executionRecord),
		submissions: make(map[domain.OperationID]int),
	}
}

func ResourceType() domain.ResourceTypeRef { return resourceType }

func (p *Provisioner) Capabilities() []provisioning.ProvisionerCapability {
	return []provisioning.ProvisionerCapability{
		{ResourceType: resourceType, Capability: domain.CapabilityCreate},
		{ResourceType: resourceType, Capability: domain.CapabilityUpdate},
		{ResourceType: resourceType, Capability: domain.CapabilityDelete},
	}
}

func (p *Provisioner) Submit(_ context.Context, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	if err := request.Validate(); err != nil {
		return provisioning.Submission{}, err
	}
	if record, ok := p.executions[request.OperationID]; ok {
		return record.submission, record.submissionErr
	}
	p.submissions[request.OperationID]++
	if request.ResourceType != resourceType {
		return failureSubmission(provisioning.FailureUnsupported, "ResourceTypeUnsupported", "fake resource type is unsupported"), nil
	}
	if request.Capability != domain.CapabilityCreate && request.Capability != domain.CapabilityUpdate && request.Capability != domain.CapabilityDelete {
		return failureSubmission(provisioning.FailureUnsupported, "CapabilityUnsupported", "fake capability is unsupported"), nil
	}

	handle, err := provisioning.NewExecutionHandle("fake-execution-" + string(request.OperationID))
	if err != nil {
		return provisioning.Submission{}, err
	}
	record := &executionRecord{handle: handle}
	p.executions[request.OperationID] = record

	switch p.mode {
	case ModeSynchronous:
		record.submission = provisioning.Submission{Observation: p.observation(handle, provisioning.ExecutionStateSucceeded, true, false, false)}
	case ModeAsynchronous:
		record.submission = provisioning.Submission{Observation: p.observation(handle, provisioning.ExecutionStateAccepted, false, false, false)}
	case ModeDeclarative:
		record.submission = provisioning.Submission{Observation: provisioning.ExecutionObservation{
			Execution:  &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle},
			Resource:   resourceObservation(false, false, false),
			ObservedAt: time.Unix(0, 0).UTC(),
		}}
	case ModeFailure:
		record.submission = provisioning.Submission{Observation: provisioning.ExecutionObservation{
			Execution: &provisioning.Execution{
				State:   provisioning.ExecutionStateFailed,
				Handle:  &handle,
				Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureExecution, Reason: "ExecutionFailed", Message: "fake execution failed"},
			},
			Resource:   resourceObservation(false, false, false),
			ObservedAt: time.Unix(0, 0).UTC(),
		}}
	case ModeAmbiguous:
		record.submission = provisioning.Submission{Observation: provisioning.ExecutionObservation{
			Execution: &provisioning.Execution{
				State:   provisioning.ExecutionStateUnknown,
				Handle:  &handle,
				Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "SubmissionUnknown", Message: "fake submission outcome is unknown"},
			},
			Resource:   resourceObservation(false, false, false),
			ObservedAt: time.Unix(0, 0).UTC(),
		}}
		record.submissionErr = provisioning.ErrAmbiguousSubmission
		return record.submission, record.submissionErr
	case ModeObservationFailure:
		record.submission = provisioning.Submission{Observation: p.observation(handle, provisioning.ExecutionStateAccepted, false, false, false)}
	case ModeDrift:
		record.submission = provisioning.Submission{Observation: p.observation(handle, provisioning.ExecutionStateAccepted, false, false, false)}
	default:
		delete(p.executions, request.OperationID)
		return provisioning.Submission{}, fmt.Errorf("unsupported fake mode %q", p.mode)
	}
	return record.submission, nil
}

func (p *Provisioner) Observe(_ context.Context, request provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	if err := request.Validate(); err != nil {
		return provisioning.ExecutionObservation{}, err
	}
	if p.mode == ModeObservationFailure {
		return provisioning.ExecutionObservation{}, provisioning.ObservationError{Failure: provisioning.ExecutionFailure{
			Kind: provisioning.FailureUnavailable, Reason: "ObservationUnavailable", Message: "fake observation failed",
		}}
	}
	if p.mode == ModeExisting {
		return provisioning.ExecutionObservation{
			Resource:   resourceObservation(true, true, false),
			ObservedAt: time.Unix(0, 0).UTC(),
		}, nil
	}

	if request.OperationID == "" {
		return provisioning.ExecutionObservation{}, fmt.Errorf("operation ID is required for fake execution observation")
	}
	record, ok := p.executions[request.OperationID]
	if !ok {
		return provisioning.ExecutionObservation{
			Resource:   provisioning.ResourceObservation{Presence: provisioning.ResourcePresenceNotFound, Readiness: provisioning.ResourceReadinessUnknown, Drift: provisioning.ResourceDriftUnknown},
			ObservedAt: time.Unix(0, 0).UTC(),
		}, nil
	}
	record.observations++
	switch p.mode {
	case ModeAsynchronous:
		if record.observations == 1 {
			return p.observation(record.handle, provisioning.ExecutionStateRunning, false, false, false), nil
		}
		return p.observation(record.handle, provisioning.ExecutionStateSucceeded, true, false, false), nil
	case ModeDeclarative:
		if record.observations == 1 {
			return provisioning.ExecutionObservation{Resource: resourceObservation(true, false, false), ObservedAt: time.Unix(0, 0).UTC()}, nil
		}
		return provisioning.ExecutionObservation{Resource: resourceObservation(true, true, false), ObservedAt: time.Unix(0, 0).UTC()}, nil
	case ModeDrift:
		return provisioning.ExecutionObservation{Resource: resourceObservation(true, true, true), ObservedAt: time.Unix(0, 0).UTC()}, nil
	default:
		return p.observation(record.handle, provisioning.ExecutionStateSucceeded, true, false, false), nil
	}
}

func (p *Provisioner) SubmissionCount(operationID domain.OperationID) int {
	return p.submissions[operationID]
}

func (p *Provisioner) observation(handle provisioning.ExecutionHandle, state provisioning.ExecutionState, ready, notFound, drifted bool) provisioning.ExecutionObservation {
	return provisioning.ExecutionObservation{
		Execution:  &provisioning.Execution{State: state, Handle: &handle},
		Resource:   resourceObservation(!notFound, ready, drifted),
		ObservedAt: time.Unix(0, 0).UTC(),
	}
}

func resourceObservation(present, ready, drifted bool) provisioning.ResourceObservation {
	presence := provisioning.ResourcePresenceUnknown
	if present {
		presence = provisioning.ResourcePresencePresent
	} else if !ready && !drifted {
		presence = provisioning.ResourcePresenceUnknown
	}
	readiness := provisioning.ResourceReadinessUnknown
	if ready {
		readiness = provisioning.ResourceReadinessReady
	} else if present {
		readiness = provisioning.ResourceReadinessNotReady
	}
	drift := provisioning.ResourceDriftInSync
	if drifted {
		drift = provisioning.ResourceDriftDrifted
	}
	return provisioning.ResourceObservation{Presence: presence, Readiness: readiness, Drift: drift}
}

func failureSubmission(kind provisioning.ExecutionFailureKind, reason, message string) provisioning.Submission {
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Execution:  &provisioning.Execution{State: provisioning.ExecutionStateFailed, Failure: &provisioning.ExecutionFailure{Kind: kind, Reason: reason, Message: message}},
		Resource:   resourceObservation(false, false, false),
		ObservedAt: time.Unix(0, 0).UTC(),
	}}
}
