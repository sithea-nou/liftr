// SPDX-License-Identifier: Apache-2.0

package observability

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ClusterSample is one operational snapshot of durable PostgreSQL truth.
// Every value except Pool is CLUSTER-GLOBAL: every Liftr replica sampling the
// same database observes identical numbers, so dashboards must aggregate
// these gauges with max (or last), never sum. Pool is per-process state.
// ADR-0018 records this distinction.
type ClusterSample struct {
	SampledAt time.Time

	// OutboxPendingDepth counts Pending outbox messages.
	OutboxPendingDepth int64
	// OutboxPendingOldestAgeSeconds is the age of the oldest Pending message
	// by creation time; zero when nothing is pending — depth determines
	// whether an oldest item exists.
	OutboxPendingOldestAgeSeconds float64
	// OutboxExpiredLeases counts Leased messages past their lease deadline.
	OutboxExpiredLeases int64
	// OutboxDead counts quarantined messages.
	OutboxDead int64

	// ActiveOperations counts all Pending/Running Operations cluster-wide.
	ActiveOperations int64
	// ActiveOperationsOldestAgeSeconds is the age since requestedAt of the
	// oldest active Operation; zero when nothing is active.
	ActiveOperationsOldestAgeSeconds float64
	// LongRunningWarning / LongRunningCritical map capability -> count of
	// long-running stuck candidates at each severity threshold.
	LongRunningWarning  map[string]int64
	LongRunningCritical map[string]int64
	// ReconciliationSilent maps capability -> count of reconciliation-silent
	// stuck candidates: no observation or phase activity within the window.
	ReconciliationSilent map[string]int64

	// Pool is this process's connection-pool state.
	Pool PoolStats
}

// RecordClusterSample publishes one successful sample. Transient failures
// retain previous values while freshness exposes staleness (ADR-0018).
func (t *Telemetry) RecordClusterSample(sample ClusterSample) {
	if !t.ready() {
		return
	}
	ctx := context.Background()
	i := t.instruments
	i.outboxPendingDepth.Record(ctx, sample.OutboxPendingDepth)
	i.outboxPendingOldest.Record(ctx, int64(sample.OutboxPendingOldestAgeSeconds))
	i.outboxExpiredLeases.Record(ctx, sample.OutboxExpiredLeases)
	i.outboxDead.Record(ctx, sample.OutboxDead)
	i.opsActive.Record(ctx, sample.ActiveOperations)
	i.opsOldestActiveAge.Record(ctx, int64(sample.ActiveOperationsOldestAgeSeconds))
	recordCapabilityCounts(ctx, i.opsLongRunning, attribute.String(attrSeverity, "warning"), sample.LongRunningWarning)
	recordCapabilityCounts(ctx, i.opsLongRunning, attribute.String(attrSeverity, "critical"), sample.LongRunningCritical)
	recordCapabilityCounts(ctx, i.opsSilent, attribute.KeyValue{}, sample.ReconciliationSilent)
	if !sample.SampledAt.IsZero() {
		i.samplerFreshness.Record(ctx, sample.SampledAt.Unix())
	}
	t.recordPoolStats(sample.Pool)
}

// SampleFailed records one failed sampling cycle without touching gauges.
func (t *Telemetry) SampleFailed() {
	if !t.ready() {
		return
	}
	t.instruments.samplerFailures.Add(context.Background(), 1)
}

func recordCapabilityCounts(ctx context.Context, gauge metric.Int64Gauge, severity attribute.KeyValue, counts map[string]int64) {
	for capability, count := range counts {
		attrs := []attribute.KeyValue{attribute.String(attrCapability, capability)}
		if severity.Valid() {
			attrs = append(attrs, severity)
		}
		gauge.Record(ctx, count, metric.WithAttributes(attrs...))
	}
}
