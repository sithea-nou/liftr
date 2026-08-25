// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning/opentofu"
)

var _ opentofu.EvidenceStore = (*Store)(nil)

func (s *Store) PrepareAttempt(ctx context.Context, key opentofu.AttemptKey, fence opentofu.LeaseFence) (opentofu.AttemptEvidence, error) {
	if err := validateEvidenceInputs(key, fence); err != nil {
		return opentofu.AttemptEvidence{}, err
	}
	tx, err := s.beginEvidenceTx(ctx)
	if err != nil {
		return opentofu.AttemptEvidence{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := validateOpenTofuFence(ctx, tx, key, fence, false); err != nil {
		return opentofu.AttemptEvidence{}, err
	}
	row := tx.QueryRow(ctx, `INSERT INTO opentofu_attempt_evidence
		(resource_id,operation_id,attempt_number,provisioner_ref,phase,record_version)
		VALUES ($1,$2,$3::numeric,$4,'Prepared',1)
		RETURNING resource_id,operation_id,attempt_number::text,provisioner_ref,phase,record_version::text,created_at,updated_at`,
		key.ResourceID, key.OperationID, uintText(key.AttemptNumber), key.ProvisionerRef)
	record, err := scanOpenTofuAttempt(row)
	if err != nil {
		return opentofu.AttemptEvidence{}, translateOpenTofuEvidenceError(err)
	}
	if err := commitEvidenceTx(ctx, tx); err != nil {
		return opentofu.AttemptEvidence{}, err
	}
	return record, nil
}

func (s *Store) LoadAttempt(ctx context.Context, key opentofu.AttemptKey) (opentofu.AttemptEvidence, error) {
	if err := key.Validate(); err != nil {
		return opentofu.AttemptEvidence{}, err
	}
	tx, err := s.beginEvidenceTx(ctx)
	if err != nil {
		return opentofu.AttemptEvidence{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	record, err := scanOpenTofuAttempt(tx.QueryRow(ctx, `SELECT resource_id,operation_id,attempt_number::text,
		provisioner_ref,phase,record_version::text,created_at,updated_at
		FROM opentofu_attempt_evidence
		WHERE resource_id=$1 AND operation_id=$2 AND attempt_number=$3::numeric AND provisioner_ref=$4`,
		key.ResourceID, key.OperationID, uintText(key.AttemptNumber), key.ProvisionerRef))
	if err != nil {
		return opentofu.AttemptEvidence{}, translateOpenTofuEvidenceError(err)
	}
	if err := commitEvidenceTx(ctx, tx); err != nil {
		return opentofu.AttemptEvidence{}, err
	}
	return record, nil
}

func (s *Store) AdvanceAttempt(ctx context.Context, key opentofu.AttemptKey, fence opentofu.LeaseFence, expectedPhase opentofu.AttemptPhase, expectedVersion uint64, next opentofu.AttemptPhase) (opentofu.AttemptEvidence, error) {
	if err := validateEvidenceInputs(key, fence); err != nil {
		return opentofu.AttemptEvidence{}, err
	}
	if expectedVersion == 0 || !expectedPhase.Valid() || !expectedPhase.CanAdvanceTo(next) {
		return opentofu.AttemptEvidence{}, opentofu.ErrEvidenceConflict
	}
	tx, err := s.beginEvidenceTx(ctx)
	if err != nil {
		return opentofu.AttemptEvidence{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	allowObserve := next == opentofu.AttemptApplyOutcomeUnknown || next == opentofu.AttemptObservedConverged
	if err := validateOpenTofuFence(ctx, tx, key, fence, allowObserve); err != nil {
		return opentofu.AttemptEvidence{}, err
	}
	record, err := scanOpenTofuAttempt(tx.QueryRow(ctx, `UPDATE opentofu_attempt_evidence
		SET phase=$5,record_version=record_version+1,updated_at=clock_timestamp()
		WHERE resource_id=$1 AND operation_id=$2 AND attempt_number=$3::numeric AND provisioner_ref=$4
		  AND phase=$6 AND record_version=$7::numeric
		RETURNING resource_id,operation_id,attempt_number::text,provisioner_ref,phase,record_version::text,created_at,updated_at`,
		key.ResourceID, key.OperationID, uintText(key.AttemptNumber), key.ProvisionerRef, next, expectedPhase, uintText(expectedVersion)))
	if errors.Is(err, pgx.ErrNoRows) {
		return opentofu.AttemptEvidence{}, opentofu.ErrEvidenceConflict
	}
	if err != nil {
		return opentofu.AttemptEvidence{}, translateOpenTofuEvidenceError(err)
	}
	if err := commitEvidenceTx(ctx, tx); err != nil {
		return opentofu.AttemptEvidence{}, err
	}
	return record, nil
}

func (s *Store) CreateStateBinding(ctx context.Context, key opentofu.AttemptKey, fence opentofu.LeaseFence, identity opentofu.StateBindingIdentity) (opentofu.StateBinding, error) {
	if err := validateEvidenceInputs(key, fence); err != nil {
		return opentofu.StateBinding{}, err
	}
	if err := identity.Validate(); err != nil {
		return opentofu.StateBinding{}, err
	}
	if identity.ResourceID != key.ResourceID || identity.ProvisionerRef != key.ProvisionerRef {
		return opentofu.StateBinding{}, fmt.Errorf("%w: binding and attempt identities differ", opentofu.ErrInvalidEvidence)
	}
	tx, err := s.beginEvidenceTx(ctx)
	if err != nil {
		return opentofu.StateBinding{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := validateOpenTofuFence(ctx, tx, key, fence, true); err != nil {
		return opentofu.StateBinding{}, err
	}
	record, err := scanOpenTofuStateBinding(tx.QueryRow(ctx, `INSERT INTO opentofu_state_bindings
		(resource_id,provisioner_ref,engine,program,backend,state_key,record_version)
		SELECT $1,$2,$3,$4,$5,$6,1
		WHERE EXISTS (SELECT 1 FROM opentofu_attempt_evidence
		  WHERE resource_id=$1 AND operation_id=$7 AND attempt_number=$8::numeric AND provisioner_ref=$2 AND phase='Prepared')
		RETURNING resource_id,provisioner_ref,engine,program,backend,state_key,lineage,serial::text,state_digest,
			record_version::text,created_at,updated_at`, identity.ResourceID, identity.ProvisionerRef,
		identity.Engine, identity.Program, identity.Backend, identity.StateKey, key.OperationID, uintText(key.AttemptNumber)))
	if errors.Is(err, pgx.ErrNoRows) {
		return opentofu.StateBinding{}, opentofu.ErrEvidenceConflict
	}
	if err != nil {
		return opentofu.StateBinding{}, translateOpenTofuEvidenceError(err)
	}
	if err := commitEvidenceTx(ctx, tx); err != nil {
		return opentofu.StateBinding{}, err
	}
	return record, nil
}

func (s *Store) LoadStateBinding(ctx context.Context, resourceID domain.ResourceID) (opentofu.StateBinding, error) {
	if resourceID == "" {
		return opentofu.StateBinding{}, fmt.Errorf("%w: resource ID is required", opentofu.ErrInvalidEvidence)
	}
	tx, err := s.beginEvidenceTx(ctx)
	if err != nil {
		return opentofu.StateBinding{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	record, err := scanOpenTofuStateBinding(tx.QueryRow(ctx, `SELECT resource_id,provisioner_ref,engine,program,backend,
		state_key,lineage,serial::text,state_digest,record_version::text,created_at,updated_at
		FROM opentofu_state_bindings WHERE resource_id=$1`, resourceID))
	if err != nil {
		return opentofu.StateBinding{}, translateOpenTofuEvidenceError(err)
	}
	if err := commitEvidenceTx(ctx, tx); err != nil {
		return opentofu.StateBinding{}, err
	}
	return record, nil
}

func (s *Store) UpdateState(ctx context.Context, key opentofu.AttemptKey, fence opentofu.LeaseFence, expectedVersion uint64, state opentofu.StateEvidence) (opentofu.StateBinding, error) {
	if err := validateEvidenceInputs(key, fence); err != nil {
		return opentofu.StateBinding{}, err
	}
	if expectedVersion == 0 {
		return opentofu.StateBinding{}, opentofu.ErrEvidenceConflict
	}
	if err := state.Validate(); err != nil {
		return opentofu.StateBinding{}, err
	}
	tx, err := s.beginEvidenceTx(ctx)
	if err != nil {
		return opentofu.StateBinding{}, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := validateOpenTofuFence(ctx, tx, key, fence, true); err != nil {
		return opentofu.StateBinding{}, err
	}
	record, err := scanOpenTofuStateBinding(tx.QueryRow(ctx, `UPDATE opentofu_state_bindings
		SET lineage=$3,serial=$4::numeric,state_digest=$5,record_version=record_version+1,updated_at=clock_timestamp()
		WHERE resource_id=$1 AND provisioner_ref=$2 AND record_version=$6::numeric
		  AND EXISTS (SELECT 1 FROM opentofu_attempt_evidence
		    WHERE resource_id=$1 AND operation_id=$7 AND attempt_number=$8::numeric AND provisioner_ref=$2 AND phase <> 'Prepared')
		RETURNING resource_id,provisioner_ref,engine,program,backend,state_key,lineage,serial::text,state_digest,
			record_version::text,created_at,updated_at`, key.ResourceID, key.ProvisionerRef, state.Lineage,
		uintText(state.Serial), state.Digest[:], uintText(expectedVersion), key.OperationID, uintText(key.AttemptNumber)))
	if errors.Is(err, pgx.ErrNoRows) {
		return opentofu.StateBinding{}, opentofu.ErrEvidenceConflict
	}
	if err != nil {
		return opentofu.StateBinding{}, translateOpenTofuEvidenceError(err)
	}
	if err := commitEvidenceTx(ctx, tx); err != nil {
		return opentofu.StateBinding{}, err
	}
	return record, nil
}

func validateEvidenceInputs(key opentofu.AttemptKey, fence opentofu.LeaseFence) error {
	if err := key.Validate(); err != nil {
		return err
	}
	return fence.Validate()
}

func (s *Store) beginEvidenceTx(ctx context.Context) (pgx.Tx, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("begin OpenTofu evidence transaction: %w", err)
	}
	return tx, nil
}

func commitEvidenceTx(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Commit(ctx); err != nil {
		return translateOpenTofuEvidenceError(err)
	}
	return nil
}

func validateOpenTofuFence(ctx context.Context, tx pgx.Tx, key opentofu.AttemptKey, fence opentofu.LeaseFence, allowObserve bool) error {
	var found string
	err := tx.QueryRow(ctx, `SELECT message.id
		FROM outbox_messages AS message
		JOIN provisioning_executions AS execution ON execution.operation_id=message.operation_id
		JOIN provisioning_submission_attempts AS attempt
		  ON attempt.operation_id=$2 AND attempt.attempt_number=$3::numeric
		JOIN provisioning_executions AS source_execution ON source_execution.operation_id=$2
		WHERE message.id=$5 AND message.state='Leased' AND message.lease_token=$6
		  AND message.leased_until > clock_timestamp()
		  AND source_execution.resource_id=$1 AND source_execution.provisioner_ref=$4
		  AND source_execution.current_attempt_number=$3::numeric
		  AND (
		    (message.kind='Dispatch' AND message.operation_id=$2 AND message.attempt_number=$3::numeric
		      AND attempt.dispatch_message_id=message.id) OR
		    ($7 AND message.kind='Observe' AND message.attempt_number IS NULL AND (
		      (message.operation_id=$2 AND execution.current_attempt_number=$3::numeric) OR
		      (execution.recovery_source_operation_id=$2 AND execution.recovery_source_attempt=$3::numeric)
		    ))
		  )
		  AND execution.resource_id=$1 AND execution.provisioner_ref=$4
		FOR UPDATE OF message`, key.ResourceID, key.OperationID, uintText(key.AttemptNumber), key.ProvisionerRef,
		fence.MessageID, fence.Token, allowObserve).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return opentofu.ErrFenceRejected
	}
	if err != nil {
		return translateOpenTofuEvidenceError(err)
	}
	return nil
}

func scanOpenTofuAttempt(row rowScanner) (opentofu.AttemptEvidence, error) {
	var record opentofu.AttemptEvidence
	var attempt, version string
	if err := row.Scan(&record.Key.ResourceID, &record.Key.OperationID, &attempt, &record.Key.ProvisionerRef,
		&record.Phase, &version, &record.CreatedAt, &record.UpdatedAt); err != nil {
		return opentofu.AttemptEvidence{}, err
	}
	var err error
	if record.Key.AttemptNumber, err = parseUint64(attempt); err != nil {
		return opentofu.AttemptEvidence{}, err
	}
	if record.Version, err = parseUint64(version); err != nil {
		return opentofu.AttemptEvidence{}, err
	}
	if !record.Phase.Valid() {
		return opentofu.AttemptEvidence{}, fmt.Errorf("invalid persisted OpenTofu attempt phase %q", record.Phase)
	}
	return record, nil
}

func scanOpenTofuStateBinding(row rowScanner) (opentofu.StateBinding, error) {
	var record opentofu.StateBinding
	var lineage, serial *string
	var digest []byte
	var version string
	if err := row.Scan(&record.Identity.ResourceID, &record.Identity.ProvisionerRef, &record.Identity.Engine,
		&record.Identity.Program, &record.Identity.Backend, &record.Identity.StateKey, &lineage, &serial, &digest,
		&version, &record.CreatedAt, &record.UpdatedAt); err != nil {
		return opentofu.StateBinding{}, err
	}
	var err error
	if record.Version, err = parseUint64(version); err != nil {
		return opentofu.StateBinding{}, err
	}
	if lineage != nil {
		if serial == nil || len(digest) != len(opentofu.StateDigest{}) {
			return opentofu.StateBinding{}, fmt.Errorf("invalid persisted OpenTofu state evidence")
		}
		state := opentofu.StateEvidence{Lineage: *lineage}
		if state.Serial, err = parseUint64(*serial); err != nil {
			return opentofu.StateBinding{}, err
		}
		copy(state.Digest[:], digest)
		record.State = &state
	}
	return record, nil
}

func translateOpenTofuEvidenceError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return opentofu.ErrEvidenceNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505", "23503", "23514", "23000":
			return fmt.Errorf("%w: %s", opentofu.ErrEvidenceConflict, pgErr.Message)
		case "40001", "40P01":
			return fmt.Errorf("OpenTofu evidence transaction retry required: %w", err)
		}
	}
	return err
}
