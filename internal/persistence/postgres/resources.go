// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
)

func (r *repositories) GetResource(ctx context.Context, id domain.ResourceID) (application.ResourceRecord, error) {
	var typeName, typeVersion, ownerKind, ownerID, generationText, versionText string
	var specVersion int
	var specBytes []byte
	var createdNS, updatedNS, statusUpdatedNS int64
	var observedText, state, provisionerRef string
	err := r.tx.QueryRow(ctx, `SELECT r.type_name, r.type_version, r.owner_kind, r.owner_id,
		r.generation::text, r.spec_codec_version, r.spec, r.record_version::text,
		r.created_at_ns, r.updated_at_ns, s.observed_generation::text, s.state,
		s.updated_at_ns, b.provisioner_ref
		FROM resources r
		JOIN resource_statuses s ON s.resource_id = r.id
		JOIN provisioner_bindings b ON b.resource_id = r.id
		WHERE r.id = $1 FOR UPDATE OF r, s, b`, id).Scan(
		&typeName, &typeVersion, &ownerKind, &ownerID, &generationText, &specVersion, &specBytes,
		&versionText, &createdNS, &updatedNS, &observedText, &state, &statusUpdatedNS, &provisionerRef,
	)
	if err != nil {
		return application.ResourceRecord{}, translateError(err)
	}
	spec, err := decodeResourceSpec(specVersion, specBytes)
	if err != nil {
		return application.ResourceRecord{}, err
	}
	generation, err := parseUint64(generationText)
	if err != nil {
		return application.ResourceRecord{}, err
	}
	version, err := parseUint64(versionText)
	if err != nil {
		return application.ResourceRecord{}, err
	}
	observedGeneration, err := parseUint64(observedText)
	if err != nil {
		return application.ResourceRecord{}, err
	}
	resource, err := domain.RestoreResource(domain.ResourceSnapshot{
		ID: id, Type: domain.ResourceTypeRef{Name: typeName, Version: typeVersion}, Owner: domain.OwnerRef{Kind: ownerKind, ID: ownerID},
		Generation: generation, Spec: spec, CreatedAt: time.Unix(0, createdNS).UTC(), UpdatedAt: time.Unix(0, updatedNS).UTC(),
	})
	if err != nil {
		return application.ResourceRecord{}, fmt.Errorf("restore resource %q: %w", id, err)
	}
	conditions, err := r.loadConditions(ctx, id)
	if err != nil {
		return application.ResourceRecord{}, err
	}
	status, err := domain.NewResourceStatus(id, observedGeneration, domain.ResourceState(state), conditions, time.Unix(0, statusUpdatedNS).UTC())
	if err != nil {
		return application.ResourceRecord{}, fmt.Errorf("restore resource status %q: %w", id, err)
	}
	ref, err := application.NewProvisionerRef(provisionerRef)
	if err != nil {
		return application.ResourceRecord{}, err
	}
	return application.ResourceRecord{Resource: resource, Status: status, ProvisionerRef: ref, Version: version}, nil
}

func (r *repositories) loadConditions(ctx context.Context, id domain.ResourceID) ([]domain.Condition, error) {
	rows, err := r.tx.Query(ctx, `SELECT type, status, reason, message, observed_generation::text, last_transition_at_ns
		FROM resource_conditions WHERE resource_id = $1 ORDER BY type`, id)
	if err != nil {
		return nil, translateError(err)
	}
	defer rows.Close()
	var result []domain.Condition
	for rows.Next() {
		var typeName, status, reason, message, observedText string
		var changedNS int64
		if err := rows.Scan(&typeName, &status, &reason, &message, &observedText, &changedNS); err != nil {
			return nil, err
		}
		observed, err := parseUint64(observedText)
		if err != nil {
			return nil, err
		}
		condition, err := domain.NewCondition(typeName, domain.ConditionStatus(status), reason, message, observed, time.Unix(0, changedNS).UTC())
		if err != nil {
			return nil, err
		}
		result = append(result, condition)
	}
	return result, rows.Err()
}

func (r *repositories) CreateResource(ctx context.Context, record application.ResourceRecord) error {
	if _, err := r.tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", record.Resource.ID()); err != nil {
		return translateError(err)
	}
	codecVersion, spec, err := encodeResourceSpec(record.Resource.Spec())
	if err != nil {
		return err
	}
	resourceType := record.Resource.Type()
	owner := record.Resource.Owner()
	version := record.Version
	if version == 0 {
		version = 1
	}
	_, err = r.tx.Exec(ctx, `INSERT INTO resources
		(id, type_name, type_version, owner_kind, owner_id, generation, spec_codec_version, spec, record_version, created_at_ns, updated_at_ns)
		VALUES ($1,$2,$3,$4,$5,$6::numeric,$7,$8,$9::numeric,$10,$11)`,
		record.Resource.ID(), resourceType.Name, resourceType.Version, owner.Kind, owner.ID, uintText(record.Resource.Generation()),
		codecVersion, spec, uintText(version), record.Resource.CreatedAt().UnixNano(), record.Resource.UpdatedAt().UnixNano())
	if err != nil {
		return translateError(err)
	}
	if _, err := r.tx.Exec(ctx, `INSERT INTO resource_statuses(resource_id, observed_generation, state, updated_at_ns)
		VALUES ($1,$2::numeric,$3,$4)`, record.Resource.ID(), uintText(record.Status.ObservedGeneration()), record.Status.State(), record.Status.UpdatedAt().UnixNano()); err != nil {
		return translateError(err)
	}
	if err := r.insertConditions(ctx, record.Status); err != nil {
		return err
	}
	_, err = r.tx.Exec(ctx, `INSERT INTO provisioner_bindings(resource_id, provisioner_ref) VALUES ($1,$2)`, record.Resource.ID(), record.ProvisionerRef)
	return translateError(err)
}

func (r *repositories) SaveResource(ctx context.Context, record application.ResourceRecord, expectedVersion uint64) error {
	// Ownership is fixed at creation and every authorization decision keys on
	// the stored owner, so an update carrying a different owner fails closed
	// instead of being silently ignored (ADR-0016). The row is already locked
	// by GetResource on mutation paths; the explicit comparison keeps the
	// guarantee for any caller.
	var storedKind, storedID string
	if err := r.tx.QueryRow(ctx, `SELECT owner_kind, owner_id FROM resources WHERE id=$1 FOR UPDATE`,
		record.Resource.ID()).Scan(&storedKind, &storedID); err != nil {
		return translateError(err)
	}
	if storedKind != record.Resource.Owner().Kind || storedID != record.Resource.Owner().ID {
		return fmt.Errorf("%w: resource owner is immutable", application.ErrInvalidApplicationCall)
	}
	codecVersion, spec, err := encodeResourceSpec(record.Resource.Spec())
	if err != nil {
		return err
	}
	command, err := r.tx.Exec(ctx, `UPDATE resources SET generation=$2::numeric, spec_codec_version=$3, spec=$4,
		record_version=record_version+1, updated_at_ns=$5
		WHERE id=$1 AND record_version=$6::numeric`, record.Resource.ID(), uintText(record.Resource.Generation()), codecVersion, spec,
		record.Resource.UpdatedAt().UnixNano(), uintText(expectedVersion))
	if err != nil {
		return translateError(err)
	}
	if command.RowsAffected() != 1 {
		return application.ErrConcurrencyConflict
	}
	if _, err := r.tx.Exec(ctx, `UPDATE resource_statuses SET observed_generation=$2::numeric, state=$3, updated_at_ns=$4 WHERE resource_id=$1`,
		record.Resource.ID(), uintText(record.Status.ObservedGeneration()), record.Status.State(), record.Status.UpdatedAt().UnixNano()); err != nil {
		return translateError(err)
	}
	if _, err := r.tx.Exec(ctx, "DELETE FROM resource_conditions WHERE resource_id=$1", record.Resource.ID()); err != nil {
		return translateError(err)
	}
	return r.insertConditions(ctx, record.Status)
}

func (r *repositories) insertConditions(ctx context.Context, status domain.ResourceStatus) error {
	for _, condition := range status.Conditions() {
		if _, err := r.tx.Exec(ctx, `INSERT INTO resource_conditions
			(resource_id,type,status,reason,message,observed_generation,last_transition_at_ns)
			VALUES ($1,$2,$3,$4,$5,$6::numeric,$7)`, status.ResourceID(), condition.Type(), condition.Status(), condition.Reason(),
			condition.Message(), uintText(condition.ObservedGeneration()), condition.LastTransitionAt().UnixNano()); err != nil {
			return translateError(err)
		}
	}
	return nil
}

func parseUint64(value string) (uint64, error) {
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("decode uint64 %q: %w", value, err)
	}
	return parsed, nil
}

func uintText(value uint64) string { return strconv.FormatUint(value, 10) }
