// SPDX-License-Identifier: Apache-2.0

package main

// The deterministic demo provisioner. It implements the exact provider-neutral
// provisioning contract but models NO real infrastructure backend: readiness is
// computed purely from the submitted spec so every demo beat is reproducible.
// It must never be registered outside cmd/liftr-demo-server.

import (
	"context"
	"sync"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

type demoProvisioner struct {
	mu          sync.Mutex
	submissions map[domain.OperationID]int
	// released records the deterministic demo control decisions made through
	// the local control listener (see main.go). It stands in for a real
	// backend finishing asynchronous work; nothing else can flip a held
	// Resource because Liftr deliberately offers no forced-success primitive.
	released map[string]bool
}

func newDemoProvisioner() *demoProvisioner {
	return &demoProvisioner{
		submissions: make(map[domain.OperationID]int),
		released:    make(map[string]bool),
	}
}

// Release marks one Resource converged in the simulated backend. It is called
// only by the loopback demo control listener.
func (p *demoProvisioner) Release(resourceID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.released[resourceID] = true
}

func (p *demoProvisioner) isReleased(resourceID domain.ResourceID) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.released[string(resourceID)]
}

func (p *demoProvisioner) Capabilities() []provisioning.ProvisionerCapability {
	var capabilities []provisioning.ProvisionerCapability
	for _, name := range []string{"DemoApp", "DemoDatabase", "DemoFault"} {
		ref := demoTypeRef(name)
		capabilities = append(capabilities,
			provisioning.ProvisionerCapability{ResourceType: ref, Capability: domain.CapabilityCreate},
			provisioning.ProvisionerCapability{ResourceType: ref, Capability: domain.CapabilityUpdate},
			provisioning.ProvisionerCapability{ResourceType: ref, Capability: domain.CapabilityDelete})
	}
	return capabilities
}

func (p *demoProvisioner) submissionCount(operationID domain.OperationID) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.submissions[operationID]
}

func (p *demoProvisioner) Submit(_ context.Context, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	if err := request.Validate(); err != nil {
		return provisioning.Submission{}, err
	}
	p.mu.Lock()
	p.submissions[request.OperationID]++
	p.mu.Unlock()

	switch request.ResourceType {
	case demoTypeRef("DemoFault"):
		return p.submitFault(request)
	default:
		return p.submitHoldable(request)
	}
}

// submitFault scripts the DemoFault/v1 scenarios documented on faultSchema.
func (p *demoProvisioner) submitFault(request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	scenario, _ := request.Spec.Values()["scenario"].(string)
	switch scenario {
	case "failure":
		if request.Capability != domain.CapabilityDelete {
			return failureSubmission("DeterministicFailure", "demo backend fails create/update conclusively"), nil
		}
		return destroySuccess(), nil
	case "ambiguous":
		if request.AttemptNumber == 1 {
			handle, handleErr := provisioning.NewExecutionHandle("demo-ambiguous-" + string(request.OperationID))
			if handleErr != nil {
				return provisioning.Submission{}, handleErr
			}
			unknown := provisioning.Submission{Observation: provisioning.ExecutionObservation{
				Correlation: provisioning.RequestCorrelationFound,
				Execution: &provisioning.Execution{
					State:   provisioning.ExecutionStateUnknown,
					Handle:  &handle,
					Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "OutcomeUnknown", Message: "demo backend could not confirm the submission outcome"},
				},
				Resource: provisioning.ResourceObservation{Presence: provisioning.ResourcePresenceUnknown,
					Readiness: provisioning.ResourceReadinessUnknown, Drift: provisioning.ResourceDriftUnknown},
			}}
			return unknown, provisioning.ErrAmbiguousSubmission
		}
		return provisioning.Submission{Observation: successObservation()}, nil
	default:
		return provisioning.Submission{Observation: successObservation()}, nil
	}
}

// submitHoldable scripts DemoDatabase/v1 and DemoApp/v1. spec.hold gates
// convergence, while the demo-only spec.holdDelete gate can independently
// keep destruction in progress until the control plane releases it.
func (p *demoProvisioner) submitHoldable(request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	if p.held(request.ResourceID, request.Spec, request.Capability) {
		return provisioning.Submission{Observation: provisioning.ExecutionObservation{
			Correlation: provisioning.RequestCorrelationFound,
			Execution:   &provisioning.Execution{State: provisioning.ExecutionStateAccepted},
			Resource: provisioning.ResourceObservation{Presence: provisioning.ResourcePresencePresent,
				Readiness: provisioning.ResourceReadinessNotReady, Drift: provisioning.ResourceDriftInSync},
		}}, nil
	}
	if request.Capability == domain.CapabilityDelete {
		return destroySuccess(), nil
	}
	return provisioning.Submission{Observation: successObservation()}, nil
}

// held reports whether the simulated backend still withholds convergence for
// this Resource.
func (p *demoProvisioner) held(resourceID domain.ResourceID, spec domain.ResourceSpec, capability domain.Capability) bool {
	if p.isReleased(resourceID) {
		return false
	}
	if capability == domain.CapabilityDelete {
		holdDelete, _ := spec.Values()["holdDelete"].(bool)
		if holdDelete {
			return true
		}
	}
	return specHold(spec)
}

func (p *demoProvisioner) Observe(_ context.Context, request provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	if err := request.Validate(); err != nil {
		return provisioning.ExecutionObservation{}, err
	}
	if request.ResourceType == demoTypeRef("DemoFault") {
		if scenario, _ := request.Spec.Values()["scenario"].(string); scenario == "failure" && request.Capability != domain.CapabilityDelete {
			return failureObservation("DeterministicFailure", "demo backend fails create/update conclusively"), nil
		}
		return successObservation(), nil
	}
	// DemoDatabase/DemoApp: the same deterministic hold gate as Submit governs
	// observations, so a held Resource (including one being destroyed) stays
	// nonterminal until released through the demo control plane.
	if p.held(request.ResourceID, request.Spec, request.Capability) {
		return runningObservation(), nil
	}
	if request.Capability == domain.CapabilityDelete {
		return destroyObservation(), nil
	}
	return successObservation(), nil
}

func destroyObservation() provisioning.ExecutionObservation {
	return provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateSucceeded},
		Resource: provisioning.ResourceObservation{Presence: provisioning.ResourcePresenceNotFound,
			Readiness: provisioning.ResourceReadinessNotReady, Drift: provisioning.ResourceDriftInSync},
	}
}
func successObservation() provisioning.ExecutionObservation {
	return provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateSucceeded},
		Resource: provisioning.ResourceObservation{Presence: provisioning.ResourcePresencePresent,
			Readiness: provisioning.ResourceReadinessReady, Drift: provisioning.ResourceDriftInSync},
	}
}

func runningObservation() provisioning.ExecutionObservation {
	return provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateRunning},
		Resource: provisioning.ResourceObservation{Presence: provisioning.ResourcePresencePresent,
			Readiness: provisioning.ResourceReadinessNotReady, Drift: provisioning.ResourceDriftInSync},
	}
}

func destroySuccess() provisioning.Submission {
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateSucceeded},
		Resource: provisioning.ResourceObservation{Presence: provisioning.ResourcePresenceNotFound,
			Readiness: provisioning.ResourceReadinessNotReady, Drift: provisioning.ResourceDriftInSync},
	}}
}

func failureSubmission(reason, message string) provisioning.Submission {
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateFailed,
			Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureExecution, Reason: reason, Message: message}},
		Resource: provisioning.ResourceObservation{Presence: provisioning.ResourcePresencePresent,
			Readiness: provisioning.ResourceReadinessNotReady, Drift: provisioning.ResourceDriftInSync},
	}}
}

func failureObservation(reason, message string) provisioning.ExecutionObservation {
	return provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateFailed,
			Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureExecution, Reason: reason, Message: message}},
		Resource: provisioning.ResourceObservation{Presence: provisioning.ResourcePresencePresent,
			Readiness: provisioning.ResourceReadinessNotReady, Drift: provisioning.ResourceDriftInSync},
	}
}

func specHold(spec domain.ResourceSpec) bool {
	held, _ := spec.Values()["hold"].(bool)
	return held
}
