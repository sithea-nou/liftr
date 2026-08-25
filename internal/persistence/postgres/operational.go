// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"fmt"
	"time"
)

// OperationalSnapshot is one bounded sample of durable cluster truth for the
// operational-sampler gauges. Every value except pool statistics is
// cluster-global: all replicas sampling the same database observe identical
// numbers, so consumers must aggregate with max, not sum (ADR-0018).
type OperationalSnapshot struct {
	SampledAt time.Time

	OutboxPendingDepth              int64
	OutboxPendingOldestAgeSeconds   float64 // zero when nothing is pending; depth says whether an oldest item exists
	OutboxExpiredLeases             int64
	OutboxDead                      int64
	ActiveOperations                int64
	ActiveOldestAgeSeconds          float64 // zero when nothing is active
	LongRunningWarningByCapability  map[string]int64
	LongRunningCriticalByCapability map[string]int64
	SilentByCapability              map[string]int64
}

// PoolStatsSnapshot is per-process connection-pool state.
type PoolStatsSnapshot struct {
	Acquired   int64
	Idle       int64
	Connecting int64
	MaxTotal   int64
}

// DiagnosticThresholds carries the configurable stuck-candidate thresholds.
// They drive diagnostic gauges only and never influence lifecycle state
// (ADR-0018).
type DiagnosticThresholds struct {
	LongRunningWarnAfter time.Duration
	LongRunningCritAfter time.Duration
	SilentAfter          time.Duration
}

// The sampler aggregates ride the partial indexes that already serve claim
// and expiry-recovery paths (outbox_claimable, outbox_expired_leases) plus
// the partial Dead index; the active-Operation predicates ride the partial
// unique index on active operations. Nothing scans terminal history.
const (
	// Empty queues report age 0, never a negative sentinel: depth determines
	// whether an oldest item exists (ADR-0018).
	sampleOutboxDepthSQL = `
		SELECT count(*),
		       CASE WHEN count(*) = 0 THEN 0
		            ELSE EXTRACT(EPOCH FROM (clock_timestamp() - min(created_at)))
		       END
		FROM outbox_messages WHERE state = 'Pending'`

	sampleExpiredLeasesSQL = `
		SELECT count(*)
		FROM outbox_messages
		WHERE state = 'Leased' AND leased_until <= clock_timestamp()`

	sampleDeadSQL = `
		SELECT count(*)
		FROM outbox_messages
		WHERE state = 'Dead'`

	sampleActiveOperationsSQL = `
		SELECT count(*),
		       CASE WHEN count(*) = 0 THEN 0
		            ELSE EXTRACT(EPOCH FROM (clock_timestamp() - to_timestamp(min(requested_at_ns) / 1000000000.0)))
		       END
		FROM operations
		WHERE state IN ('Pending', 'Running')`

	sampleLongRunningSQL = `
		SELECT capability, count(*)
		FROM operations
		WHERE state IN ('Pending', 'Running')
		  AND requested_at_ns <= ((EXTRACT(EPOCH FROM clock_timestamp()) * 1000000000)::bigint - $1)
		GROUP BY capability`

	// sampleReconciliationSilentSQL measures ACTIVITY, not progress: an
	// Operation counts as reconciliation-silent only when neither Liftr-side
	// observation activity (last_observed_at_ns) nor any lifecycle phase/state
	// transition (phase_changed_at_ns, started_at_ns) has occurred within the
	// window. A continuously-observed backend that never converges stays
	// silent=false while its long-running gauge rises (ADR-0018).
	sampleReconciliationSilentSQL = `
		WITH active AS (
			SELECT o.capability AS capability,
			       GREATEST(
			           COALESCE(e.last_observed_at_ns, o.requested_at_ns),
			           COALESCE(o.phase_changed_at_ns, o.requested_at_ns),
			           COALESCE(o.started_at_ns, o.requested_at_ns),
			           o.requested_at_ns) AS last_activity_ns
			FROM operations o
			LEFT JOIN provisioning_executions e ON e.operation_id = o.id
			WHERE o.state IN ('Pending', 'Running')
		)
		SELECT capability, count(*)
		FROM active
		WHERE ((EXTRACT(EPOCH FROM clock_timestamp()) * 1000000000)::bigint - last_activity_ns) > $1
		GROUP BY capability`
)

// SnapshotOperationalState runs the bounded aggregate queries backing the
// operational sampler. Callers invoke this from a periodic goroutine under a
// strict context budget — never synchronously from a metrics scrape.
func (s *Store) SnapshotOperationalState(ctx context.Context, thresholds DiagnosticThresholds) (OperationalSnapshot, error) {
	snapshot := OperationalSnapshot{
		SampledAt:                       time.Now().UTC(),
		LongRunningWarningByCapability:  map[string]int64{},
		LongRunningCriticalByCapability: map[string]int64{},
		SilentByCapability:              map[string]int64{},
	}

	if err := s.pool.QueryRow(ctx, sampleOutboxDepthSQL).
		Scan(&snapshot.OutboxPendingDepth, &snapshot.OutboxPendingOldestAgeSeconds); err != nil {
		return OperationalSnapshot{}, fmt.Errorf("sample outbox depth: %w", err)
	}
	if err := s.pool.QueryRow(ctx, sampleExpiredLeasesSQL).Scan(&snapshot.OutboxExpiredLeases); err != nil {
		return OperationalSnapshot{}, fmt.Errorf("sample expired leases: %w", err)
	}
	if err := s.pool.QueryRow(ctx, sampleDeadSQL).Scan(&snapshot.OutboxDead); err != nil {
		return OperationalSnapshot{}, fmt.Errorf("sample dead messages: %w", err)
	}
	if err := s.pool.QueryRow(ctx, sampleActiveOperationsSQL).
		Scan(&snapshot.ActiveOperations, &snapshot.ActiveOldestAgeSeconds); err != nil {
		return OperationalSnapshot{}, fmt.Errorf("sample active operations: %w", err)
	}
	if err := s.scanCapabilityCounts(ctx, snapshot.LongRunningWarningByCapability,
		sampleLongRunningSQL, thresholds.LongRunningWarnAfter); err != nil {
		return OperationalSnapshot{}, fmt.Errorf("sample long-running warning: %w", err)
	}
	if err := s.scanCapabilityCounts(ctx, snapshot.LongRunningCriticalByCapability,
		sampleLongRunningSQL, thresholds.LongRunningCritAfter); err != nil {
		return OperationalSnapshot{}, fmt.Errorf("sample long-running critical: %w", err)
	}
	if err := s.scanCapabilityCounts(ctx, snapshot.SilentByCapability,
		sampleReconciliationSilentSQL, thresholds.SilentAfter); err != nil {
		return OperationalSnapshot{}, fmt.Errorf("sample reconciliation silence: %w", err)
	}
	return snapshot, nil
}

func (s *Store) scanCapabilityCounts(ctx context.Context, into map[string]int64, query string, after time.Duration) error {
	rows, err := s.pool.Query(ctx, query, after.Nanoseconds())
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var capability string
		var count int64
		if err := rows.Scan(&capability, &count); err != nil {
			return err
		}
		into[capability] = count
	}
	return rows.Err()
}

// PoolStats returns the current per-process pool counters for the sampler.
func (s *Store) PoolStats() PoolStatsSnapshot {
	stats := s.pool.Stat()
	return PoolStatsSnapshot{
		Acquired:   int64(stats.AcquiredConns()),
		Idle:       int64(stats.IdleConns()),
		Connecting: int64(stats.ConstructingConns()),
		MaxTotal:   int64(stats.MaxConns()),
	}
}
