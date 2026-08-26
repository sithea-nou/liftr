// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
)

func (r *repositories) References() application.ReferenceRepository           { return r }
func (r *repositories) DependencyWaits() application.DependencyWaitRepository { return r }

func scanReferenceEdges(rows pgx.Rows) ([]application.ReferenceEdge, error) {
	defer rows.Close()
	edges := []application.ReferenceEdge{}
	for rows.Next() {
		var slot, target, generationText string
		if err := rows.Scan(&slot, &target, &generationText); err != nil {
			return nil, translateError(err)
		}
		generation, err := parseUint64(generationText)
		if err != nil {
			return nil, err
		}
		edges = append(edges, application.ReferenceEdge{Slot: slot, TargetID: domain.ResourceID(target), Generation: generation})
	}
	return edges, translateError(rows.Err())
}

func (r *repositories) DesiredReferences(ctx context.Context, source domain.ResourceID) ([]application.ReferenceEdge, error) {
	rows, err := r.tx.Query(ctx, `SELECT slot, target_id, generation::text
		FROM resource_desired_references WHERE source_id=$1 ORDER BY slot, target_id`, string(source))
	if err != nil {
		return nil, translateError(err)
	}
	return scanReferenceEdges(rows)
}

func (r *repositories) AppliedReferences(ctx context.Context, source domain.ResourceID) ([]application.ReferenceEdge, error) {
	rows, err := r.tx.Query(ctx, `SELECT slot, target_id, generation::text
		FROM resource_applied_references WHERE source_id=$1 ORDER BY slot, target_id`, string(source))
	if err != nil {
		return nil, translateError(err)
	}
	return scanReferenceEdges(rows)
}

func insertReferenceEdges(ctx context.Context, r *repositories, table string, source domain.ResourceID, generation uint64, edges []application.ReferenceEdge) error {
	for _, edge := range edges {
		if _, err := r.tx.Exec(ctx, fmt.Sprintf(
			`INSERT INTO %s (source_id, slot, target_id, generation) VALUES ($1,$2,$3,$4::numeric)`,
			table), string(source), edge.Slot, string(edge.TargetID), uintText(generation)); err != nil {
			return translateError(err)
		}
	}
	return nil
}

func (r *repositories) ReplaceDesiredReferences(ctx context.Context, source domain.ResourceID, generation uint64, edges []application.ReferenceEdge) error {
	if _, err := r.tx.Exec(ctx, `DELETE FROM resource_desired_references WHERE source_id=$1`, string(source)); err != nil {
		return translateError(err)
	}
	return insertReferenceEdges(ctx, r, "resource_desired_references", source, generation, edges)
}

func (r *repositories) AdvanceAppliedReferences(ctx context.Context, source domain.ResourceID, generation uint64, edges []application.ReferenceEdge) error {
	if _, err := r.tx.Exec(ctx, `DELETE FROM resource_applied_references WHERE source_id=$1`, string(source)); err != nil {
		return translateError(err)
	}
	return insertReferenceEdges(ctx, r, "resource_applied_references", source, generation, edges)
}

func (r *repositories) DeleteReferencesForSource(ctx context.Context, source domain.ResourceID) error {
	if _, err := r.tx.Exec(ctx, `DELETE FROM resource_desired_references WHERE source_id=$1`, string(source)); err != nil {
		return translateError(err)
	}
	if _, err := r.tx.Exec(ctx, `DELETE FROM resource_applied_references WHERE source_id=$1`, string(source)); err != nil {
		return translateError(err)
	}
	return nil
}

// HasInboundProtectiveReference implements the fail-closed protective rule:
// ANY inbound row of either table is protective evidence. Rows whose source is
// Deleted are invariant corruption — normal lifecycle removes them atomically
// with the Deleted transition — so they refuse the delete with
// ErrReferenceInvariant instead of being silently ignored.
func (r *repositories) HasInboundProtectiveReference(ctx context.Context, target domain.ResourceID) (bool, error) {
	var protective, corrupted, missingStatus int64
	err := r.tx.QueryRow(ctx, `WITH inbound AS (
			SELECT d.source_id FROM resource_desired_references d WHERE d.target_id=$1
			UNION ALL
			SELECT a.source_id FROM resource_applied_references a WHERE a.target_id=$1
		)
		SELECT
			count(*)::bigint,
			count(*) FILTER (WHERE s.state = 'Deleted')::bigint,
			count(*) FILTER (WHERE s.resource_id IS NULL)::bigint
		FROM inbound i
		JOIN resources r ON r.id = i.source_id
		LEFT JOIN resource_statuses s ON s.resource_id = r.id`, string(target)).Scan(&protective, &corrupted, &missingStatus)
	if err != nil {
		return false, translateError(err)
	}
	if corrupted > 0 || missingStatus > 0 {
		return false, fmt.Errorf("%w: %d protective reference row(s) belong to a Deleted or status-less source", application.ErrReferenceInvariant, corrupted+missingStatus)
	}
	return protective > 0, nil
}

func (r *repositories) OutgoingReferenceTargets(ctx context.Context, source domain.ResourceID) ([]domain.ResourceID, error) {
	rows, err := r.tx.Query(ctx, `SELECT DISTINCT target_id FROM resource_desired_references WHERE source_id=$1 ORDER BY target_id`, string(source))
	if err != nil {
		return nil, translateError(err)
	}
	defer rows.Close()
	targets := []domain.ResourceID{}
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			return nil, translateError(err)
		}
		targets = append(targets, domain.ResourceID(target))
	}
	return targets, translateError(rows.Err())
}

func (r *repositories) RegisterDependencyWaits(ctx context.Context, operationID domain.OperationID, operationVersion uint64, targets map[domain.ResourceID]uint64) error {
	for target, targetVersion := range targets {
		if _, err := r.tx.Exec(ctx, `INSERT INTO operation_dependency_waits
				(operation_id, target_id, wait_seq, operation_version, registered_target_version)
				VALUES ($1,$2, nextval(pg_get_serial_sequence('operation_dependency_waits','wait_seq')), $3::numeric, $4::numeric)
				ON CONFLICT (operation_id, target_id) DO UPDATE SET
					operation_version = EXCLUDED.operation_version,
					registered_target_version = EXCLUDED.registered_target_version`,
			string(operationID), string(target), uintText(operationVersion), uintText(targetVersion)); err != nil {
			return translateError(err)
		}
	}
	return nil
}

func (r *repositories) DeleteDependencyWaitsForOperation(ctx context.Context, operationID domain.OperationID) error {
	_, err := r.tx.Exec(ctx, `DELETE FROM operation_dependency_waits WHERE operation_id=$1`, string(operationID))
	return translateError(err)
}

func (r *repositories) HasDependencyWaiterForTarget(ctx context.Context, target domain.ResourceID) (bool, error) {
	var exists bool
	err := r.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operation_dependency_waits WHERE target_id=$1)`, string(target)).Scan(&exists)
	return exists, translateError(err)
}

func (r *repositories) HasDependencyWaitsForOperation(ctx context.Context, operationID domain.OperationID) (bool, error) {
	var exists bool
	err := r.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operation_dependency_waits WHERE operation_id=$1)`, string(operationID)).Scan(&exists)
	return exists, translateError(err)
}

const dependencyWaitBatchSize = 256

func (r *repositories) PageDependencyWaitersByTarget(ctx context.Context, target domain.ResourceID, afterSequence uint64, limit int) ([]application.DependencyWait, uint64, error) {
	rows, err := r.tx.Query(ctx, `SELECT operation_id, target_id, wait_seq, operation_version::text, registered_target_version::text
		FROM operation_dependency_waits
		WHERE target_id=$1 AND wait_seq > $2::bigint
		ORDER BY wait_seq LIMIT $3`, string(target), int64(afterSequence), limit)
	if err != nil {
		return nil, 0, translateError(err)
	}
	defer rows.Close()
	waits := []application.DependencyWait{}
	next := uint64(0)
	for rows.Next() {
		var wait application.DependencyWait
		var operationID, targetID, operationVersionText, registeredTargetVersionText string
		if err := rows.Scan(&operationID, &targetID, &wait.WaitSequence, &operationVersionText, &registeredTargetVersionText); err != nil {
			return nil, 0, translateError(err)
		}
		wait.OperationID = domain.OperationID(operationID)
		wait.TargetID = domain.ResourceID(targetID)
		wait.OperationVersion, err = parseUint64(operationVersionText)
		if err != nil {
			return nil, 0, err
		}
		wait.RegisteredTargetVersion, err = parseUint64(registeredTargetVersionText)
		if err != nil {
			return nil, 0, err
		}
		waits = append(waits, wait)
		next = wait.WaitSequence
	}
	if err := rows.Err(); err != nil {
		return nil, 0, translateError(err)
	}
	hasMore, err := r.hasWaiterBeyond(ctx, target, next)
	if err != nil {
		return nil, 0, err
	}
	if !hasMore {
		next = 0
	}
	return waits, next, nil
}

func (r *repositories) hasWaiterBeyond(ctx context.Context, target domain.ResourceID, afterSequence uint64) (bool, error) {
	var exists bool
	err := r.tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operation_dependency_waits WHERE target_id=$1 AND wait_seq > $2::bigint)`,
		string(target), int64(afterSequence)).Scan(&exists)
	return exists, translateError(err)
}

// EnqueueWakeDependents coalesces behind any active wake for the same target.
// The bare ON CONFLICT DO NOTHING swallows both the versioned dedupe_key hit
// and the partial one-active-per-target index violation, which is exactly the
// M21 coalescing contract: the active wake's finalizer handshake observes the
// newer target version and schedules a follow-up.
func (r *repositories) EnqueueWakeDependents(ctx context.Context, message application.OutboxMessage) error {
	_, err := r.tx.Exec(ctx, `INSERT INTO outbox_messages
			(id, kind, operation_id, resource_id, attempt_number, dedupe_key, expected_version, sequence, payload_version, payload, state, available_at)
			VALUES ($1,$2,NULL,$3,NULL,$4,$5::numeric,NULL,$6,$7,'Pending',clock_timestamp())
			ON CONFLICT DO NOTHING`,
		message.ID, string(message.Kind), string(message.ResourceID), message.DedupeKey, uintText(message.ExpectedVersion),
		message.PayloadVersion, string(message.Payload))
	return translateError(err)
}
