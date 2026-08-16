// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
)

func (r *repositories) GetOperation(ctx context.Context, id domain.OperationID) (application.OperationRecord, error) {
	row := r.tx.QueryRow(ctx, `SELECT resource_id, capability, target_generation::text, state, phase,
		requested_at_ns, started_at_ns, phase_changed_at_ns, completed_at_ns,
		failure_reason, failure_message, record_version::text
		FROM operations WHERE id=$1 FOR UPDATE`, id)
	return scanOperation(id, row)
}

func (r *repositories) ActiveForResource(ctx context.Context, id domain.ResourceID) (application.OperationRecord, bool, error) {
	row := r.tx.QueryRow(ctx, `SELECT id, capability, target_generation::text, state, phase,
		requested_at_ns, started_at_ns, phase_changed_at_ns, completed_at_ns,
		failure_reason, failure_message, record_version::text
		FROM operations WHERE resource_id=$1 AND state IN ('Pending','Running') FOR UPDATE`, id)
	var operationID domain.OperationID
	var capability, targetText, state, phase string
	var requestedNS, phaseChangedNS int64
	var startedNS, completedNS *int64
	var failureReason, failureMessage *string
	var versionText string
	if err := row.Scan(&operationID, &capability, &targetText, &state, &phase, &requestedNS, &startedNS, &phaseChangedNS, &completedNS, &failureReason, &failureMessage, &versionText); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return application.OperationRecord{}, false, nil
		}
		return application.OperationRecord{}, false, translateError(err)
	}
	record, err := restoreOperation(operationID, id, capability, targetText, state, phase, requestedNS, startedNS, phaseChangedNS, completedNS, failureReason, failureMessage, versionText)
	return record, err == nil, err
}

func scanOperation(id domain.OperationID, row pgx.Row) (application.OperationRecord, error) {
	var resourceID domain.ResourceID
	var capability, targetText, state, phase string
	var requestedNS, phaseChangedNS int64
	var startedNS, completedNS *int64
	var failureReason, failureMessage *string
	var versionText string
	if err := row.Scan(&resourceID, &capability, &targetText, &state, &phase, &requestedNS, &startedNS, &phaseChangedNS, &completedNS, &failureReason, &failureMessage, &versionText); err != nil {
		return application.OperationRecord{}, translateError(err)
	}
	return restoreOperation(id, resourceID, capability, targetText, state, phase, requestedNS, startedNS, phaseChangedNS, completedNS, failureReason, failureMessage, versionText)
}

func restoreOperation(id domain.OperationID, resourceID domain.ResourceID, capability, targetText, state, phase string, requestedNS int64, startedNS *int64, phaseChangedNS int64, completedNS *int64, failureReason, failureMessage *string, versionText string) (application.OperationRecord, error) {
	target, err := parseUint64(targetText)
	if err != nil {
		return application.OperationRecord{}, err
	}
	version, err := parseUint64(versionText)
	if err != nil {
		return application.OperationRecord{}, err
	}
	snapshot := domain.OperationSnapshot{ID: id, ResourceID: resourceID, Capability: domain.Capability(capability), TargetGeneration: target,
		State: domain.OperationState(state), Phase: domain.OperationPhase(phase), RequestedAt: time.Unix(0, requestedNS).UTC(), PhaseChangedAt: time.Unix(0, phaseChangedNS).UTC()}
	if startedNS != nil {
		snapshot.StartedAt = time.Unix(0, *startedNS).UTC()
	}
	if completedNS != nil {
		snapshot.CompletedAt = time.Unix(0, *completedNS).UTC()
	}
	if failureReason != nil {
		snapshot.FailureReason = *failureReason
	}
	if failureMessage != nil {
		snapshot.FailureMessage = *failureMessage
	}
	operation, err := domain.RestoreOperation(snapshot)
	if err != nil {
		return application.OperationRecord{}, fmt.Errorf("restore operation %q: %w", id, err)
	}
	return application.OperationRecord{Operation: operation, Version: version}, nil
}

func (r *repositories) CreateOperation(ctx context.Context, record application.OperationRecord) error {
	version := record.Version
	if version == 0 {
		version = 1
	}
	operation := record.Operation
	failure, failed := operation.Failure()
	var failureReason, failureMessage any
	if failed {
		failureReason, failureMessage = failure.Reason(), failure.Message()
	}
	_, err := r.tx.Exec(ctx, `INSERT INTO operations
		(id,resource_id,capability,target_generation,state,phase,requested_at_ns,started_at_ns,phase_changed_at_ns,completed_at_ns,failure_reason,failure_message,record_version)
		VALUES ($1,$2,$3,$4::numeric,$5,$6,$7,$8,$9,$10,$11,$12,$13::numeric)`, operation.ID(), operation.ResourceID(), operation.Capability(), uintText(operation.TargetGeneration()),
		operation.State(), operation.Phase(), operation.RequestedAt().UnixNano(), nullableUnixNano(operation.StartedAt()), operation.PhaseChangedAt().UnixNano(), nullableUnixNano(operation.CompletedAt()),
		failureReason, failureMessage, uintText(version))
	return translateError(err)
}

func (r *repositories) SaveOperation(ctx context.Context, record application.OperationRecord, expectedVersion uint64) error {
	operation := record.Operation
	failure, failed := operation.Failure()
	var failureReason, failureMessage any
	if failed {
		failureReason, failureMessage = failure.Reason(), failure.Message()
	}
	command, err := r.tx.Exec(ctx, `UPDATE operations SET state=$2, phase=$3, started_at_ns=$4, phase_changed_at_ns=$5,
		completed_at_ns=$6, failure_reason=$7, failure_message=$8, record_version=record_version+1
		WHERE id=$1 AND record_version=$9::numeric`, operation.ID(), operation.State(), operation.Phase(), nullableUnixNano(operation.StartedAt()), operation.PhaseChangedAt().UnixNano(),
		nullableUnixNano(operation.CompletedAt()), failureReason, failureMessage, uintText(expectedVersion))
	if err != nil {
		return translateError(err)
	}
	if command.RowsAffected() != 1 {
		return application.ErrConcurrencyConflict
	}
	return nil
}

func (r *repositories) Append(ctx context.Context, event domain.Event) error {
	var operationID any
	if event.OperationID() != "" {
		operationID = event.OperationID()
	}
	_, err := r.tx.Exec(ctx, `INSERT INTO events(id,resource_id,operation_id,generation,type,reason,message,occurred_at_ns)
		VALUES ($1,$2,$3,$4::numeric,$5,$6,$7,$8)`, event.ID(), event.ResourceID(), operationID, uintText(event.Generation()), event.Type(), event.Reason(), event.Message(), event.OccurredAt().UnixNano())
	return translateError(err)
}

func (r *repositories) GetIdempotency(ctx context.Context, key string) (application.IdempotencyRecord, error) {
	if _, err := r.tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 1))", key); err != nil {
		return application.IdempotencyRecord{}, translateError(err)
	}
	var record application.IdempotencyRecord
	var fingerprint []byte
	err := r.tx.QueryRow(ctx, `SELECT idempotency_key,fingerprint,command_kind,resource_id,operation_id FROM idempotency_records
		WHERE scope='control-plane' AND idempotency_key=$1 FOR UPDATE`, key).Scan(&record.Key, &fingerprint, &record.CommandKind, &record.ResourceID, &record.OperationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.IdempotencyRecord{}, application.ErrIdempotencyNotFound
	}
	if err != nil {
		return application.IdempotencyRecord{}, translateError(err)
	}
	record.Fingerprint = string(fingerprint)
	return record, nil
}

func (r *repositories) PutIdempotency(ctx context.Context, record application.IdempotencyRecord) error {
	_, err := r.tx.Exec(ctx, `INSERT INTO idempotency_records(scope,idempotency_key,command_kind,fingerprint,resource_id,operation_id)
		VALUES ('control-plane',$1,$2,$3,$4,$5)`, record.Key, record.CommandKind, []byte(record.Fingerprint), record.ResourceID, record.OperationID)
	return translateError(err)
}

func nullableUnixNano(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UnixNano()
}
