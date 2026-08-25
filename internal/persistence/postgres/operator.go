// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
	"github.com/sithea-nou/liftr/internal/provisioning/opentofu"
)

// operatorIdempotencyLockNamespace keeps the operator key lock disjoint from
// the developer idempotency lock, which hashes (scope,key) with inner salt 1
// over the raw scope. Prefixing the scope with a versioned namespace plus the
// salt-7 inner hash makes collisions across planes structurally impossible.
const operatorIdempotencyLockNamespace = "liftr/operator-idempotency/v1"

// StateIdentity returns the deliberately curated portion of private OpenTofu
// evidence. A Resource without an OpenTofu binding simply has no state
// identity section in operator diagnostics.
func (s *Store) StateIdentity(ctx context.Context, resourceID domain.ResourceID) (application.StateIdentitySummary, bool, error) {
	binding, err := s.LoadStateBinding(ctx, resourceID)
	if errors.Is(err, opentofu.ErrEvidenceNotFound) {
		return application.StateIdentitySummary{}, false, nil
	}
	if err != nil {
		return application.StateIdentitySummary{}, false, err
	}
	return stateIdentitySummary(binding), true, nil
}

func (r *repositories) StateIdentity(ctx context.Context, resourceID domain.ResourceID) (application.StateIdentitySummary, bool, error) {
	binding, err := scanOpenTofuStateBinding(r.tx.QueryRow(ctx, `SELECT resource_id,provisioner_ref,engine,program,backend,
		state_key,lineage,serial::text,state_digest,record_version::text,created_at,updated_at
		FROM opentofu_state_bindings WHERE resource_id=$1`, resourceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return application.StateIdentitySummary{}, false, nil
	}
	if err != nil {
		return application.StateIdentitySummary{}, false, translateOpenTofuEvidenceError(err)
	}
	return stateIdentitySummary(binding), true, nil
}

func stateIdentitySummary(binding opentofu.StateBinding) application.StateIdentitySummary {
	summary := application.StateIdentitySummary{
		ProvisionerRef: string(binding.Identity.ProvisionerRef),
		Engine:         binding.Identity.Engine, Program: binding.Identity.Program,
		Backend: binding.Identity.Backend, StateKey: binding.Identity.StateKey,
		Version: binding.Version,
	}
	if binding.State != nil {
		summary.LineagePresent = binding.State.Lineage != ""
		summary.Serial = binding.State.Serial
		summary.DigestPrefix = hex.EncodeToString(binding.State.Digest[:8])
	}
	return summary
}

// SpecDigest hashes the exact versioned bytes stored for desired state. This
// binds diagnostics to intent content without decoding or returning the spec.
func (s *Store) SpecDigest(ctx context.Context, resourceID domain.ResourceID) (string, bool, error) {
	var spec []byte
	err := s.pool.QueryRow(ctx, `SELECT spec FROM resources WHERE id=$1`, resourceID).Scan(&spec)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, translateError(err)
	}
	digest := sha256.Sum256(spec)
	return hex.EncodeToString(digest[:]), true, nil
}

func (r *repositories) SpecDigest(ctx context.Context, resourceID domain.ResourceID) (string, bool, error) {
	var spec []byte
	err := r.tx.QueryRow(ctx, `SELECT spec FROM resources WHERE id=$1`, resourceID).Scan(&spec)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, translateError(err)
	}
	digest := sha256.Sum256(spec)
	return hex.EncodeToString(digest[:]), true, nil
}

// GetOperatorAction loads one immutable accepted operator action.
func (r *repositories) GetOperatorAction(ctx context.Context, id string) (application.OperatorActionRecord, error) {
	row := r.tx.QueryRow(ctx, `SELECT id,actor_principal_id,actor_kind,action,target_kind,target_id,
		source_work_id,created_work_id,idempotency_digest,request_id,created_at
		FROM operator_actions WHERE id=$1 FOR UPDATE`, id)
	return scanOperatorAction(row)
}

// InsertOperatorAction appends one accepted operator mutation. Updates and
// deletes are rejected by trigger; the insert itself is append-only by
// construction.
func (r *repositories) InsertOperatorAction(ctx context.Context, record application.OperatorActionRecord) error {
	var sourceWorkID any
	if record.SourceWorkID != "" {
		sourceWorkID = record.SourceWorkID
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	_, err := r.tx.Exec(ctx, `INSERT INTO operator_actions(id,actor_principal_id,actor_kind,action,target_kind,target_id,
		source_work_id,created_work_id,idempotency_digest,request_id,created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		record.ID, string(record.ActorPrincipalID), string(record.ActorKind), string(record.Action),
		string(record.TargetKind), record.TargetID, sourceWorkID, record.CreatedWorkID,
		record.IdempotencyDigest, record.RequestID, record.CreatedAt)
	return translateError(err)
}

func scanOperatorAction(row rowScanner) (application.OperatorActionRecord, error) {
	var record application.OperatorActionRecord
	var actorPrincipalID, actorKind, action, targetKind string
	var sourceWorkID *string
	var digest []byte
	err := row.Scan(&record.ID, &actorPrincipalID, &actorKind, &action, &targetKind, &record.TargetID,
		&sourceWorkID, &record.CreatedWorkID, &digest, &record.RequestID, &record.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.OperatorActionRecord{}, application.ErrResourceNotFound
	}
	if err != nil {
		return application.OperatorActionRecord{}, translateError(err)
	}
	record.ActorPrincipalID = identity.PrincipalID(actorPrincipalID)
	record.ActorKind = identity.PrincipalKind(actorKind)
	record.Action = application.OperatorAuditAction(action)
	record.TargetKind = identity.OperatorTargetKind(targetKind)
	if sourceWorkID != nil {
		record.SourceWorkID = *sourceWorkID
	}
	record.IdempotencyDigest = digest
	return record, nil
}

// GetOperatorIdempotency serializes concurrent same-key mutations through one
// transaction-scoped advisory lock before reading the bound record.
func (r *repositories) GetOperatorIdempotency(ctx context.Context, scope, key string) (application.OperatorIdempotencyRecord, error) {
	if _, err := r.tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, hashtextextended($2, 7)))`,
		operatorIdempotencyLockNamespace+":"+scope, key); err != nil {
		return application.OperatorIdempotencyRecord{}, translateError(err)
	}
	row := r.tx.QueryRow(ctx, `SELECT scope,key,fingerprint,operator_action_id FROM operator_idempotency
		WHERE scope=$1 AND key=$2 FOR UPDATE`, scope, key)
	var record application.OperatorIdempotencyRecord
	err := row.Scan(&record.Scope, &record.Key, &record.Fingerprint, &record.OperatorActionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.OperatorIdempotencyRecord{}, application.ErrOperatorIdempotencyNotFound
	}
	if err != nil {
		return application.OperatorIdempotencyRecord{}, translateError(err)
	}
	return record, nil
}

func (r *repositories) PutOperatorIdempotency(ctx context.Context, record application.OperatorIdempotencyRecord) error {
	_, err := r.tx.Exec(ctx, `INSERT INTO operator_idempotency(scope,key,fingerprint,operator_action_id)
		VALUES ($1,$2,$3,$4)`, record.Scope, record.Key, record.Fingerprint, record.OperatorActionID)
	return translateError(err)
}
