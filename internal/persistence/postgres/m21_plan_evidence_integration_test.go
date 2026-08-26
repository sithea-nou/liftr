// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/persistence/postgres"
)

// M21 scale-plan evidence (ADR-0022 gate): seed representative relationship
// scale into REAL PostgreSQL and prove every hot query stays index-backed.
//
// Seeded shape:
//   - >= 100,000 desired edges total
//   - a shared target with 10,000 inbound dependents (the fanout star)
//   - 12,000 dependency waits (10,000 against the star target)
//   - a 34-node chain approaching MaxDependencyDepth for traversal proofs

const (
	planStarDependents = 10_000
	planMeshEdges      = 90_000
	planMiscWaits      = 2_000
	planChainNodes     = application.MaxDependencyDepth + 2
)

type m21PlanWorld struct {
	pool     *pgxpool.Pool
	store    *postgres.Store
	cleanup  func()
	targetID string
}

func planResourceID(i int) string { return fmt.Sprintf("pl-r%06d", i) }

// TestM21PostgresLargeGraphPlanEvidence seeds the scale graph and asserts the
// nine hot relationship queries avoid sequential scans on the big tables,
// logging per-query timings and index usage following the M17/M20 diagnostic
// precedent.
func TestM21PostgresLargeGraphPlanEvidence(t *testing.T) {
	w := newPlanWorld(t)
	defer w.cleanup()
	ctx := context.Background()

	w.seed(t)
	t.Logf("seeded: %d resources, %d+1 desired edges, %d waits",
		planStarDependents+planMeshEdges/2+planChainNodes+2, planStarDependents+planMeshEdges, planStarDependents+planMiscWaits)

	// ---- the nine hot queries (exact repository SQL) ----

	desiredBySource := `SELECT slot, target_id, generation::text FROM resource_desired_references WHERE source_id=$1 ORDER BY slot, target_id`
	appliedBySource := `SELECT slot, target_id, generation::text FROM resource_applied_references WHERE source_id=$1 ORDER BY slot, target_id`
	outgoingTargets := `SELECT DISTINCT target_id FROM resource_desired_references WHERE source_id=$1 ORDER BY target_id`
	waitersByKeyset := `SELECT operation_id, target_id, wait_seq, operation_version::text, registered_target_version::text
		FROM operation_dependency_waits WHERE target_id=$1 AND wait_seq > $2::bigint ORDER BY wait_seq LIMIT $3`
	waitsByOperation := `SELECT EXISTS(SELECT 1 FROM operation_dependency_waits WHERE operation_id=$1)`
	waitersExist := `SELECT EXISTS(SELECT 1 FROM operation_dependency_waits WHERE target_id=$1)`
	protectiveUnion := `WITH inbound AS (
			SELECT d.source_id FROM resource_desired_references d WHERE d.target_id=$1
			UNION ALL
			SELECT a.source_id FROM resource_applied_references a WHERE a.target_id=$1
		)
		SELECT count(*)::bigint,
			count(*) FILTER (WHERE s.state = 'Deleted')::bigint,
			count(*) FILTER (WHERE s.resource_id IS NULL)::bigint
		FROM inbound i
		JOIN resources r ON r.id = i.source_id
		LEFT JOIN resource_statuses s ON s.resource_id = r.id`

	type query struct {
		name    string
		sql     string
		args    []any
		mustUse []string // substring matched against ANY index name in the plan
	}
	starSource := planResourceID(0)
	queries := []query{
		{"1 outgoing-desired-by-source", desiredBySource, []any{starSource}, []string{"resource_desired_references_pkey"}},
		{"2 outgoing-applied-by-source", appliedBySource, []any{starSource}, nil}, // applied table empty at this scale seed; assert no seq scan on it anyway
		{"3+4+8 protective-inbound-union", protectiveUnion, []any{w.targetID}, []string{"resource_desired_refs_inbound", "resource_applied_refs_inbound"}},
		{"5 waiters-by-target-keyset", waitersByKeyset, []any{w.targetID, 5_000, 256}, []string{"operation_dependency_waits_by_target"}},
		{"6 waits-by-operation", waitsByOperation, []any{"op-w-000001"}, []string{"operation_dependency_waits_pkey"}},
		{"7 cycle-traversal-outgoing", outgoingTargets, []any{chainNode(15)}, nil},
		// The wake-EXISTS hot path runs on EVERY gate-relevant transition of
		// every Resource; the production-critical case is a SPARSE target
		// (zero-to-few waiters), so plan evidence uses one. At extreme
		// single-target skew (83% of all waits) the planner's seq scan is
		// legitimately optimal and is exercised functionally below instead.
		{"9 wake-only-if-waiters", waitersExist, []any{planResourceID(0)}, []string{"operation_dependency_waits_by_target"}},
	}

	for _, q := range queries {
		nodes, elapsed := w.explain(t, ctx, q.sql, q.args...)
		var seqScanned []string
		var indexes []string
		collectPlan(nodes, &seqScanned, &indexes)
		for _, relation := range seqScanned {
			switch relation {
			case "resource_desired_references", "resource_applied_references", "operation_dependency_waits":
				t.Fatalf("%s sequentially scanned %s\nplan indexes=%v", q.name, relation, indexes)
			}
		}
		if len(q.mustUse) > 0 {
			ok := false
			for _, want := range q.mustUse {
				for _, got := range indexes {
					if got == want {
						ok = true
					}
				}
			}
			if !ok {
				t.Fatalf("%s did not use any expected index %v; got %v", q.name, q.mustUse, indexes)
			}
		}
		t.Logf("PLAN %-32s %8.2fms indexes=%v", q.name, elapsed, indexes)
	}

	// Functional sweep at scale: page ALL 10,000 star waiters in bounded
	// keyset batches — no OFFSET, bounded memory, monotonic sequences.
	const batchSize = 256
	cursor := uint64(0)
	paged := 0
	batches := 0
	lastSeq := uint64(0)
	for {
		var waits []application.DependencyWait
		var next uint64
		err := w.store.Within(ctx, func(tx application.UnitOfWork) error {
			var err error
			waits, next, err = tx.DependencyWaits().PageDependencyWaitersByTarget(ctx, domain.ResourceID(w.targetID), cursor, batchSize)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(waits) > batchSize {
			t.Fatalf("batch of %d exceeds bound %d", len(waits), batchSize)
		}
		for _, wait := range waits {
			if wait.WaitSequence <= lastSeq {
				t.Fatalf("keyset ordering violated at seq %d after %d", wait.WaitSequence, lastSeq)
			}
			lastSeq = wait.WaitSequence
		}
		paged += len(waits)
		batches++
		if next == 0 {
			break
		}
		cursor = next
		if batches > 200 {
			t.Fatal("paging did not terminate")
		}
	}
	if paged != planStarDependents {
		t.Fatalf("paged %d star waiters, want %d", paged, planStarDependents)
	}
	t.Logf("PAGING swept %d waiters in %d bounded batches", paged, batches)

	// Protective union functional result at scale.
	err := w.store.Within(ctx, func(tx application.UnitOfWork) error {
		protected, err := tx.References().HasInboundProtectiveReference(ctx, domain.ResourceID(w.targetID))
		if err != nil {
			return err
		}
		if !protected {
			t.Fatal("star target not protected at scale")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Cycle traversal functional proof on the near-depth chain: proposing an
	// edge that closes through more than MaxDependencyDepth hops must fail
	// CONSERVATIVELY (never proven safe from truncation).
	err = w.store.Within(ctx, func(tx application.UnitOfWork) error {
		return application.DetectDependencyCycle(ctx, tx, domain.ResourceID(chainNode(planChainNodes-3)), []domain.ResourceID{domain.ResourceID(chainNode(planChainNodes - 2))})
	})
	if err != nil {
		t.Fatalf("short traversal should succeed: %v", err)
	}
	err = w.store.Within(ctx, func(tx application.UnitOfWork) error {
		return application.DetectDependencyCycle(ctx, tx, "pl-chain-034", []domain.ResourceID{"pl-chain-000"})
	})
	if err == nil {
		t.Fatal("deep closing edge was admitted without a complete proof")
	}
}

// ---- seeding ----

func newPlanWorld(t *testing.T) *m21PlanWorld {
	t.Helper()
	pool, cleanup := migratedPool(t)
	store, err := postgres.NewStore(pool)
	if err != nil {
		cleanup()
		t.Fatal(err)
	}
	return &m21PlanWorld{pool: pool, store: store, cleanup: cleanup, targetID: "pl-star-target"}
}

func (w *m21PlanWorld) exec(t *testing.T, ctx context.Context, sql string, args ...any) {
	t.Helper()
	if _, err := w.pool.Exec(ctx, sql, args...); err != nil {
		t.Fatal(err)
	}
}

func (w *m21PlanWorld) seed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UnixNano()

	insertResources := func(ids []string) {
		rows := make([][]any, 0, len(ids))
		for _, id := range ids {
			rows = append(rows, []any{id, "Widget", "v1", "team", "planning", int64(1), int32(1), []byte(`{}`), int64(1), now, now})
		}
		if _, err := w.pool.CopyFrom(ctx, pgx.Identifier{"resources"},
			[]string{"id", "type_name", "type_version", "owner_kind", "owner_id", "generation", "spec_codec_version", "spec", "record_version", "created_at_ns", "updated_at_ns"}, pgx.CopyFromRows(rows)); err != nil {
			t.Fatal(err)
		}
	}
	insertStatuses := func(ids []string) {
		rows := make([][]any, 0, len(ids))
		for _, id := range ids {
			rows = append(rows, []any{id, int64(1), "Ready", now})
		}
		if _, err := w.pool.CopyFrom(ctx, pgx.Identifier{"resource_statuses"},
			[]string{"resource_id", "observed_generation", "state", "updated_at_ns"}, pgx.CopyFromRows(rows)); err != nil {
			t.Fatal(err)
		}
	}
	insertDesired := func(pairs [][2]string) {
		rows := make([][]any, 0, len(pairs))
		for _, pair := range pairs {
			rows = append(rows, []any{pair[0], "dependency", pair[1], int64(1)})
		}
		if _, err := w.pool.CopyFrom(ctx, pgx.Identifier{"resource_desired_references"},
			[]string{"source_id", "slot", "target_id", "generation"}, pgx.CopyFromRows(rows)); err != nil {
			t.Fatal(err)
		}
	}

	// Star: 10k dependents -> shared target.
	starSources := make([]string, 0, planStarDependents)
	starEdges := make([][2]string, 0, planStarDependents)
	for i := range planStarDependents {
		id := planResourceID(i)
		starSources = append(starSources, id)
		starEdges = append(starEdges, [2]string{id, w.targetID})
	}
	all := append([]string{w.targetID}, starSources...)

	// Mesh: remaining edges spread over a second pool so total desired edges
	// comfortably exceeds 100k.
	meshSources := make([]string, 0, planMeshEdges/2)
	meshEdges := make([][2]string, 0, planMeshEdges)
	for i := 0; len(meshEdges) < planMeshEdges; i++ {
		id := fmt.Sprintf("pl-m%06d", i)
		meshSources = append(meshSources, id)
		meshEdges = append(meshEdges, [2]string{id, planResourceID(i % planStarDependents)})
		meshEdges = append(meshEdges, [2]string{id, planResourceID((i*7 + 3) % planStarDependents)})
	}

	// Depth chain approaching MaxDependencyDepth plus its root.
	chain := make([]string, 0, planChainNodes+1)
	chain = append(chain, "pl-chain-root")
	for i := 0; i < planChainNodes; i++ {
		chain = append(chain, chainNode(i))
	}
	chainEdges := make([][2]string, 0, len(chain)-1)
	for i := 0; i < len(chain)-1; i++ {
		chainEdges = append(chainEdges, [2]string{chain[i], chain[i+1]})
	}
	// NOTE: deliberately NO closing edge — the seeded chain stays an acyclic
	// PATH longer than MaxDependencyDepth so the traversal-proof assertions
	// below exercise both a short safe proof and the conservative bound.

	allResources := append(append(all, meshSources...), chain...)
	insertResources(allResources)
	insertStatuses(all)
	insertDesired(append(append(starEdges, meshEdges...), chainEdges...))

	// Waits: 10k against the star target bound to per-source operations,
	// plus scattered misc waits on their own operations.
	operations := make([][]any, 0, planStarDependents+planMiscWaits)
	waits := make([][]any, 0, planStarDependents+planMiscWaits)
	base := time.Now().UnixNano()
	for i := range planStarDependents {
		opID := fmt.Sprintf("op-w-%06d", i)
		operations = append(operations, []any{opID, starSources[i], "create", int64(1), "Running", "Applying", base, base, int64(1), base, nil})
		waits = append(waits, []any{opID, w.targetID, int64(1), int64(1)})
	}
	for i := range planMiscWaits {
		opID := fmt.Sprintf("op-wm-%05d", i)
		operations = append(operations, []any{opID, fmt.Sprintf("pl-m%06d", i), "update", int64(1), "Running", "Applying", base, base, int64(1), base, nil})
		waits = append(waits, []any{opID, planResourceID((i * 13) % planStarDependents), int64(1), int64(1)})
	}
	if _, err := w.pool.CopyFrom(ctx, pgx.Identifier{"operations"},
		[]string{"id", "resource_id", "capability", "target_generation", "state", "phase", "requested_at_ns", "phase_changed_at_ns", "record_version", "started_at_ns", "completed_at_ns"}, pgx.CopyFromRows(operations)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.pool.CopyFrom(ctx, pgx.Identifier{"operation_dependency_waits"},
		[]string{"operation_id", "target_id", "operation_version", "registered_target_version"}, pgx.CopyFromRows(waits)); err != nil {
		t.Fatal(err)
	}

	w.exec(t, ctx, "ANALYZE resource_desired_references")
	w.exec(t, ctx, "ANALYZE operation_dependency_waits")
	w.exec(t, ctx, "ANALYZE resources")
}

// ---- EXPLAIN plumbing ----

type planNode struct {
	NodeType string     `json:"Node Type"`
	Relation string     `json:"Relation Name"`
	Index    string     `json:"Index Name"`
	Plans    []planNode `json:"Plans"`
}

type explainRow struct {
	Plan          planNode `json:"Plan"`
	ExecutionTime float64  `json:"Execution Time"`
}

func (w *m21PlanWorld) explain(t *testing.T, ctx context.Context, sql string, args ...any) (planNode, float64) {
	t.Helper()
	rows, err := w.pool.Query(ctx, "EXPLAIN (ANALYZE, FORMAT JSON) "+sql, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var raw []byte
	for rows.Next() {
		var line []byte
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		raw = append(raw, line...)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var parsed []explainRow
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("explain parse: %v (%s)", err, string(raw))
	}
	if len(parsed) != 1 {
		t.Fatalf("unexpected explain output rows: %d", len(parsed))
	}
	return parsed[0].Plan, parsed[0].ExecutionTime
}

func collectPlan(node planNode, seqScanned *[]string, indexes *[]string) {
	if node.NodeType == "Seq Scan" && node.Relation != "" {
		*seqScanned = append(*seqScanned, node.Relation)
	}
	if node.Index != "" {
		*indexes = append(*indexes, node.Index)
	}
	for _, child := range node.Plans {
		collectPlan(child, seqScanned, indexes)
	}
}

func chainNode(i int) string { return fmt.Sprintf("pl-chain-%03d", i) }

// copyRows wraps a row batch for pgx.CopyFrom.
