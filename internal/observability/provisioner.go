// SPDX-License-Identifier: Apache-2.0

package observability

import (
	"context"
	"errors"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ProvisionerKind is the bounded, software-defined backend family used as a
// METRIC dimension. It exists because private ProvisionerRef values are
// deployment-controlled strings whose length bounds say nothing about their
// cardinality; refs stay in structured logs and sampled traces only, while
// metrics carry this closed enum. A future provisioner adds exactly one
// code-defined value here (ADR-0018).
type ProvisionerKind string

const (
	ProvisionerKindPulumi     ProvisionerKind = "pulumi"
	ProvisionerKindCrossplane ProvisionerKind = "crossplane"
)

// Valid reports whether k is one of the enumerated kinds.
func (k ProvisionerKind) Valid() bool {
	switch k {
	case ProvisionerKindPulumi, ProvisionerKindCrossplane:
		return true
	default:
		return false
	}
}

// Bounded Submit outcomes.
const (
	ProvOutcomeSuccess     = "success"
	ProvOutcomeRejected    = "rejected"
	ProvOutcomeUnavailable = "unavailable"
	ProvOutcomeAmbiguous   = "ambiguous"
	ProvOutcomeError       = "error"
)

// Bounded Observe outcomes.
const (
	ObsOutcomeNone       = "none"
	ObsOutcomeRunning    = "running"
	ObsOutcomeSucceeded  = "succeeded"
	ObsOutcomeFailed     = "failed"
	ObsOutcomeUnknown    = "unknown"
	ObsOutcomeUnavailabl = "unavailable"
)

type instrumentedProvisioner struct {
	inner provisioning.Provisioner
	kind  ProvisionerKind
	tel   *Telemetry
}

// InstrumentProvisioner wraps one registered provisioner with provider-neutral
// metrics and boundary spans. The provisioning.Provisioner interface is
// unchanged; wrapped instances are installed at composition so every worker
// call flows through the same seam. Metric labels are exclusively
// provisioner kind + capability + bounded outcome — never the private ref,
// stack names, GVKs, handles, or provider text (ADR-0018).
func InstrumentProvisioner(inner provisioning.Provisioner, kind ProvisionerKind, tel *Telemetry) (provisioning.Provisioner, error) {
	if !kind.Valid() {
		return nil, errors.New("provisioner kind must be a code-defined enum value")
	}
	if inner == nil || tel == nil || tel.instruments == nil {
		return nil, errors.New("instrumented provisioner dependencies are required")
	}
	return &instrumentedProvisioner{inner: inner, kind: kind, tel: tel}, nil
}

func (p *instrumentedProvisioner) Capabilities() []provisioning.ProvisionerCapability {
	return p.inner.Capabilities()
}

func (p *instrumentedProvisioner) Submit(ctx context.Context, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	ctx, span := p.tel.Tracer().StartSpan(ctx, "provisioning.Submit",
		attribute.String(attrProvKind, string(p.kind)),
		attribute.String(attrCapability, string(request.Capability)),
	)
	submission, err := p.inner.Submit(ctx, request)
	outcome := classifySubmission(submission, err)
	p.recordCall(p.tel.instruments.provSubmissions, attrProvMethodSubmitValue, request.Capability, outcome,
		time.Duration(0), span)
	span.SetString("liftr.operation_id", string(request.OperationID))
	span.SetInt("liftr.attempt_number", int64(request.AttemptNumber))
	span.RecordError(err)
	span.End()
	return submission, err
}

func (p *instrumentedProvisioner) Observe(ctx context.Context, request provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	started := time.Now()
	ctx, span := p.tel.Tracer().StartSpan(ctx, "provisioning.Observe",
		attribute.String(attrProvKind, string(p.kind)),
		attribute.String(attrCapability, string(request.Capability)),
	)
	observation, err := p.inner.Observe(ctx, request)
	outcome := classifyObservation(observation, err)
	attrs := p.baseAttrs(attrProvMethodObserveValue, request.Capability, outcome)
	p.tel.instruments.provObservations.Add(ctx, 1, metric.WithAttributes(attrs...))
	p.tel.instruments.provCallDuration.Record(ctx, time.Since(started).Seconds(), metric.WithAttributes(attrs...))
	if request.OperationID != "" {
		span.SetString("liftr.operation_id", string(request.OperationID))
		span.SetInt("liftr.attempt_number", int64(request.AttemptNumber))
	}
	if err != nil {
		span.RecordError(err)
	}
	span.End()
	return observation, err
}

const (
	attrProvMethodSubmitValue  = "submit"
	attrProvMethodObserveValue = "observe"
)

func (p *instrumentedProvisioner) baseAttrs(method string, capability domain.Capability, outcome string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String(attrProvKind, string(p.kind)),
		attribute.String(attrCapability, string(capability)),
		attribute.String(attrProvOutcome, outcome),
		attribute.String(attrProvMethod, method),
	}
}

func (p *instrumentedProvisioner) recordCall(counter metric.Int64Counter, method string, capability domain.Capability, outcome string, duration time.Duration, _ Span) {
	attrs := metric.WithAttributes(p.baseAttrs(method, capability, outcome)...)
	counter.Add(context.Background(), 1, attrs)
}

func classifySubmission(submission provisioning.Submission, err error) string {
	switch {
	case errors.Is(err, provisioning.ErrAmbiguousSubmission):
		return ProvOutcomeAmbiguous
	case err != nil:
		var observationErr provisioning.ObservationError
		if errors.As(err, &observationErr) {
			switch observationErr.Failure.Kind {
			case provisioning.FailureUnavailable, provisioning.FailureTimeout:
				return ProvOutcomeUnavailable
			default:
				return ProvOutcomeError
			}
		}
		return ProvOutcomeError
	case submission.Observation.Execution != nil && submission.Observation.Execution.State == provisioning.ExecutionStateFailed:
		failure := submission.Observation.Execution.Failure
		if failure != nil {
			switch failure.Kind {
			case provisioning.FailureInvalidRequest, provisioning.FailureUnsupported:
				return ProvOutcomeRejected
			case provisioning.FailureUnavailable, provisioning.FailureTimeout:
				return ProvOutcomeUnavailable
			}
		}
		return ProvOutcomeError
	default:
		return ProvOutcomeSuccess
	}
}

func classifyObservation(observation provisioning.ExecutionObservation, err error) string {
	if err != nil {
		var observationErr provisioning.ObservationError
		if errors.As(err, &observationErr) {
			switch observationErr.Failure.Kind {
			case provisioning.FailureUnavailable, provisioning.FailureTimeout:
				return ObsOutcomeUnavailabl
			default:
				return ProvOutcomeError
			}
		}
		return ProvOutcomeError
	}
	if observation.Execution == nil {
		return ObsOutcomeNone
	}
	switch observation.Execution.State {
	case provisioning.ExecutionStateAccepted, provisioning.ExecutionStateRunning:
		return ObsOutcomeRunning
	case provisioning.ExecutionStateSucceeded:
		return ObsOutcomeSucceeded
	case provisioning.ExecutionStateFailed:
		return ObsOutcomeFailed
	case provisioning.ExecutionStateUnknown:
		return ObsOutcomeUnknown
	default:
		return ProvOutcomeError
	}
}
