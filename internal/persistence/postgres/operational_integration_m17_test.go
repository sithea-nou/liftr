// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sithea-nou/liftr/internal/persistence/postgres"
)

func mustExecM17(t *testing.T, pool *pgxpool.Pool, query string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), query, args...); err != nil {
		t.Fatalf("seed query failed: %v\n%s", err, query)
	}
}

// The sampler's Dead-count aggregate must ride the dedicated partial index:
// terminal outbox rows are immutable and retained forever, so a sequential
// scan would grow without bound. This pins the plan evidence behind the
// 000008 migration decision.
func TestOperationalSamplerQueriesRideIndexes(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}

	plans := map[string]string{
		"dead":    "SELECT count(*) FROM outbox_messages WHERE state = 'Dead'",
		"pending": "SELECT count(*), min(created_at) FROM outbox_messages WHERE state = 'Pending'",
		"expired": "SELECT count(*) FROM outbox_messages WHERE state = 'Leased' AND leased_until <= clock_timestamp()",
		"active":  "SELECT count(*) FROM operations WHERE state IN ('Pending','Running')",
	}
	for name, query := range plans {
		rows, err := pool.Query(ctx, "EXPLAIN (FORMAT TEXT) "+query)
		if err != nil {
			t.Fatal(err)
		}
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan.WriteString(line)
			plan.WriteString("\n")
		}
		rows.Close()
		text := plan.String()
		switch name {
		case "dead":
			if !strings.Contains(text, "outbox_dead") || strings.Contains(text, "Seq Scan on outbox_messages") {
				t.Fatalf("dead count plan does not use the partial index:\n%s", text)
			}
		case "pending":
			if !strings.Contains(text, "outbox_claimable") && !strings.Contains(text, "Index Only Scan") {
				t.Fatalf("pending depth plan does not use the claimable partial index:\n%s", text)
			}
		case "expired":
			if !strings.Contains(text, "outbox_expired_leases") {
				t.Fatalf("expired lease plan does not use the lease partial index:\n%s", text)
			}
		case "active":
			if !strings.Contains(text, "operations_one_active_per_resource") &&
				!strings.Contains(text, "Index Only Scan") && !strings.Contains(text, "Bitmap Index Scan") {
				t.Fatalf("active operation plan does not use an index:\n%s", text)
			}
		}
	}
}

// Seeded durable state maps onto honest diagnostics: long-running measures
// total runtime; reconciliation-silence measures ACTIVITY absence, never lack
// of progress. Correction 1 scenarios 1 and 2 pinned at the SQL boundary.
func TestOperationalSnapshotClassifiesStuckCandidates(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}

	nowNS := time.Now().UnixNano()
	ns := func(d time.Duration) int64 { return nowNS + d.Nanoseconds() }
	twoHoursAgo := ns(-2 * time.Hour)
	threeHoursAgo := ns(-3 * time.Hour)
	oneMinuteAgo := ns(-time.Minute)
	for _, resourceID := range []string{"res-1", "res-2", "res-3"} {
		mustExecM17(t, pool, `
			INSERT INTO resources (id, type_name, type_version, owner_kind, owner_id, generation,
				spec_codec_version, spec, record_version, created_at_ns, updated_at_ns)
			VALUES ($1, 'PostgreSQLDatabase', 'v1', 'team', 'platform', 1, 1, '{}'::jsonb, 1,
				$2 - 86400000000000, $2 - 86400000000000)`, resourceID, nowNS)
		mustExecM17(t, pool, `
			INSERT INTO resource_statuses (resource_id, state, observed_generation, updated_at_ns)
			VALUES ($1, 'Ready', 1, $2 - 86400000000000)`, resourceID, nowNS)
		mustExecM17(t, pool, `
			INSERT INTO provisioner_bindings (resource_id, provisioner_ref) VALUES ($1, 'pulumi')`, resourceID)
	}

	// Scenario 1: long-running AND actively reconciled — fresh Liftr-side
	// observation activity one minute ago on an operation started 2h ago.
	mustExecM17(t, pool, `
		INSERT INTO operations (id, resource_id, capability, target_generation, state, phase,
			requested_at_ns, started_at_ns, phase_changed_at_ns, record_version)
		VALUES ('op-active-fresh', 'res-1', 'create', 1, 'Running', 'Applying',
			$1, $2, $3, 1)`, twoHoursAgo, twoHoursAgo+1000, twoHoursAgo+2000)
	mustExecM17(t, pool, `
		INSERT INTO provisioning_executions (operation_id, resource_id, provisioner_ref, resource_type_name,
			resource_type_version, capability, target_generation, spec_codec_version, submitted_spec, state,
			correlation_status, next_observation_sequence, current_attempt_number, record_version,
			last_observed_at_ns)
		VALUES ('op-active-fresh', 'res-1', 'pulumi', 'PostgreSQLDatabase', 'v1', 'create', 1, 1, '{}'::jsonb,
			'Unknown', 'Unknown', 1, 1, 1, $1)`, oneMinuteAgo)

	// Scenario 2: long-running AND silent — no execution at all, no phase
	// activity for three hours.
	mustExecM17(t, pool, `
		INSERT INTO operations (id, resource_id, capability, target_generation, state, phase,
			requested_at_ns, started_at_ns, phase_changed_at_ns, record_version)
		VALUES ('op-active-silent', 'res-2', 'update', 2, 'Pending', 'Requested',
			$1, NULL, $2, 1)`, threeHoursAgo, threeHoursAgo)

	// Young healthy active operation: neither diagnostic fires.
	mustExecM17(t, pool, `
		INSERT INTO operations (id, resource_id, capability, target_generation, state, phase,
			requested_at_ns, started_at_ns, phase_changed_at_ns, record_version)
		VALUES ('op-active-young', 'res-3', 'delete', 1, 'Running', 'Destroying',
			$1, $2, $3, 1)`, ns(-time.Minute), ns(-50*time.Second), ns(-40*time.Second))

	thresholds := postgres.DiagnosticThresholds{
		LongRunningWarnAfter: time.Hour,
		LongRunningCritAfter: 2 * time.Hour,
		SilentAfter:          15 * time.Minute,
	}
	snapshot, err := store.SnapshotOperationalState(ctx, thresholds)
	if err != nil {
		t.Fatal(err)
	}

	if snapshot.ActiveOperations != 3 {
		t.Fatalf("active=%d, want 3", snapshot.ActiveOperations)
	}
	if got := snapshot.LongRunningWarningByCapability; got["create"] != 1 || got["update"] != 1 || got["delete"] != 0 {
		t.Fatalf("long-running warning=%+v, want create=1 update=1 delete=0", got)
	}
	if got := snapshot.LongRunningCriticalByCapability; got["create"] != 1 || got["update"] != 1 {
		t.Fatalf("long-running critical=%+v, want create=1 update=1", got)
	}
	if got := snapshot.SilentByCapability; got["create"] != 0 {
		t.Fatalf("actively observed operation counted reconciliation-silent: %+v", got)
	}
	if got := snapshot.SilentByCapability; got["update"] != 1 {
		t.Fatalf("silent operation missing from diagnostics: %+v", got)
	}
	if snapshot.OutboxPendingDepth != 0 {
		t.Fatalf("unexpected pending outbox depth %d", snapshot.OutboxPendingDepth)
	}
	if snapshot.ActiveOldestAgeSeconds < 10700 {
		t.Fatalf("oldest active age=%f, want roughly 2h+", snapshot.ActiveOldestAgeSeconds)
	}

	// A large sampled backlog completes inside the sampler budget because
	// every aggregate rides a partial index rather than scanning history.
	mustExecM17(t, pool, `
		INSERT INTO operations (id, resource_id, capability, target_generation, state, phase,
			requested_at_ns, started_at_ns, phase_changed_at_ns, completed_at_ns, record_version)
		SELECT 'op-backlog-'||g, 'res-1', 'create', 1, 'Succeeded', 'Applying',
		       $1 - 86400000000000, $1 - 86400000000000, $1 - 86400000000000,
		       $1 - 86390000000000, 1
		FROM generate_series(1, 20000) g`, nowNS)
	mustExecM17(t, pool, `
		INSERT INTO outbox_messages (id, kind, operation_id, dedupe_key, payload_version, payload, state, available_at, created_at)
		SELECT 'backlog-'||g, 'Observe', 'op-backlog-'||g, 'backlog-dedupe-'||g, 1, '{}'::jsonb, 'Pending',
		       clock_timestamp(), clock_timestamp() - (g || ' seconds')::interval
		FROM generate_series(1, 20000) g`)
	started := time.Now()
	sampleCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	snapshot, err = store.SnapshotOperationalState(sampleCtx, thresholds)
	if err != nil {
		t.Fatalf("sampling a 20k backlog failed: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Fatalf("sampler monopolized the budget: %v", elapsed)
	}
	if snapshot.OutboxPendingDepth != 20000 {
		t.Fatalf("pending depth=%d, want 20000", snapshot.OutboxPendingDepth)
	}
	if snapshot.OutboxPendingOldestAgeSeconds < 19990 {
		t.Fatalf("oldest pending age=%f, want ~20000", snapshot.OutboxPendingOldestAgeSeconds)
	}
	if snapshot.OutboxDead != 0 {
		t.Fatalf("dead count=%d, want 0 on fresh schema", snapshot.OutboxDead)
	}
}
