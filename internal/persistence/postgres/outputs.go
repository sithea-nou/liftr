// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
)

// SaveResourceOutputs inserts one immutable output snapshot. Republication
// of identical provenance and content is idempotent; any contradiction for
// the same Resource/generation pair fails closed.
func (r *repositories) SaveResourceOutputs(ctx context.Context, record application.ResourceOutputRecord) error {
	values, err := json.Marshal(record.Values.Values())
	if err != nil {
		return fmt.Errorf("encode resource output values: %w", err)
	}
	inserted, err := r.tx.Exec(ctx, `INSERT INTO resource_outputs
		(resource_id, observed_generation, operation_id, capability, output_mapping_ref, output_contract_digest, values_jsonb, values_digest, published_at_ns)
		VALUES ($1,$2::numeric,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (resource_id, observed_generation) DO NOTHING`,
		record.ResourceID, uintText(record.ObservedGeneration), record.OperationID, record.Capability,
		record.OutputMappingRef, record.OutputContractDigest, values, record.ValuesDigest, record.Values.PublishedAt().UnixNano())
	if err != nil {
		return translateError(err)
	}
	if inserted.RowsAffected() == 1 {
		return nil
	}
	var existingOperation, existingMapping, existingContract, existingValuesDigest string
	var existingCapability string
	err = r.tx.QueryRow(ctx, `SELECT operation_id, capability, output_mapping_ref, output_contract_digest, values_digest
		FROM resource_outputs WHERE resource_id=$1 AND observed_generation=$2::numeric`,
		record.ResourceID, uintText(record.ObservedGeneration)).Scan(
		&existingOperation, &existingCapability, &existingMapping, &existingContract, &existingValuesDigest)
	if err != nil {
		return translateError(err)
	}
	if existingOperation != string(record.OperationID) || existingCapability != string(record.Capability) ||
		existingMapping != record.OutputMappingRef || existingContract != record.OutputContractDigest ||
		existingValuesDigest != record.ValuesDigest {
		return fmt.Errorf("%w: contradictory output evidence for %s generation %d", application.ErrInvalidApplicationCall, record.ResourceID, record.ObservedGeneration)
	}
	return nil
}

func (r *repositories) LatestResourceOutputs(ctx context.Context, id domain.ResourceID) (application.ResourceOutputRecord, bool, error) {
	var operationID, capability, mappingRef, contractDigest, valuesDigest string
	var generationText string
	var valuesBytes []byte
	var publishedNS int64
	err := r.tx.QueryRow(ctx, `SELECT observed_generation::text, operation_id, capability, output_mapping_ref,
		output_contract_digest, values_jsonb, values_digest, published_at_ns
		FROM resource_outputs WHERE resource_id=$1
		ORDER BY resource_outputs.observed_generation DESC LIMIT 1`, id).Scan(
		&generationText, &operationID, &capability, &mappingRef, &contractDigest, &valuesBytes, &valuesDigest, &publishedNS)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return application.ResourceOutputRecord{}, false, nil
		}
		return application.ResourceOutputRecord{}, false, translateError(err)
	}
	generation, err := parseUint64(generationText)
	if err != nil {
		return application.ResourceOutputRecord{}, false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(valuesBytes))
	decoder.UseNumber()
	var raw map[string]any
	if err := decoder.Decode(&raw); err != nil {
		return application.ResourceOutputRecord{}, false, fmt.Errorf("decode resource output values: %w", err)
	}
	values := make(map[string]any, len(raw))
	for key, value := range raw {
		scalar, err := decodeOutputScalar(value)
		if err != nil {
			return application.ResourceOutputRecord{}, false, fmt.Errorf("decode output field %q: %w", key, err)
		}
		values[key] = scalar
	}
	snapshot, err := domain.NewResourceOutputs(generation, values, time.Unix(0, publishedNS).UTC())
	if err != nil {
		return application.ResourceOutputRecord{}, false, err
	}
	record := application.ResourceOutputRecord{
		ResourceID:           id,
		ObservedGeneration:   generation,
		OperationID:          domain.OperationID(operationID),
		Capability:           domain.Capability(capability),
		OutputMappingRef:     mappingRef,
		OutputContractDigest: contractDigest,
		Values:               snapshot,
		ValuesDigest:         valuesDigest,
	}
	return record, true, nil
}

func decodeOutputScalar(value any) (any, error) {
	switch value := value.(type) {
	case string:
		return value, nil
	case bool:
		return value, nil
	case json.Number:
		if integral, ok := integralJSONNumber(value); ok {
			return integral, nil
		}
		asFloat, err := value.Float64()
		if err != nil {
			return nil, fmt.Errorf("unsupported number %s", value.String())
		}
		return asFloat, nil
	default:
		return nil, fmt.Errorf("unsupported output scalar type %T", value)
	}
}

func integralJSONNumber(value json.Number) (int64, bool) {
	asFloat, err := value.Float64()
	if err != nil {
		return 0, false
	}
	if asFloat != float64(int64(asFloat)) {
		return 0, false
	}
	return int64(asFloat), true
}
