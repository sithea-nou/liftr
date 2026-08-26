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
	row := r.tx.QueryRow(ctx, `SELECT id, resource_id, operation_seq::text, retry_of_operation_id, capability, target_generation::text, state, phase,
		requested_at_ns, started_at_ns, phase_changed_at_ns, completed_at_ns,
		failure_reason, failure_message, record_version::text
		FROM operations WHERE id=$1 FOR UPDATE`, id)
	record, err := scanOperation(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.OperationRecord{}, application.ErrOperationNotFound
	}
	if err != nil {
		return application.OperationRecord{}, translateError(err)
	}
	return record, nil
}

func (r *repositories) LookupOperation(ctx context.Context, id domain.OperationID) (application.OperationRecord, error) {
	row := r.tx.QueryRow(ctx, `SELECT id, resource_id, operation_seq::text, retry_of_operation_id, capability, target_generation::text, state, phase,
		requested_at_ns, started_at_ns, phase_changed_at_ns, completed_at_ns,
		failure_reason, failure_message, record_version::text
		FROM operations WHERE id=$1`, id)
	record, err := scanOperation(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.OperationRecord{}, application.ErrOperationNotFound
	}
	if err != nil {
		return application.OperationRecord{}, translateError(err)
	}
	return record, nil
}
func (r *repositories) ActiveForResource(ctx context.Context, id domain.ResourceID) (application.OperationRecord, bool, error) {
	row := r.tx.QueryRow(ctx, `SELECT id, resource_id, operation_seq::text, retry_of_operation_id, capability, target_generation::text, state, phase,
		requested_at_ns, started_at_ns, phase_changed_at_ns, completed_at_ns,
		failure_reason, failure_message, record_version::text
		FROM operations WHERE resource_id=$1 AND state IN ('Pending','Running') FOR UPDATE`, id)
	record, err := scanOperation(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return application.OperationRecord{}, false, nil
		}
		return application.OperationRecord{}, false, translateError(err)
	}
	return record, true, nil
}

// LatestForResource selects the newest inserted Operation for a Resource.
func (r *repositories) LatestForResource(ctx context.Context, id domain.ResourceID) (application.OperationRecord, bool, error) {
	row := r.tx.QueryRow(ctx, `SELECT id, resource_id, operation_seq::text, retry_of_operation_id, capability, target_generation::text, state, phase,
		requested_at_ns, started_at_ns, phase_changed_at_ns, completed_at_ns,
		failure_reason, failure_message, record_version::text
		FROM operations WHERE resource_id=$1 ORDER BY operations.operation_seq DESC LIMIT 1`, id)
	record, err := scanOperation(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return application.OperationRecord{}, false, nil
		}
		return application.OperationRecord{}, false, translateError(err)
	}
	return record, true, nil
}

func (r *repositories) PageForResource(ctx context.Context, id domain.ResourceID, beforeSequence uint64, limit int) (application.OperationPage, error) {
	if limit <= 0 {
		return application.OperationPage{}, fmt.Errorf("%w: operation page limit must be greater than zero", application.ErrInvalidApplicationCall)
	}
	rows, err := r.tx.Query(ctx, `SELECT id, resource_id, operation_seq::text, retry_of_operation_id, capability, target_generation::text, state, phase,
		requested_at_ns, started_at_ns, phase_changed_at_ns, completed_at_ns,
		failure_reason, failure_message, record_version::text
		FROM operations WHERE resource_id=$1 AND ($2::numeric=0 OR operation_seq < $2::numeric)
		ORDER BY operations.operation_seq DESC LIMIT $3`, id, uintText(beforeSequence), limit+1)
	if err != nil {
		return application.OperationPage{}, translateError(err)
	}
	defer rows.Close()
	records := make([]application.OperationRecord, 0, limit+1)
	for rows.Next() {
		record, err := scanOperation(rows)
		if err != nil {
			return application.OperationPage{}, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return application.OperationPage{}, translateError(err)
	}
	page := application.OperationPage{}
	if len(records) > limit {
		page.Records = records[:limit]
		page.NextSequence = page.Records[len(page.Records)-1].Sequence
	} else {
		page.Records = records
	}
	return page, nil
}

type operationScanner interface {
	Scan(...any) error
}

func scanOperation(row operationScanner) (application.OperationRecord, error) {
	var id domain.OperationID
	var resourceID domain.ResourceID
	var sequenceText, capability, targetText, state, phase string
	var retryOf *string
	var requestedNS, phaseChangedNS int64
	var startedNS, completedNS *int64
	var failureReason, failureMessage *string
	var versionText string
	// The raw scan error is preserved so GetOperation can distinguish a
	// missing row (pgx.ErrNoRows) before persistence translation.
	if err := row.Scan(&id, &resourceID, &sequenceText, &retryOf, &capability, &targetText, &state, &phase, &requestedNS, &startedNS, &phaseChangedNS, &completedNS, &failureReason, &failureMessage, &versionText); err != nil {
		return application.OperationRecord{}, err
	}
	return restoreOperation(id, resourceID, sequenceText, retryOf, capability, targetText, state, phase, requestedNS, startedNS, phaseChangedNS, completedNS, failureReason, failureMessage, versionText)
}

func restoreOperation(id domain.OperationID, resourceID domain.ResourceID, sequenceText string, retryOf *string, capability, targetText, state, phase string, requestedNS int64, startedNS *int64, phaseChangedNS int64, completedNS *int64, failureReason, failureMessage *string, versionText string) (application.OperationRecord, error) {
	sequence, err := parseUint64(sequenceText)
	if err != nil {
		return application.OperationRecord{}, err
	}
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
	if retryOf != nil {
		snapshot.RetryOfOperationID = domain.OperationID(*retryOf)
	}
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
	return application.OperationRecord{Operation: operation, Sequence: sequence, Version: version}, nil
}

func (r *repositories) CreateOperation(ctx context.Context, record application.OperationRecord) error {
	version := record.Version
	if version == 0 {
		version = 1
	}
	operation := record.Operation
	var retryOf any
	if operation.RetryOfOperationID() != "" {
		retryOf = operation.RetryOfOperationID()
	}
	failure, failed := operation.Failure()
	var failureReason, failureMessage any
	if failed {
		failureReason, failureMessage = failure.Reason(), failure.Message()
	}
	_, err := r.tx.Exec(ctx, `INSERT INTO operations
		(id,resource_id,retry_of_operation_id,capability,target_generation,state,phase,requested_at_ns,started_at_ns,phase_changed_at_ns,completed_at_ns,failure_reason,failure_message,record_version)
		VALUES ($1,$2,$3,$4,$5::numeric,$6,$7,$8,$9,$10,$11,$12,$13,$14::numeric)`, operation.ID(), operation.ResourceID(), retryOf, operation.Capability(), uintText(operation.TargetGeneration()),
		operation.State(), operation.Phase(), operation.RequestedAt().UnixNano(), nullableUnixNano(operation.StartedAt()), operation.PhaseChangedAt().UnixNano(), nullableUnixNano(operation.CompletedAt()),
		failureReason, failureMessage, uintText(version))
	return translateError(err)
}

func (r *repositories) SaveOperation(ctx context.Context, record application.OperationRecord, expectedVersion uint64) error {
	operation := record.Operation
	var retryOf any
	if operation.RetryOfOperationID() != "" {
		retryOf = operation.RetryOfOperationID()
	}
	failure, failed := operation.Failure()
	var failureReason, failureMessage any
	if failed {
		failureReason, failureMessage = failure.Reason(), failure.Message()
	}
	command, err := r.tx.Exec(ctx, `UPDATE operations SET state=$2, phase=$3, started_at_ns=$4, phase_changed_at_ns=$5,
		completed_at_ns=$6, failure_reason=$7, failure_message=$8, record_version=record_version+1,
		resource_id=$9, capability=$10, target_generation=$11::numeric, requested_at_ns=$12, retry_of_operation_id=$13
		WHERE id=$1 AND record_version=$14::numeric`, operation.ID(), operation.State(), operation.Phase(), nullableUnixNano(operation.StartedAt()), operation.PhaseChangedAt().UnixNano(),
		nullableUnixNano(operation.CompletedAt()), failureReason, failureMessage, operation.ResourceID(), operation.Capability(), uintText(operation.TargetGeneration()), operation.RequestedAt().UnixNano(), retryOf, uintText(expectedVersion))
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
	// The events table's data column records deliberately selected normalized
	// audit fields for admitted user mutations — the stable principal ID and
	// principal kind only. Access tokens, raw claims, and memberships never
	// reach this payload (ADR-0012).
	data := make(map[string]any, 2)
	if actor, present := event.Actor(); present {
		data["actor"] = map[string]string{"id": actor.ID, "kind": actor.Kind}
	}
	if admission, present := event.Admission(); present {
		data["admission"] = map[string]string{"policyRevision": admission.PolicyRevision}
	}
	var persistedData any
	if len(data) != 0 {
		persistedData = data
	}
	_, err := r.tx.Exec(ctx, `INSERT INTO events(id,resource_id,operation_id,generation,type,reason,message,occurred_at_ns,data)
		VALUES ($1,$2,$3,$4::numeric,$5,$6,$7,$8,$9)`, event.ID(), event.ResourceID(), operationID, uintText(event.Generation()), event.Type(), event.Reason(), event.Message(), event.OccurredAt().UnixNano(), persistedData)
	return translateError(err)
}

// GetIdempotency resolves one idempotency record inside the caller's
// per-principal scope. Records persisted before Milestone 11 carry the legacy
// "control-plane" scope; they are never matched by post-M11 lookups because a
// PrincipalID can never equal that value, so those anonymous-era rows are
// retired in place rather than migrated (ADR-0012).
func (r *repositories) GetIdempotency(ctx context.Context, scope, key string) (application.IdempotencyRecord, error) {
	// One advisory lock over the composite namespace serializes concurrent
	// admissions of the same scoped key.
	if _, err := r.tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, hashtextextended($2, 1)))`, scope, key); err != nil {
		return application.IdempotencyRecord{}, translateError(err)
	}
	var record application.IdempotencyRecord
	var fingerprint []byte
	err := r.tx.QueryRow(ctx, `SELECT idempotency_key,fingerprint,command_kind,resource_id,operation_id FROM idempotency_records
		WHERE scope=$1 AND idempotency_key=$2 FOR UPDATE`, scope, key).Scan(&record.Key, &fingerprint, &record.CommandKind, &record.ResourceID, &record.OperationID)
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
		VALUES ($1,$2,$3,$4,$5,$6)`, record.Scope, record.Key, record.CommandKind, []byte(record.Fingerprint), record.ResourceID, record.OperationID)
	return translateError(err)
}

func nullableUnixNano(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UnixNano()
}
