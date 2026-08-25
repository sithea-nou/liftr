// SPDX-License-Identifier: Apache-2.0

package observability

import (
	"context"

	"github.com/sithea-nou/liftr/internal/worker"
	"go.opentelemetry.io/otel/metric"
)

// operationStateLabel maps the durable Operation state onto the bounded
// lowercase metric label; unknown values collapse into a fixed fallback so
// labels can never become unbounded.
func operationStateLabel(state string) string {
	switch state {
	case "Succeeded":
		return "succeeded"
	case "Failed":
		return "failed"
	case "Canceled":
		return "canceled"
	default:
		return "unknown"
	}
}

// WorkCompleted adapts the worker's per-item events onto instruments and the
// operator log. Successes on polling kinds log at DEBUG so continuous
// Crossplane observation cannot flood INFO; every non-success outcome is
// WARN; panics are ERROR (ADR-0018).
func (t *Telemetry) WorkCompleted(event worker.WorkEvent) {
	if !t.ready() {
		return
	}
	t.instruments.workerWork.Add(context.Background(), 1, metric.WithAttributes(
		attributeString(attrWorkerKind, event.Kind),
		attributeString(attrWorkOutcome, event.Outcome),
	))
	if t.config.Logger == nil {
		return
	}
	switch event.Outcome {
	case WorkOutcomeSuccess:
		t.config.Logger.Debug("worker item completed", workLogArgs(event)...)
	case WorkOutcomePanic:
		return // logged at ERROR by WorkerPanic
	default:
		t.config.Logger.Warn("worker item did not complete cleanly", workLogArgs(event)...)
	}
}

func workLogArgs(event worker.WorkEvent) []any {
	args := []any{
		"worker_kind", event.Kind,
		"outcome", event.Outcome,
		"error_class", "",
	}
	if event.OperationID != "" {
		args = append(args, "operation_id", event.OperationID)
	}
	if event.ResourceID != "" {
		args = append(args, "resource_id", event.ResourceID)
	}
	if event.AttemptNumber != 0 {
		args = append(args, "attempt_number", event.AttemptNumber)
	}
	if event.ErrorClass != "" {
		args[len(args)-1] = event.ErrorClass
	}
	return args
}

// OperationTerminalized records one real durable terminal transition and logs
// it at INFO with its persisted requestedAt -> completedAt latency.
func (t *Telemetry) OperationTerminalized(event worker.TerminalEvent) {
	if !t.ready() {
		return
	}
	state := operationStateLabel(event.TerminalState)
	attrs := metric.WithAttributes(
		attributeString(attrCapability, event.Capability),
		attributeString(attrOperationStat, state),
	)
	t.instruments.opsTerminal.Add(context.Background(), 1, attrs)
	t.instruments.opDuration.Record(context.Background(), event.DurationSeconds, attrs)
	if t.config.Logger != nil {
		t.config.Logger.Info("operation reached terminal state",
			"operation_id", event.OperationID,
			"resource_id", event.ResourceID,
			"capability", event.Capability,
			"state", state,
			"duration_seconds", event.DurationSeconds,
			"error_class", "",
		)
	}
}

// WorkerPanic logs one recovered per-work panic and counts it.
func (t *Telemetry) WorkerPanic(kind string, value string) {
	if !t.ready() {
		return
	}
	t.instruments.workerPanics.Add(context.Background(), 1, metric.WithAttributes(
		attributeString(attrWorkerKind, kind)))
	if t.config.Logger != nil {
		t.config.Logger.Error("worker panic recovered",
			"error_class", "panic",
			"panic_value", value,
			"worker_kind", kind,
		)
	}
}
