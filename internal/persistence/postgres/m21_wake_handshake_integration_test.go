// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/worker"
)

// Deterministic real-PostgreSQL proofs of the M21 versioned-wake handshake
// (ADR-0022). Both interleavings are driven with explicit row-lock barriers —
// two pooled connections, FOR UPDATE on the target row, and a
// pg_stat_activity lock-wait probe — so no step relies on timing or sleep.
//
// The statements executed here mirror the exact repository SQL of the target
// transition (lock row -> mutate -> conditional/coalescing enqueue) and the
// wake finalizer (lock row -> read version -> terminalize old -> insert
// follow-up) so the proof binds to production behavior.

type wakeSession struct {
	t   *testing.T
	ctx context.Context
	w   *m21World
	c   *pgxpool.Conn
	pid int
}

func acquireWakeSession(t *testing.T, w *m21World) *wakeSession {
	t.Helper()
	conn, err := w.pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := &wakeSession{t: t, ctx: context.Background(), w: w, c: conn}
	if err := s.c.QueryRow(context.Background(), "SELECT pg_backend_pid()").Scan(&s.pid); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *wakeSession) release() { s.c.Release() }

func (s *wakeSession) begin() {
	s.t.Helper()
	if _, err := s.c.Exec(s.ctx, "BEGIN"); err != nil {
		s.t.Fatal(err)
	}
}

func (s *wakeSession) commit() {
	s.t.Helper()
	if _, err := s.c.Exec(s.ctx, "COMMIT"); err != nil {
		s.t.Fatal(err)
	}
}

func (s *wakeSession) lockTarget(id string) {
	s.t.Helper()
	if _, err := s.c.Exec(s.ctx, `SELECT id FROM resources WHERE id=$1 FOR UPDATE`, id); err != nil {
		s.t.Fatal(err)
	}
}

func scanUint64(dest *uint64) any { return &uint64Scanner{dest} }

type uint64Scanner struct{ dest *uint64 }

func (n *uint64Scanner) Scan(src any) error {
	text, ok := src.(string)
	if !ok {
		return errors.New("expected numeric text")
	}
	var parsed uint64
	for _, char := range []byte(text) {
		if char < '0' || char > '9' {
			return fmt.Errorf("invalid numeric %q", text)
		}
		parsed = parsed*10 + uint64(char-'0')
	}
	*n.dest = parsed
	return nil
}

func (s *wakeSession) currentVersion(id string) uint64 {
	s.t.Helper()
	var version uint64
	if err := s.c.QueryRow(s.ctx, `SELECT record_version::text FROM resources WHERE id=$1`, id).Scan(scanUint64(&version)); err != nil {
		s.t.Fatal(err)
	}
	return version
}

func (s *wakeSession) advanceTarget(id string) uint64 {
	s.t.Helper()
	var version uint64
	if err := s.c.QueryRow(s.ctx, `UPDATE resources SET record_version = record_version + 1, updated_at_ns = updated_at_ns + 1 WHERE id=$1 RETURNING record_version::text`, id).Scan(scanUint64(&version)); err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.c.Exec(s.ctx, `UPDATE resource_statuses SET updated_at_ns = updated_at_ns + 1 WHERE resource_id=$1`, id); err != nil {
		s.t.Fatal(err)
	}
	return version
}

func (s *wakeSession) hasWaiters(id string) bool {
	s.t.Helper()
	var exists bool
	if err := s.c.QueryRow(s.ctx, `SELECT EXISTS(SELECT 1 FROM operation_dependency_waits WHERE target_id=$1)`, id).Scan(&exists); err != nil {
		s.t.Fatal(err)
	}
	return exists
}

func (s *wakeSession) activeWakeExists(id string) bool {
	s.t.Helper()
	var exists bool
	if err := s.c.QueryRow(s.ctx, `SELECT EXISTS(SELECT 1 FROM outbox_messages
		 WHERE kind='WakeDependents' AND resource_id=$1 AND state IN ('Pending','Leased'))`, id).Scan(&exists); err != nil {
		s.t.Fatal(err)
	}
	return exists
}

func (s *wakeSession) completeWake(resourceID string, version uint64) {
	s.t.Helper()
	tag, err := s.c.Exec(s.ctx, `UPDATE outbox_messages SET state='Completed', completed_at=clock_timestamp(), terminal_reason='Woke'
		 WHERE kind='WakeDependents' AND resource_id=$1 AND expected_version=$2::numeric AND state IN ('Pending','Leased')`,
		resourceID, version)
	if err != nil {
		s.t.Fatal(err)
	}
	if tag.RowsAffected() != 1 {
		s.t.Fatalf("expected to terminalize wake v%d for %s", version, resourceID)
	}
}

func (s *wakeSession) insertWake(resourceID string, version uint64) {
	s.t.Helper()
	key := fmt.Sprintf("wake-dependents:%s:%d", resourceID, version)
	if _, err := s.c.Exec(s.ctx, `INSERT INTO outbox_messages
			(id, kind, operation_id, resource_id, attempt_number, dedupe_key, expected_version, sequence, payload_version, payload, state, available_at)
			VALUES ($1,'WakeDependents',NULL,$2,NULL,$1,$3::numeric,NULL,1,'{}','Pending',clock_timestamp())
			ON CONFLICT DO NOTHING`, key, resourceID, version); err != nil {
		s.t.Fatal(err)
	}
}

// waitUntilBlocked polls pg_stat_activity until the given backend is waiting
// on a lock — event-driven verification of the barrier with a hard deadline.
func waitUntilBlocked(t *testing.T, pool *pgxpool.Pool, pid int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var blocked bool
		err := pool.QueryRow(context.Background(),
			`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE pid=$1 AND wait_event_type='Lock')`, pid).Scan(&blocked)
		if err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("expected session to block on the target row lock")
}

func seedWakeScenario(t *testing.T, w *m21World, id string) uint64 {
	t.Helper()
	now := time.Now().UnixNano()
	w.rawExec(t, `INSERT INTO resources (id,type_name,type_version,owner_kind,owner_id,generation,spec_codec_version,spec,record_version,created_at_ns,updated_at_ns)
		 VALUES ($1,'Widget','v1','team','planning',1,1,'{}',5,$2::bigint,$2::bigint)`, id, now)
	w.rawExec(t, `INSERT INTO resource_statuses (resource_id,observed_generation,state,updated_at_ns) VALUES ($1,1,'Ready',$2::bigint)`, id, now)
	opID := "op-hs-" + id
	w.rawExec(t, `INSERT INTO operations (id,resource_id,capability,target_generation,state,phase,requested_at_ns,started_at_ns,phase_changed_at_ns,record_version)
		 VALUES ($1::text,$2::text,'create',1,'Running','Applying',$3::bigint,$3::bigint,$3::bigint,7)`, opID, id, now)
	w.rawExec(t, `INSERT INTO operation_dependency_waits (operation_id,target_id,operation_version,registered_target_version)
		 VALUES ($1::text,$2::text,7,5)`, opID, id)
	// Both interleavings start with an ACTIVE versioned wake for the target.
	w.rawExec(t, `INSERT INTO outbox_messages
			(id, kind, operation_id, resource_id, attempt_number, dedupe_key, expected_version, sequence, payload_version, payload, state, available_at)
			VALUES ($1,'WakeDependents',NULL,$2,NULL,$1,$3::numeric,NULL,1,'{}','Pending',clock_timestamp())`,
		fmt.Sprintf("wake-dependents:%s:5", id), id, uint64(5))
	return 5
}

func countActiveWakesSQL(t *testing.T, w *m21World, id string, version uint64) int {
	t.Helper()
	var count int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox_messages
		 WHERE kind='WakeDependents' AND resource_id=$1 AND state IN ('Pending','Leased') AND expected_version=$2::numeric`,
		id, version).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func countCompletedWakesSQL(t *testing.T, w *m21World, id string, version uint64) int {
	t.Helper()
	var count int
	if err := w.pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox_messages
		 WHERE kind='WakeDependents' AND resource_id=$1 AND expected_version=$2::numeric AND state='Completed'`,
		id, version).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// TestM21PostgresWakeHandshakeInterleavingA proves the finalizer-first order:
// the finalizer locks the target row first, completes V5 (target still at its
// represented version), commits; ONLY THEN does the blocked gate-relevant
// transition obtain the row, advance to V6, observe waiters, and mint Wake V6.
// No gate-relevant change is lost and exactly one current wake stands after.
func TestM21PostgresWakeHandshakeInterleavingA(t *testing.T) {
	w := newM21World(t)
	defer w.cleanup()
	id := "hs-a-target"
	v5 := seedWakeScenario(t, w, id)

	finalizer := acquireWakeSession(t, w)
	defer finalizer.release()
	transition := acquireWakeSession(t, w)
	defer transition.release()

	finalizer.begin()
	finalizer.lockTarget(id)

	transition.begin()
	blocked := make(chan struct{})
	go func() {
		defer close(blocked)
		transition.lockTarget(id) // must block behind the finalizer
	}()
	waitUntilBlocked(t, w.pool, transition.pid)

	if got := finalizer.currentVersion(id); got != v5 {
		t.Fatalf("finalizer saw version %d, want %d", got, v5)
	}
	finalizer.completeWake(id, v5)
	finalizer.commit()

	<-blocked // the transition unblocks deterministically after the commit
	newVersion := transition.advanceTarget(id)
	if newVersion != v5+1 {
		t.Fatalf("advanced version = %d, want %d", newVersion, v5+1)
	}
	if !transition.hasWaiters(id) {
		t.Fatal("waiters vanished before the transition committed")
	}
	transition.insertWake(id, newVersion)
	transition.commit()

	if n := countActiveWakesSQL(t, w, id, newVersion); n != 1 {
		t.Fatalf("active V%d wakes = %d, want exactly 1", newVersion, n)
	}
}

// TestM21PostgresWakeHandshakeInterleavingB proves the transition-first order:
// the transition locks the row, advances V5->V6, finds an ACTIVE V5 wake and
// coalesces (no second row); the later finalizer then locks the row, observes
// V6, terminalizes V5 and inserts EXACTLY ONE current V6 wake atomically —
// terminalizing before inserting so the one-active constraint admits it.
func TestM21PostgresWakeHandshakeInterleavingB(t *testing.T) {
	w := newM21World(t)
	defer w.cleanup()
	id := "hs-b-target"
	v5 := seedWakeScenario(t, w, id)

	transition := acquireWakeSession(t, w)
	defer transition.release()
	finalizer := acquireWakeSession(t, w)
	defer finalizer.release()

	transition.begin()
	transition.lockTarget(id)
	newVersion := transition.advanceTarget(id)
	if !transition.hasWaiters(id) {
		t.Fatal("waiters missing for the coalescing check")
	}
	if !transition.activeWakeExists(id) {
		t.Fatal("V5 wake should still be active during the coalescing window")
	}
	// Coalesce: deliberately NO insert while V5 is active.
	transition.commit()

	finalizer.begin()
	finalizer.lockTarget(id)
	current := finalizer.currentVersion(id)
	if current != newVersion {
		t.Fatalf("finalizer observed version %d, want %d", current, newVersion)
	}
	finalizer.completeWake(id, v5)    // terminalize FIRST...
	finalizer.insertWake(id, current) // ...then insert the follow-up
	finalizer.commit()

	if n := countActiveWakesSQL(t, w, id, newVersion); n != 1 {
		t.Fatalf("active V%d wakes = %d, want exactly 1", newVersion, n)
	}
	if got := countCompletedWakesSQL(t, w, id, v5); got != 1 {
		t.Fatalf("completed V5 wakes = %d, want exactly 1", got)
	}
}

// TestM21PostgresWakeGlobalDedupeRegression pins Correction 2's reusability
// rule through the REAL repository port against REAL PostgreSQL: after Wake V1
// completes, a later versioned wake inserts cleanly; an exact duplicate
// coalesces silently; a different version folds behind the active one. Global
// dedupe_key uniqueness across terminal history can never suppress future
// versioned wakes.
func TestM21PostgresWakeGlobalDedupeRegression(t *testing.T) {
	w := newM21World(t)
	defer w.cleanup()
	ctx := context.Background()

	now := time.Now().UnixNano()
	w.rawExec(t, `INSERT INTO resources (id,type_name,type_version,owner_kind,owner_id,generation,spec_codec_version,spec,record_version,created_at_ns,updated_at_ns)
		 VALUES ('dedupe-target','Widget','v1','team','planning',1,1,'{"kind":"object","object":{},"list":null}',9,$1::bigint,$1::bigint)`, now)
	w.rawExec(t, `INSERT INTO resource_statuses (resource_id,observed_generation,state,updated_at_ns) VALUES ('dedupe-target',1,'Ready',$1)`, now)
	// The durable worker loads Resources through the full three-way join, so
	// the seed needs its binding row too.
	w.rawExec(t, `INSERT INTO provisioner_bindings (resource_id, provisioner_ref) VALUES ('dedupe-target', 'm21-pg-provider')`)

	enqueue := func(version uint64) error {
		return w.store.Within(ctx, func(tx application.UnitOfWork) error {
			return tx.Outbox().EnqueueWakeDependents(ctx, application.WakeDependentsMessage("dedupe-target", version))
		})
	}

	// V1 lives, then completes through the real claim/complete machinery.
	if err := enqueue(1); err != nil {
		t.Fatal(err)
	}
	err := w.store.Within(ctx, func(tx application.UnitOfWork) error {
		message, found, err := tx.Outbox().ClaimOutbox(ctx, "dedupe-regression-token", time.Minute)
		if err != nil {
			return err
		}
		if !found || message.Kind != application.OutboxWakeDependents {
			return errors.New("regression expected to claim the V1 wake")
		}
		return tx.Outbox().CompleteOutbox(ctx, message.ID, message.LeaseToken, "RegressionComplete")
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := countCompletedWakesSQL(t, w, "dedupe-target", 1); got != 1 {
		t.Fatalf("completed V1 rows = %d, want 1", got)
	}

	// Historical V1 must not suppress V2.
	if err := enqueue(2); err != nil {
		t.Fatalf("versioned wake after completed predecessor failed: %v", err)
	}
	// Exact duplicate coalesces silently.
	if err := enqueue(2); err != nil {
		t.Fatalf("exact duplicate enqueue errored: %v", err)
	}
	// Different version while V2 active folds behind it.
	if err := enqueue(3); err != nil {
		t.Fatalf("coalesced enqueue errored: %v", err)
	}
	if n := countActiveWakesSQL(t, w, "dedupe-target", 2); n != 1 {
		t.Fatalf("active V2 wakes = %d, want exactly 1", n)
	}
	if n := countActiveWakesSQL(t, w, "dedupe-target", 3); n != 0 {
		t.Fatalf("active V3 wakes = %d, want 0 (coalesced)", n)
	}

	// A durable worker processes the active V2 wake end-to-end and the
	// finalizer's no-op handshake leaves zero active wakes behind.
	instance, err := worker.NewWithCatalog(w.store, w.resolver, w.catalog)
	if err != nil {
		t.Fatal(err)
	}
	for range 16 {
		worked, runErr := instance.RunOnce(ctx)
		if runErr != nil {
			t.Fatal(runErr)
		}
		if !worked {
			break
		}
	}
	if n := countActiveWakesSQL(t, w, "dedupe-target", 2); n != 0 {
		t.Fatalf("V2 wake survived worker processing (%d active)", n)
	}
	if got := countCompletedWakesSQL(t, w, "dedupe-target", 2); got != 1 {
		t.Fatalf("completed V2 rows = %d, want 1", got)
	}
}
