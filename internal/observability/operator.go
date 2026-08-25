// SPDX-License-Identifier: Apache-2.0

package observability

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

func (t *Telemetry) OperatorRequest(action, result string) {
	if !t.ready() {
		return
	}
	t.instruments.operatorRequests.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String(attrOperatorAction, boundedOperatorAction(action)),
		attribute.String(attrOperatorResult, boundedOperatorResult(result)),
	))
}

func (t *Telemetry) OperatorRecovery(kind, result string) {
	if !t.ready() {
		return
	}
	t.instruments.operatorRecoveries.Add(context.Background(), 1, metric.WithAttributes(
		attribute.String(attrRecoveryKind, boundedRecoveryKind(kind)),
		attribute.String(attrOperatorResult, boundedOperatorResult(result)),
	))
}

func boundedOperatorAction(value string) string {
	switch value {
	case "diagnostics_read", "trigger_observe", "trigger_passive_observe", "recover_dead_work":
		return value
	default:
		return "other"
	}
}

func boundedOperatorResult(value string) string {
	switch value {
	case "read", "applied", "replayed", "stale", "not_applicable", "unsafe", "conflict", "denied", "error":
		return value
	default:
		return "error"
	}
}

func boundedRecoveryKind(value string) string {
	switch value {
	case "Dispatch", "Observe", "PassiveObserve", "Drive":
		return value
	default:
		return "other"
	}
}
