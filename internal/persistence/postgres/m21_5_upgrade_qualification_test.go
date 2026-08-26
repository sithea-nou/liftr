// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sithea-nou/liftr/internal/application"
	applicationfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/lifecycle"
	"github.com/sithea-nou/liftr/internal/persistence/postgres"
	"github.com/sithea-nou/liftr/internal/provisioning"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
	"github.com/sithea-nou/liftr/internal/worker"
)

// TestM21_5FreshInstallAppliesEveryMigrationAndSmokesLifecycle qualifies the
// fresh-install path: an empty PostgreSQL 17 schema reaches the current
// migration head, the composed control plane starts against it, and one full
// lifecycle converges through the durable worker.
func TestM21_5FreshInstallAppliesEveryMigrationAndSmokesLifecycle(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()

	if err := postgres.Migrate(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	assertMigrationsApplied(t, pool, 11)

	if err := postgres.VerifySchema(context.Background(), pool); err != nil {
		t.Fatalf("fresh schema failed verification: %v", err)
	}

	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	world := newUpgradeWorld(t, pool, store, provisioningfake.New(provisioningfake.ModeSynchronous))
	defer world.close()

	world.create(t, "fresh-resource", nil)
	world.drain(t)
	if state := world.state(t, "fresh-resource"); state != domain.ResourceStateReady {
		t.Fatalf("fresh install state = %s, want Ready", state)
	}
	world.remove(t, "fresh-resource", "fresh-delete")
	world.drain(t)
	if state := world.state(t, "fresh-resource"); state != domain.ResourceStateDeleted {
		t.Fatalf("fresh install delete state = %s, want Deleted", state)
	}
}

// TestM21_5UpgradeFromPreRelationshipSchemaPreservesEverything qualifies the
// upgrade path: a database carrying representative M1–M20 durable data at
// migration 000010 receives migration 000011 and every historical invariant
// survives without synthetic relationship state.
func TestM21_5UpgradeFromPreRelationshipSchemaPreservesEverything(t *testing.T) {
	databaseURL := os.Getenv("LIFTR_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("LIFTR_TEST_DATABASE_URL is not set")
	}

	schema, adminCleanup := createIsolatedSchema(t, databaseURL)

	// ---- Pre-M21 world: migrations 000001..000010 plus meaningful history.
	applyManualMigrations(t, databaseURL, schema, 10)
	pool, closePool := openSchemaPool(t, databaseURL, schema)
	seedPreRelationshipHistory(t, pool)

	preCounts := tableCounts(t, pool)

	// ---- Upgrade in place: the current binary applies exactly 000011.
	if err := postgres.Migrate(context.Background(), pool); err != nil {
		closePool()
		adminCleanup()
		t.Fatalf("upgrade migration failed: %v", err)
	}
	assertMigrationsApplied(t, pool, 11)
	if err := postgres.VerifySchema(context.Background(), pool); err != nil {
		t.Fatalf("upgraded schema failed verification: %v", err)
	}

	postCounts := tableCounts(t, pool)
	for table, before := range preCounts {
		if postCounts[table] != before {
			t.Errorf("table %s changed during migration: %d -> %d", table, before, postCounts[table])
		}
	}
	assertSeededRowsPreserved(t, pool)

	for _, table := range []string{"resource_desired_references", "resource_applied_references", "operation_dependency_waits"} {
		count := singleCount(t, pool, "SELECT count(*) FROM "+table)
		if count != 0 {
			t.Errorf("migration synthesized %d rows into %s; upgrade must never invent relationships", count, table)
		}
	}

	// ---- Restart: a freshly opened pool re-verifies and serves old data.
	closePool()
	restarted, closeRestarted := openSchemaPool(t, databaseURL, schema)
	defer func() {
		closeRestarted()
		adminCleanup()
	}()
	if err := postgres.VerifySchema(context.Background(), restarted); err != nil {
		t.Fatalf("restarted server schema verification failed: %v", err)
	}
	store, err := postgres.NewStore(restarted)
	if err != nil {
		t.Fatal(err)
	}
	assertLegacyResourceReadable(t, store)

	gatedProvider := newUpgradeGateProvider()
	world := newUpgradeWorld(t, restarted, store, gatedProvider)
	defer world.close()

	// ---- Old outbox rows remain valid: terminal rows stay untouched while
	// the worker keeps processing fresh work in the upgraded schema.
	world.update(t, "legacy-ready", nil, true, "post-upgrade-grow")
	world.drain(t)
	if state := world.state(t, "legacy-ready"); state != domain.ResourceStateReady {
		t.Fatalf("legacy-ready post-upgrade update state = %s, want Ready", state)
	}
	if generation := world.generation(t, "legacy-ready"); generation != 2 {
		t.Fatalf("legacy-ready generation = %d, want 2 after admitted update", generation)
	}

	// ---- The WakeDependents vocabulary introduced by 000011 is processable
	// immediately after the upgrade.
	insertWakeDependentsWork(t, restarted, "legacy-ready")
	world.drain(t)
	wakeStates := rawStringColumn(t, restarted,
		"SELECT state FROM outbox_messages WHERE kind = 'WakeDependents'")
	if len(wakeStates) != 1 || wakeStates[0] != "Completed" {
		t.Fatalf("WakeDependents rows after drain = %v, want [Completed]", wakeStates)
	}

	// ---- New reference-bearing Resources work after the upgrade: the whole
	// M21 dependency lifecycle runs on top of migrated history.
	world.createWithGate(t, "upgrade-anchor")
	world.create(t, "upgrade-dependent", map[string][]string{"dependency": {"upgrade-anchor"}})
	// Bounded pumping: while the gate holds the anchor nonterminal its
	// observation loop legitimately keeps rescheduling, so quiescence is
	// not reachable yet.
	world.pumpSteps(t, 64)

	if state := world.state(t, "upgrade-dependent"); state != domain.ResourceStatePending {
		t.Fatalf("dependent state = %s, want Pending while gated", state)
	}
	if status, reason, found := world.conditionOf(t, "upgrade-dependent"); !found || status != domain.ConditionStatusFalse || reason != lifecycle.ReasonWaitingForDependencies {
		t.Fatalf("gated condition = (%s, %s, %t)", status, reason, found)
	}
	if submissions := gatedProvider.submissionCount(world.lastOperation(t, "upgrade-dependent")); submissions != 0 {
		t.Fatalf("dependent submitted %d times while dependency-gated, want 0", submissions)
	}
	if waits := singleCount(t, restarted, "SELECT count(*) FROM operation_dependency_waits WHERE target_id = 'upgrade-anchor'"); waits != 1 {
		t.Fatalf("wait rows for upgrade-anchor = %d, want 1", waits)
	}

	gatedProvider.release()
	world.drain(t)
	if state := world.state(t, "upgrade-dependent"); state != domain.ResourceStateReady {
		t.Fatalf("dependent state = %s, want Ready after wake", state)
	}
	desired := world.edges(t, false, "upgrade-dependent")
	applied := world.edges(t, true, "upgrade-dependent")
	if len(desired) != 1 || len(applied) != 1 || desired[0].TargetID != "upgrade-anchor" || applied[0].TargetID != "upgrade-anchor" {
		t.Fatalf("post-upgrade edges mismatch: desired=%+v applied=%+v", desired, applied)
	}

	// ---- Deletion protection holds for migrated-and-new graphs alike.
	if err := world.removeExpectingUseError(t, "upgrade-anchor", "blocked-anchor"); !errors.Is(err, application.ErrResourceInUse) {
		t.Fatalf("delete of protected anchor returned %v, want RESOURCE_IN_USE", err)
	}

	// ---- Old zero-reference Resources delete normally after the upgrade.
	if err := world.remove(t, "legacy-failed", "post-upgrade-delete"); err != nil {
		t.Fatalf("delete of unprotected legacy resource failed: %v", err)
	}
	world.drain(t)
	if state := world.state(t, "legacy-failed"); state != domain.ResourceStateDeleted {
		t.Fatalf("legacy-failed delete state = %s, want Deleted", state)
	}

	// ---- Source deletion releases references only after Deleted.
	if err := world.remove(t, "upgrade-dependent", "release-dependent"); err != nil {
		t.Fatalf("dependent delete admission failed: %v", err)
	}
	world.drain(t)
	if state := world.state(t, "upgrade-dependent"); state != domain.ResourceStateDeleted {
		t.Fatalf("dependent state = %s, want Deleted", state)
	}
	if remaining := singleCount(t, restarted,
		"SELECT count(*) FROM resource_applied_references WHERE source_id = 'upgrade-dependent'"); remaining != 0 {
		t.Fatalf("applied edges survived source deletion: %d", remaining)
	}
	if err := world.remove(t, "upgrade-anchor", "release-anchor"); err != nil {
		t.Fatalf("anchor delete after dependent removal failed: %v", err)
	}
	world.drain(t)
	if state := world.state(t, "upgrade-anchor"); state != domain.ResourceStateDeleted {
		t.Fatalf("anchor state = %s, want Deleted", state)
	}

	// ---- Second restart: tombstones and full history remain readable.
	closeRestarted()
	finalPool, closeFinal := openSchemaPool(t, databaseURL, schema)
	defer closeFinal()
	if err := postgres.VerifySchema(context.Background(), finalPool); err != nil {
		t.Fatalf("final restart verification failed: %v", err)
	}
	finalStore, err := postgres.NewStore(finalPool)
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]domain.ResourceState{
		"legacy-ready":      domain.ResourceStateReady,
		"legacy-failed":     domain.ResourceStateDeleted,
		"legacy-tombstone":  domain.ResourceStateDeleted,
		"upgrade-dependent": domain.ResourceStateDeleted,
		"upgrade-anchor":    domain.ResourceStateDeleted,
	} {
		if state := upgradeResourceState(t, finalStore, id); state != want {
			t.Errorf("%s state after second restart = %s, want %s", id, state, want)
		}
	}
	assertTerminalHistoryPreserved(t, finalPool)
}

// ---- Schema-level helpers -------------------------------------------------

func createIsolatedSchema(t *testing.T, databaseURL string) (string, func()) {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("liftr_test_%d_%d", time.Now().UnixNano(), schemaSequence.Add(1))
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cleanup := func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	}
	return schema, cleanup
}

func openSchemaPool(t *testing.T, databaseURL, schema string) (*pgxpool.Pool, func()) {
	t.Helper()
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	return pool, pool.Close
}

// applyManualMigrations replays the embedded-equivalent migration files from
// disk up to the requested count and records them in liftr_schema_migrations
// with the same checksum discipline the real migrator enforces. The later
// postgres.Migrate call verifies this exact prefix, so any drift fails loudly.
func applyManualMigrations(t *testing.T, databaseURL, schema string, count int) {
	t.Helper()
	entries, err := filepath.Glob("migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < count {
		t.Fatalf("repository carries %d migrations, want at least %d", len(entries), count)
	}
	pool, closePool := openSchemaPool(t, databaseURL, schema)
	defer closePool()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS liftr_schema_migrations (
		version bigint PRIMARY KEY,
		name text NOT NULL,
		checksum text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT clock_timestamp()
	)`); err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries[:count] {
		base := filepath.Base(entry)
		parts := strings.SplitN(base, "_", 2)
		version, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		contents, err := os.ReadFile(entry)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(contents)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, string(contents)); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("manual application of %s failed: %v", base, err)
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO liftr_schema_migrations(version, name, checksum) VALUES ($1, $2, $3)",
			version, base, hex.EncodeToString(digest[:])); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func assertMigrationsApplied(t *testing.T, pool *pgxpool.Pool, want int64) {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		"SELECT version FROM liftr_schema_migrations ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var versions []int64
	for rows.Next() {
		var version int64
		if err := rows.Scan(&version); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if int64(len(versions)) != want {
		t.Fatalf("applied migrations = %v, want %d rows", versions, want)
	}
	for i, version := range versions {
		if version != int64(i+1) {
			t.Fatalf("applied migrations = %v, want contiguous 1..%d", versions, want)
		}
	}
}

// ---- Seeding the pre-M21 world ---------------------------------------------

// specEnvelope encodes developer values through the exact durable spec codec
// shape (spec_codec.go) so seeded rows decode identically to admitted ones.
func specEnvelope(t *testing.T, values map[string]any) []byte {
	t.Helper()
	var encode func(value any) (map[string]any, error)
	encode = func(value any) (map[string]any, error) {
		switch value := value.(type) {
		case string:
			return map[string]any{"kind": "string", "scalar": value}, nil
		case bool:
			return map[string]any{"kind": "bool", "scalar": strconv.FormatBool(value)}, nil
		case int:
			return map[string]any{"kind": "int", "scalar": strconv.Itoa(value)}, nil
		case uint64:
			return map[string]any{"kind": "uint64", "scalar": strconv.FormatUint(value, 10)}, nil
		case map[string]any:
			object := make(map[string]any, len(value))
			for key, item := range value {
				encoded, err := encode(item)
				if err != nil {
					return nil, err
				}
				object[key] = encoded
			}
			return map[string]any{"kind": "object", "object": object}, nil
		default:
			return nil, fmt.Errorf("unsupported seed spec value %T", value)
		}
	}
	node, err := encode(values)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func legacySpecJSON(t *testing.T, size uint64, name string) []byte {
	t.Helper()
	return specEnvelope(t, map[string]any{"size": size, "name": name})
}

// seedPreRelationshipHistory inserts representative durable M1–M20 data at
// schema version 000010: resources across statuses, conditions, operation
// history including a retry linkage shape, events, executions, submission
// attempts, terminal outbox history, developer idempotency, provisioner
// bindings, published outputs, OpenTofu evidence/state bindings (M19), and
// operator audit/idempotency rows (M20). M18 policy provenance has no durable
// table by design (policy is a startup overlay; quotas derive live from
// resources), so nothing synthetic is added for it.
func seedPreRelationshipHistory(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatalf("seed failed: %v\nSQL: %s", err, query)
		}
	}
	const ns int64 = 1755300000000000000 // fixed epoch nanoseconds for determinism
	refPulumi := "legacy-pulumi-ref"

	// Resources across the full status spectrum.
	exec(`INSERT INTO resources (id, type_name, type_version, owner_kind, owner_id, generation, spec_codec_version, spec, record_version, created_at_ns, updated_at_ns) VALUES
		('legacy-ready', 'FakeResource', 'v1', 'team', 'platform', 1, 1, $4, 4, $1, $1),
		('legacy-failed', 'FakeResource', 'v1', 'team', 'platform', 1, 1, $5, 2, $1, $2),
		('legacy-tombstone', 'FakeResource', 'v1', 'team', 'platform', 2, 1, $6, 6, $1, $3)`,
		ns, ns+1000, ns+9000, legacySpecJSON(t, 3, "legacy"), legacySpecJSON(t, 1, "doomed"), legacySpecJSON(t, 2, "gone"))

	exec(`INSERT INTO resource_statuses (resource_id, observed_generation, state, updated_at_ns) VALUES
		('legacy-ready', 1, 'Ready', $1),
		('legacy-failed', 1, 'Failed', $2),
		('legacy-tombstone', 2, 'Deleted', $3)`, ns+8000, ns+2000, ns+9500)

	exec(`INSERT INTO resource_conditions (resource_id, type, status, reason, message, observed_generation, last_transition_at_ns) VALUES
		('legacy-ready', 'Ready', 'True', 'ProvisionerReportedReady', 'legacy convergence evidence', 1, $1),
		('legacy-failed', 'Ready', 'False', 'ExecutionFailed', 'legacy failure evidence', 1, $2),
		('legacy-tombstone', 'Ready', 'False', 'ResourceDeleted', 'legacy tombstone', 2, $3)`,
		ns+8000, ns+2000, ns+9500)

	exec(`INSERT INTO provisioner_bindings (resource_id, provisioner_ref) VALUES
		('legacy-ready', $1), ('legacy-failed', $1), ('legacy-tombstone', $1)`, refPulumi)

	// Operation history: succeeded create, failed create, succeeded
	// create+delete pair for the tombstone.
	exec(`INSERT INTO operations (id, resource_id, capability, target_generation, state, phase, requested_at_ns, started_at_ns, phase_changed_at_ns, completed_at_ns, failure_reason, failure_message, record_version) VALUES
		('op-lr-create', 'legacy-ready', 'create', 1, 'Succeeded', 'Applying', $1, $1, $1, $2, NULL, NULL, 3),
		('op-lf-create', 'legacy-failed', 'create', 1, 'Failed', 'Applying', $1, $1, $1, $3, 'ExecutionFailed', 'legacy execution failure', 3),
		('op-lt-create', 'legacy-tombstone', 'create', 1, 'Succeeded', 'Applying', $1, $1, $1, $2, NULL, NULL, 3),
		('op-lt-delete', 'legacy-tombstone', 'delete', 2, 'Succeeded', 'Destroying', $4, $4, $4, $5, NULL, NULL, 3)`,
		ns, ns+7000, ns+3000, ns+8500, ns+9000)

	exec(`INSERT INTO events (id, resource_id, operation_id, generation, type, reason, message, occurred_at_ns) VALUES
		('evt-lr-create', 'legacy-ready', 'op-lr-create', 1, 'OperationCompleted', 'Provisioned', 'legacy create completed', $1),
		('evt-lf-create', 'legacy-failed', 'op-lf-create', 1, 'OperationFailed', 'ExecutionFailed', 'legacy create failed', $2),
		('evt-lt-delete', 'legacy-tombstone', 'op-lt-delete', 2, 'OperationCompleted', 'Destroyed', 'legacy destroy completed', $3)`,
		ns+7000, ns+3000, ns+9000)

	// Execution + attempt + dispatch evidence for each operation.
	exec(`INSERT INTO provisioning_executions (operation_id, resource_id, provisioner_ref, resource_type_name, resource_type_version, capability, target_generation, spec_codec_version, submitted_spec, state, handle, acceptance_confirmed, correlation_status, submission, latest_observation, last_observed_at_ns, current_attempt_number, next_observation_sequence, record_version, output_mapping_ref, output_resolution) VALUES
		('op-lr-create', 'legacy-ready', $1, 'FakeResource', 'v1', 'create', 1, 1, $5, 'Succeeded', 'legacy-handle-lr', true, 'Found', '{"correlation":"Found"}', '{"execution":{"state":"Succeeded"}}', $2, 1, 2, 5, '', 'Published'),
		('op-lf-create', 'legacy-failed', $1, 'FakeResource', 'v1', 'create', 1, 1, $6, 'Failed', 'legacy-handle-lf', true, 'Found', '{"correlation":"Found"}', '{"execution":{"state":"Failed","failure":{"kind":"ExecutionFailed","reason":"ExecutionFailed"}}}', $3, 1, 2, 5, '', 'None'),
		('op-lt-create', 'legacy-tombstone', $1, 'FakeResource', 'v1', 'create', 1, 1, $7, 'Succeeded', 'legacy-handle-lt', true, 'Found', '{"correlation":"Found"}', '{"execution":{"state":"Succeeded"}}', $2, 1, 2, 5, '', 'None'),
		('op-lt-delete', 'legacy-tombstone', $1, 'FakeResource', 'v1', 'delete', 2, 1, $7, 'Succeeded', 'legacy-handle-ltd', true, 'Found', '{"correlation":"Found"}', '{"execution":{"state":"Succeeded"}}', $4, 1, 2, 5, '', 'None')`,
		refPulumi, ns+7000, ns+3000, ns+9000, legacySpecJSON(t, 3, "legacy"), legacySpecJSON(t, 1, "doomed"), legacySpecJSON(t, 2, "gone"))

	exec(`INSERT INTO provisioning_submission_attempts (operation_id, attempt_number, state, dispatch_message_id, claimed_at, resolved_at) VALUES
		('op-lr-create', 1, 'Accepted', 'ob-dispatch-lr', to_timestamp($1 / 1e9), to_timestamp($2 / 1e9)),
		('op-lf-create', 1, 'Accepted', 'disp-lf-create', to_timestamp($1 / 1e9), to_timestamp($3 / 1e9)),
		('op-lt-create', 1, 'Accepted', 'disp-lt-create', to_timestamp($1 / 1e9), to_timestamp($2 / 1e9)),
		('op-lt-delete', 1, 'Accepted', 'disp-lt-delete', to_timestamp($4 / 1e9), to_timestamp($5 / 1e9))`,
		ns, ns+7000, ns+3000, ns+8500, ns+9000)

	// Terminal outbox history (immutable Completed/Dead rows) plus the
	// developer idempotency record bound to the surviving create.
	exec(`INSERT INTO outbox_messages (id, kind, operation_id, resource_id, attempt_number, dedupe_key, expected_version, payload_version, payload, state, available_at, completed_at, dead_at, last_error, attempt_count) VALUES
		('ob-drive-lr', 'Drive', 'op-lr-create', NULL, NULL, 'drive-op-lr-create', 1, 1, '{}', 'Completed', to_timestamp($1/1e9), to_timestamp($2/1e9), NULL, NULL, 1),
		('ob-dispatch-lr', 'Dispatch', 'op-lr-create', NULL, 1, 'dispatch-op-lr-create-1', 1, 1, '{}', 'Completed', to_timestamp($1/1e9), to_timestamp($2/1e9), NULL, NULL, 1),
		('ob-drive-lf', 'Drive', 'op-lf-create', NULL, NULL, 'drive-op-lf-create', 1, 1, '{}', 'Dead', to_timestamp($1/1e9), NULL, to_timestamp($5/1e9), 'legacy poison classification', 3),
		('ob-drive-lt', 'Drive', 'op-lt-create', NULL, NULL, 'drive-op-lt-create', 1, 1, '{}', 'Completed', to_timestamp($1/1e9), to_timestamp($2/1e9), NULL, NULL, 1),
		('ob-drive-ltd', 'Drive', 'op-lt-delete', NULL, NULL, 'drive-op-lt-delete', 2, 1, '{}', 'Completed', to_timestamp($3/1e9), to_timestamp($4/1e9), NULL, NULL, 1)`,
		ns, ns+7000, ns+8500, ns+9000, ns+4000)

	exec(`INSERT INTO idempotency_records (scope, idempotency_key, command_kind, fingerprint, resource_id, operation_id) VALUES
		('team/platform', 'legacy-create-key', 'CreateResource', decode('aabbccdd', 'hex'), 'legacy-ready', 'op-lr-create')`)

	// Published contract-owned outputs from the succeeded create.
	exec(`INSERT INTO resource_outputs (resource_id, observed_generation, operation_id, capability, output_mapping_ref, output_contract_digest, values_jsonb, values_digest, published_at_ns) VALUES
		('legacy-ready', 1, 'op-lr-create', 'create', '', 'legacy-contract-digest', '{"endpoint":"legacy.example.internal"}', 'legacy-values-digest', $1)`, ns+7100)

	// M19 private evidence: walk the monotonic phase trigger honestly.
	exec(`INSERT INTO opentofu_attempt_evidence (resource_id, operation_id, attempt_number, provisioner_ref, phase, record_version) VALUES
		('legacy-ready', 'op-lr-create', 1, $1, 'Prepared', 1)`, refPulumi)
	exec(`UPDATE opentofu_attempt_evidence SET phase = 'ApplyMayStart', record_version = 2 WHERE resource_id = 'legacy-ready'`)
	exec(`UPDATE opentofu_attempt_evidence SET phase = 'ApplyExited', record_version = 3 WHERE resource_id = 'legacy-ready'`)
	exec(`UPDATE opentofu_attempt_evidence SET phase = 'ObservedConverged', record_version = 4 WHERE resource_id = 'legacy-ready'`)
	exec(`INSERT INTO opentofu_state_bindings (resource_id, provisioner_ref, engine, program, backend, state_key, lineage, serial, state_digest, record_version) VALUES
		('legacy-ready', $1, 'tofu-cli', 'legacy-program', 'https://legacy-backend.invalid/state', 'legacy/state-key', NULL, NULL, NULL, 1)`, refPulumi)
	exec(`UPDATE opentofu_state_bindings SET lineage = 'legacy-lineage-01', serial = 7,
		state_digest = decode('00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff', 'hex'), record_version = 2
		WHERE resource_id = 'legacy-ready'`)

	// M20 operator audit: an accepted observe trigger over completed work,
	// plus its scoped operator idempotency binding.
	exec(`INSERT INTO outbox_messages (id, kind, operation_id, resource_id, attempt_number, dedupe_key, expected_version, payload_version, payload, state, available_at, completed_at, attempt_count) VALUES
		('ob-observe-lr', 'Observe', 'op-lr-create', NULL, NULL, 'observe-op-lr-create-oa', 1, 1, '{}', 'Completed', to_timestamp($1/1e9), to_timestamp($2/1e9), 1)`,
		ns+6000, ns+6500)
	exec(`INSERT INTO operator_actions (id, actor_principal_id, actor_kind, action, target_kind, target_id, source_work_id, created_work_id, idempotency_digest, request_id) VALUES
		('oa-legacy-1', 'legacy-operator', 'user', 'trigger_observe', 'operation', 'op-lr-create', NULL, 'ob-observe-lr', decode('1122334455667788112233445566778811223344556677881122334455667788', 'hex'), 'req-legacy-1')`)
	exec(`INSERT INTO operator_idempotency (scope, key, fingerprint, operator_action_id) VALUES
		('legacy-operator', 'legacy-operator-key', decode('1122334455667788112233445566778811223344556677881122334455667788', 'hex'), 'oa-legacy-1')`)

	// Inventory sanity at v10: the owner sequence index exists and the three
	// seeded resources are visible to the M16 inventory query.
	count := singleCount(t, pool, "SELECT count(*) FROM resources WHERE owner_kind = 'team' AND owner_id = 'platform'")
	if count != 3 {
		t.Fatalf("seeded inventory rows = %d, want 3", count)
	}
}

// ---- Post-upgrade preservation assertions ----------------------------------

var upgradeTables = []string{
	"resources", "resource_statuses", "resource_conditions", "provisioner_bindings",
	"operations", "events", "provisioning_executions", "provisioning_submission_attempts",
	"idempotency_records", "outbox_messages", "resource_outputs",
	"opentofu_attempt_evidence", "opentofu_state_bindings",
	"operator_actions", "operator_idempotency",
}

func tableCounts(t *testing.T, pool *pgxpool.Pool) map[string]int64 {
	t.Helper()
	counts := make(map[string]int64, len(upgradeTables))
	for _, table := range upgradeTables {
		counts[table] = singleCount(t, pool, "SELECT count(*) FROM "+table)
	}
	return counts
}

func singleCount(t *testing.T, pool *pgxpool.Pool, query string) int64 {
	t.Helper()
	var count int64
	if err := pool.QueryRow(context.Background(), query).Scan(&count); err != nil {
		t.Fatalf("count query failed: %v\nSQL: %s", err, query)
	}
	return count
}

func rawStringColumn(t *testing.T, pool *pgxpool.Pool, query string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return values
}

// assertSeededRowsPreserved spot-checks durable truth across every milestone's
// data class after the upgrade.
func assertSeededRowsPreserved(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	expectInt64 := func(query string, want int64) {
		t.Helper()
		var got int64
		if err := pool.QueryRow(ctx, query).Scan(&got); err != nil {
			t.Fatalf("preservation probe failed: %v\nSQL: %s", err, query)
		}
		if got != want {
			t.Errorf("preservation probe = %d, want %d\nSQL: %s", got, want, query)
		}
	}
	expectText := func(query string, want string) {
		t.Helper()
		var got string
		if err := pool.QueryRow(ctx, query).Scan(&got); err != nil {
			t.Fatalf("preservation probe failed: %v\nSQL: %s", err, query)
		}
		if got != want {
			t.Errorf("preservation probe = %q, want %q\nSQL: %s", got, want, query)
		}
	}

	// Statuses and conditions survived verbatim.
	expectText("SELECT state FROM resource_statuses WHERE resource_id = 'legacy-ready'", "Ready")
	expectText("SELECT message FROM resource_conditions WHERE resource_id = 'legacy-ready' AND type = 'Ready'", "legacy convergence evidence")

	// Terminal operations and their failure evidence survived.
	expectText("SELECT failure_reason FROM operations WHERE id = 'op-lf-create'", "ExecutionFailed")
	expectInt64("SELECT count(*) FROM operations WHERE id IN ('op-lr-create','op-lt-create','op-lt-delete') AND state = 'Succeeded'", 3)

	// Execution and attempt evidence survived.
	expectText("SELECT state FROM provisioning_executions WHERE operation_id = 'op-lr-create'", "Succeeded")
	expectText("SELECT output_resolution FROM provisioning_executions WHERE operation_id = 'op-lr-create'", "Published")
	expectText("SELECT state FROM provisioning_submission_attempts WHERE operation_id = 'op-lr-create' AND attempt_number = 1", "Accepted")

	// Terminal outbox history remains valid and immutable.
	expectInt64("SELECT count(*) FROM outbox_messages WHERE id LIKE 'ob-%' AND state IN ('Completed','Dead')", 6)
	expectText("SELECT last_error FROM outbox_messages WHERE id = 'ob-drive-lf'", "legacy poison classification")

	// Developer idempotency and published outputs survived.
	expectInt64("SELECT count(*) FROM idempotency_records WHERE scope = 'team/platform'", 1)
	expectText("SELECT values_jsonb->>'endpoint' FROM resource_outputs WHERE resource_id = 'legacy-ready'", "legacy.example.internal")

	// M19 evidence survived at its converged phase with its state binding.
	expectText("SELECT phase FROM opentofu_attempt_evidence WHERE resource_id = 'legacy-ready'", "ObservedConverged")
	expectInt64("SELECT serial FROM opentofu_state_bindings WHERE resource_id = 'legacy-ready'", 7)

	// M20 operator audit survived append-only.
	expectText("SELECT action FROM operator_actions WHERE id = 'oa-legacy-1'", "trigger_observe")
	expectInt64("SELECT count(*) FROM operator_idempotency WHERE scope = 'legacy-operator'", 1)
}

// assertTerminalHistoryPreserved re-checks the immutable seeded history after
// the qualification's own post-upgrade lifecycle activity: mutable fields the
// new operations legitimately rewrote (statuses, conditions, generations) are
// deliberately excluded here.
func assertTerminalHistoryPreserved(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	expectInt64 := func(query string, want int64) {
		t.Helper()
		var got int64
		if err := pool.QueryRow(ctx, query).Scan(&got); err != nil {
			t.Fatalf("terminal history probe failed: %v\nSQL: %s", err, query)
		}
		if got != want {
			t.Errorf("terminal history probe = %d, want %d\nSQL: %s", got, want, query)
		}
	}

	expectInt64("SELECT count(*) FROM outbox_messages WHERE id LIKE 'ob-%' AND state IN ('Completed','Dead')", 6)
	expectInt64("SELECT count(*) FROM provisioning_submission_attempts WHERE operation_id = 'op-lr-create' AND attempt_number = 1 AND state = 'Accepted'", 1)
	expectInt64("SELECT count(*) FROM resource_outputs WHERE resource_id = 'legacy-ready'", 1)
	expectInt64("SELECT count(*) FROM opentofu_attempt_evidence WHERE resource_id = 'legacy-ready' AND phase = 'ObservedConverged'", 1)
	expectInt64("SELECT count(*) FROM operator_actions WHERE id = 'oa-legacy-1'", 1)
	expectInt64("SELECT count(*) FROM idempotency_records WHERE scope = 'team/platform'", 1)
}

func assertLegacyResourceReadable(t *testing.T, store *postgres.Store) {
	t.Helper()
	var record application.ResourceRecord
	err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		var err error
		record, err = tx.Resources().GetResource(context.Background(), domain.ResourceID("legacy-ready"))
		return err
	})
	if err != nil {
		t.Fatalf("API read of legacy resource failed: %v", err)
	}
	if record.Resource.Generation() != 1 {
		t.Fatalf("legacy-ready generation = %d, want 1", record.Resource.Generation())
	}
	if record.Status.State() != domain.ResourceStateReady {
		t.Fatalf("legacy-ready state = %s, want Ready", record.Status.State())
	}
	foundCondition := false
	for _, condition := range record.Status.Conditions() {
		if condition.Type() == "Ready" && condition.Reason() == "ProvisionerReportedReady" {
			foundCondition = true
		}
	}
	if !foundCondition {
		t.Fatalf("legacy-ready lost its Ready condition after upgrade")
	}
}

func insertWakeDependentsWork(t *testing.T, pool *pgxpool.Pool, targetID string) {
	t.Helper()
	ctx := context.Background()
	var version int64
	if err := pool.QueryRow(ctx,
		"SELECT record_version FROM resources WHERE id = $1", targetID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO outbox_messages (id, kind, resource_id, dedupe_key, expected_version, payload_version, payload)
		 VALUES ('wakemanual-post-upgrade', 'WakeDependents', $1, 'wake-manual-post-upgrade', $2, 1, '{}')`,
		targetID, version); err != nil {
		t.Fatalf("insert WakeDependents work failed: %v", err)
	}
}

// TestM21_5QuotaDenialWithReferencesPersistsNoRelationshipEdges is the
// adversarial M18xM21 cross-milestone check: a reference-bearing create that
// is refused by the transactional quota must leave NO relationship rows and no
// protective evidence behind.
func TestM21_5QuotaDenialWithReferencesPersistsNoRelationshipEdges(t *testing.T) {
	pool, cleanup := migratedPool(t)
	defer cleanup()

	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	world := newUpgradeWorld(t, pool, store, provisioningfake.New(provisioningfake.ModeSynchronous))
	defer world.close()

	// The target exists and is Ready; the dependent create carries a valid
	// reference but is denied by the quota policy.
	world.create(t, "quota-target", nil)
	world.drain(t)

	denying := quotaDenyAllPolicy{}
	denyingService, err := application.NewService(world.catalogPort(), &applicationfake.Selector{Ref: world.selectorRef()},
		world.resolver, store, applicationfake.AllowAll{}, denying)
	if err != nil {
		t.Fatal(err)
	}
	spec := upgradeSpec(t, "quota-dependent")
	command := application.CreateResourceCommand{
		Actor: applicationfake.Principal("upgrader"), ID: domain.ResourceID("quota-dependent"),
		Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: spec, References: map[string][]string{"dependency": {"quota-target"}},
		OperationID:    domain.OperationID("op-quota-denied"),
		EventID:        domain.EventID("evt-quota-denied"),
		RequestedAt:    time.Now().UTC(),
		IdempotencyKey: "key-quota-denied",
	}
	if _, err := denyingService.AdmitCreateResource(context.Background(), command); !errors.Is(err, application.ErrQuotaExceeded) {
		t.Fatalf("quota denial = %v, want ErrQuotaExceeded", err)
	}

	for source, table := range map[string]string{
		"resource_desired_references": "SELECT count(*) FROM resource_desired_references WHERE source_id = 'quota-dependent'",
		"resource_applied_references": "SELECT count(*) FROM resource_applied_references WHERE source_id = 'quota-dependent'",
	} {
		if count := singleCount(t, pool, table); count != 0 {
			t.Errorf("%s persisted %d rows for a quota-denied create", source, count)
		}
	}
	if protected := singleCount(t, pool,
		"SELECT count(*) FROM resource_desired_references WHERE target_id = 'quota-target'"); protected != 0 {
		t.Errorf("denied admission left %d protective edges on the target", protected)
	}

	// The same admission succeeds once the restriction lifts; the reference
	// path itself was never corrupted by the earlier refusal.
	world.create(t, "quota-dependent", map[string][]string{"dependency": {"quota-target"}})
	world.drain(t)
	if state := world.state(t, "quota-dependent"); state != domain.ResourceStateReady {
		t.Fatalf("post-denial dependent state = %s, want Ready", state)
	}
}

// quotaDenyAllPolicy refuses every create as quota-exceeded without touching
// durable state.
type quotaDenyAllPolicy struct{}

func (quotaDenyAllPolicy) Revision() application.PolicyRevision {
	return application.NewPolicyRevision([]byte(`{"apiVersion":"liftr.dev/admission-policy/v1","rules":[{"id":"test-deny","kind":"resource-count-quota","limit":0}]}`))
}

func (p quotaDenyAllPolicy) Plan(intent application.AdmissionIntent) (application.AdmissionPlan, error) {
	return application.AdmissionPlan{
		Intent:   intent,
		Revision: p.Revision(),
		CountConstraints: []application.ResourceCountConstraint{{
			RuleID: "test-deny", Dimension: application.QuotaOwnerResources, Limit: 0,
		}},
	}, nil
}

func (p quotaDenyAllPolicy) Decide(plan application.AdmissionPlan, facts application.ResourceCountFacts) (application.AdmissionDecision, error) {
	return application.AdmissionDecision{
		Outcome:  application.AdmissionDenied,
		Revision: plan.Revision,
		Denial: &application.PolicyDenial{
			Kind: application.PolicyDenialQuotaExceeded, RuleID: "test-deny",
			Measure: string(application.QuotaOwnerResources),
			Current: facts.OwnerNonDeleted, Requested: facts.OwnerNonDeleted + 1, Limit: 0,
		},
	}, nil
}

// ---- Composition over the upgraded schema -----------------------------------

type upgradeWorld struct {
	service  *application.Service
	store    *postgres.Store
	pool     *pgxpool.Pool
	resolver *applicationfake.Resolver
	ref      application.ProvisionerRef
	catalog  application.ResourceTypeCatalog
	instance *worker.Worker
	cleanup  func()
}

func newUpgradeWorld(t *testing.T, pool *pgxpool.Pool, store *postgres.Store, provider provisioning.Provisioner) *upgradeWorld {
	t.Helper()
	ref, _ := application.NewProvisionerRef("upgrade-provider")
	resolver := &applicationfake.Resolver{Providers: map[application.ProvisionerRef]provisioning.Provisioner{ref: provider}}
	// Pre-upgrade rows carry their own durable provisioner binding; the
	// upgraded control plane must still resolve it.
	legacyRef, _ := application.NewProvisionerRef("legacy-pulumi-ref")
	resolver.Providers[legacyRef] = provisioningfake.New(provisioningfake.ModeSynchronous)
	typeValue, err := domain.NewResourceType(provisioningfake.ResourceType(), "Upgrade qualification resource",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		t.Fatal(err)
	}
	slots, err := resourcecontract.NewReferenceContract([]resourcecontract.ReferenceSlot{{
		Name:               "dependency",
		AllowedTargetTypes: []domain.ResourceTypeRef{provisioningfake.ResourceType()},
		MinItems:           0,
		MaxItems:           1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	catalog := applicationfake.Catalog{
		Types:      map[domain.ResourceTypeRef]domain.ResourceType{provisioningfake.ResourceType(): typeValue},
		References: map[domain.ResourceTypeRef]*resourcecontract.ReferenceContract{provisioningfake.ResourceType(): &slots},
	}
	service, err := application.NewService(catalog, &applicationfake.Selector{Ref: ref}, resolver, store, applicationfake.AllowAll{})
	if err != nil {
		t.Fatal(err)
	}
	instance, err := worker.NewWithCatalog(store, resolver, catalog)
	if err != nil {
		t.Fatal(err)
	}
	instance.RetryBase = 5 * time.Millisecond
	return &upgradeWorld{service: service, store: store, pool: pool, resolver: resolver,
		ref: ref, catalog: catalog, instance: instance, cleanup: func() {}}
}

func (w *upgradeWorld) selectorRef() application.ProvisionerRef      { return w.ref }
func (w *upgradeWorld) catalogPort() application.ResourceTypeCatalog { return w.catalog }

func (w *upgradeWorld) close() { w.cleanup() }

func (w *upgradeWorld) pumpSteps(t *testing.T, steps int) {
	t.Helper()
	for range steps {
		if _, err := w.instance.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func (w *upgradeWorld) drain(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		worked, err := w.instance.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if worked {
			continue
		}
		if singleCount(t, w.pool, "SELECT count(*) FROM outbox_messages WHERE state = 'Pending'") == 0 {
			return
		}
		// Delayed work (bounded observation backoff) exists; wait for its
		// availability window instead of declaring false quiescence.
		if time.Now().After(deadline) {
			for _, row := range rawStringColumn(t, w.pool,
				"SELECT id || ' ' || kind || ' ' || state || ' avail=' || available_at || ' attempts=' || attempt_count || ' err=' || coalesce(last_error,'') FROM outbox_messages WHERE state IN ('Pending','Leased') ORDER BY id") {
				t.Logf("STUCK %s", row)
			}
			for _, row := range rawStringColumn(t, w.pool,
				"SELECT 'clock=' || clock_timestamp() || ' now=' || now()") {
				t.Logf("STUCK %s", row)
			}
			for _, row := range rawStringColumn(t, w.pool,
				"SELECT operation_id || ' state=' || state || ' att=' || current_attempt_number || ' nextobs=' || next_observation_sequence || ' lastfail=' || coalesce(last_failure_reason,'') FROM provisioning_executions WHERE operation_id LIKE '%upgrade-%'") {
				t.Logf("STUCK exec %s", row)
			}
			for _, row := range rawStringColumn(t, w.pool,
				"SELECT id || ' reason=' || coalesce(terminal_reason,'') FROM outbox_messages WHERE kind='Observe' AND operation_id='op-up-create-upgrade-anchor' ORDER BY created_at DESC LIMIT 3") {
				t.Logf("STUCK obs %s", row)
			}
			t.Fatal("upgrade worker did not settle within the deadline")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func upgradeSpec(t *testing.T, name string) domain.ResourceSpec {
	t.Helper()
	spec, err := domain.NewResourceSpec(map[string]any{"size": uint64(5), "name": name})
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func (w *upgradeWorld) create(t *testing.T, id string, references map[string][]string) application.Result {
	t.Helper()
	command := application.CreateResourceCommand{
		Actor: applicationfake.Principal("upgrader"), ID: domain.ResourceID(id),
		Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: upgradeSpec(t, id), References: references,
		OperationID:    domain.OperationID("op-up-create-" + id),
		EventID:        domain.EventID("evt-up-create-" + id),
		RequestedAt:    time.Now().UTC(),
		IdempotencyKey: "key-up-create-" + id,
	}
	result, err := w.service.AdmitCreateResource(context.Background(), command)
	if err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	return result
}

// createWithGate admits an anchor whose convergence is held by the caller's
// gate provider, giving the test deterministic control of readiness.
func (w *upgradeWorld) createWithGate(t *testing.T, id string) application.Result {
	t.Helper()
	return w.create(t, id, nil)
}

func (w *upgradeWorld) update(t *testing.T, id string, references map[string][]string, present bool, key string) {
	t.Helper()
	command := application.UpdateResourceCommand{
		Actor:              applicationfake.Principal("upgrader"),
		ID:                 domain.ResourceID(id),
		ExpectedGeneration: w.generation(t, id),
		Spec:               upgradeSpec(t, id+"-"+key),
		ReferencesPresent:  present,
		References:         references,
		OperationID:        domain.OperationID("op-up-update-" + key),
		EventID:            domain.EventID("evt-up-update-" + key),
		RequestedAt:        time.Now().UTC(),
		IdempotencyKey:     "key-up-" + key,
	}
	if _, err := w.service.AdmitUpdateResource(context.Background(), command); err != nil {
		t.Fatalf("update %s: %v", id, err)
	}
}

func (w *upgradeWorld) remove(t *testing.T, id string, key string) error {
	t.Helper()
	command := application.DeleteResourceCommand{
		Actor:              applicationfake.Principal("upgrader"),
		ID:                 domain.ResourceID(id),
		ExpectedGeneration: w.generation(t, id),
		OperationID:        domain.OperationID("op-up-delete-" + key),
		EventID:            domain.EventID("evt-up-delete-" + key),
		RequestedAt:        time.Now().UTC(),
		IdempotencyKey:     "key-up-delete-" + key,
	}
	_, err := w.service.AdmitDeleteResource(context.Background(), command)
	return err
}

func (w *upgradeWorld) removeExpectingUseError(t *testing.T, id string, key string) error {
	t.Helper()
	return w.remove(t, id, key)
}

func (w *upgradeWorld) generation(t *testing.T, id string) uint64 {
	t.Helper()
	var generation uint64
	err := w.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		record, err := tx.Resources().GetResource(context.Background(), domain.ResourceID(id))
		if err != nil {
			return err
		}
		generation = record.Resource.Generation()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return generation
}

func (w *upgradeWorld) state(t *testing.T, id string) domain.ResourceState {
	t.Helper()
	return upgradeResourceStateWithin(w.store, t, id)
}

func (w *upgradeWorld) conditionOf(t *testing.T, id string) (domain.ConditionStatus, string, bool) {
	t.Helper()
	var status domain.ConditionStatus
	var reason string
	found := false
	err := w.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		record, err := tx.Resources().GetResource(context.Background(), domain.ResourceID(id))
		if err != nil {
			return err
		}
		for _, condition := range record.Status.Conditions() {
			if condition.Type() == lifecycle.ConditionDependenciesReady {
				status, reason, found = condition.Status(), condition.Reason(), true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return status, reason, found
}

func (w *upgradeWorld) edges(t *testing.T, applied bool, id string) []application.ReferenceEdge {
	t.Helper()
	var edges []application.ReferenceEdge
	err := w.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		ctx := context.Background()
		var err error
		if applied {
			edges, err = tx.References().AppliedReferences(ctx, domain.ResourceID(id))
		} else {
			edges, err = tx.References().DesiredReferences(ctx, domain.ResourceID(id))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return edges
}

func (w *upgradeWorld) lastOperation(t *testing.T, id string) domain.OperationID {
	t.Helper()
	rows, err := w.pool.Query(context.Background(),
		"SELECT id FROM operations WHERE resource_id = $1 ORDER BY operation_seq DESC LIMIT 1", id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("no operations found for %s", id)
	}
	var operationID string
	if err := rows.Scan(&operationID); err != nil {
		t.Fatal(err)
	}
	return domain.OperationID(operationID)
}

func upgradeResourceState(t *testing.T, store *postgres.Store, id string) domain.ResourceState {
	t.Helper()
	return upgradeResourceStateWithin(store, t, id)
}

func upgradeResourceStateWithin(store *postgres.Store, t *testing.T, id string) domain.ResourceState {
	t.Helper()
	var state domain.ResourceState
	err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		record, err := tx.Resources().GetResource(context.Background(), domain.ResourceID(id))
		if err != nil {
			return err
		}
		state = record.Status.State()
		return nil
	})
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	return state
}

// upgradeGateProvider holds every execution in Running until release() is
// called, giving the qualification deterministic convergence control.
type upgradeGateProvider struct {
	held        chan struct{}
	mu          sync.Mutex
	submissions map[domain.OperationID]int
}

func newUpgradeGateProvider() *upgradeGateProvider {
	return &upgradeGateProvider{held: make(chan struct{}), submissions: make(map[domain.OperationID]int)}
}

func (p *upgradeGateProvider) release() { close(p.held) }

func (p *upgradeGateProvider) Capabilities() []provisioning.ProvisionerCapability {
	return []provisioning.ProvisionerCapability{{ResourceType: provisioningfake.ResourceType(), Capability: domain.CapabilityCreate},
		{ResourceType: provisioningfake.ResourceType(), Capability: domain.CapabilityUpdate},
		{ResourceType: provisioningfake.ResourceType(), Capability: domain.CapabilityDelete}}
}

func (p *upgradeGateProvider) Submit(_ context.Context, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	p.mu.Lock()
	p.submissions[request.OperationID]++
	p.mu.Unlock()
	handle, err := provisioning.NewExecutionHandle("upgrade-" + string(request.OperationID))
	if err != nil {
		return provisioning.Submission{}, err
	}
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle},
		Resource: provisioning.ResourceObservation{Presence: provisioning.ResourcePresencePresent,
			Readiness: provisioning.ResourceReadinessNotReady, Drift: provisioning.ResourceDriftInSync},
	}}, nil
}

func (p *upgradeGateProvider) Observe(_ context.Context, _ provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	select {
	case <-p.held:
		return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
			Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded},
			Resource: provisioning.ResourceObservation{Presence: provisioning.ResourcePresencePresent,
				Readiness: provisioning.ResourceReadinessReady, Drift: provisioning.ResourceDriftInSync}}, nil
	default:
		return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
			Execution: &provisioning.Execution{State: provisioning.ExecutionStateRunning}}, nil
	}
}

func (p *upgradeGateProvider) submissionCount(operationID domain.OperationID) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.submissions[operationID]
}
