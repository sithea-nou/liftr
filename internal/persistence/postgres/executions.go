// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

type observationDTO struct {
	Correlation provisioning.RequestCorrelation `json:"correlation"`
	Execution   *executionDTO                   `json:"execution,omitempty"`
	Resource    domain.ObservedFacts            `json:"resource"`
	ObservedNS  *int64                          `json:"observedAtNs,omitempty"`
}

type executionDTO struct {
	State   provisioning.ExecutionState    `json:"state"`
	Handle  string                         `json:"handle,omitempty"`
	Failure *provisioning.ExecutionFailure `json:"failure,omitempty"`
}

func encodeObservation(observation *provisioning.ExecutionObservation) ([]byte, error) {
	if observation == nil {
		return nil, nil
	}
	dto := observationDTO{Correlation: observation.Correlation, Resource: observation.Resource}
	if !observation.ObservedAt.IsZero() {
		value := observation.ObservedAt.UnixNano()
		dto.ObservedNS = &value
	}
	if observation.Execution != nil {
		dto.Execution = &executionDTO{State: observation.Execution.State, Failure: observation.Execution.Failure}
		if observation.Execution.Handle != nil {
			dto.Execution.Handle = observation.Execution.Handle.String()
		}
	}
	return json.Marshal(dto)
}

func decodeObservation(encoded []byte) (*provisioning.ExecutionObservation, error) {
	if len(encoded) == 0 {
		return nil, nil
	}
	var dto observationDTO
	if err := json.Unmarshal(encoded, &dto); err != nil {
		return nil, fmt.Errorf("decode execution observation: %w", err)
	}
	observation := &provisioning.ExecutionObservation{Correlation: dto.Correlation, Resource: dto.Resource}
	if dto.ObservedNS != nil {
		observation.ObservedAt = time.Unix(0, *dto.ObservedNS).UTC()
	}
	if dto.Execution != nil {
		observation.Execution = &provisioning.Execution{State: dto.Execution.State, Failure: dto.Execution.Failure}
		if dto.Execution.Handle != "" {
			handle, err := provisioning.NewExecutionHandle(dto.Execution.Handle)
			if err != nil {
				return nil, err
			}
			observation.Execution.Handle = &handle
		}
	}
	return observation, nil
}

func encodeSubmission(submission *provisioning.Submission) ([]byte, error) {
	if submission == nil {
		return nil, nil
	}
	return encodeObservation(&submission.Observation)
}

func decodeSubmission(encoded []byte) (*provisioning.Submission, error) {
	observation, err := decodeObservation(encoded)
	if err != nil || observation == nil {
		return nil, err
	}
	return &provisioning.Submission{Observation: *observation}, nil
}

func (r *repositories) GetExecution(ctx context.Context, id domain.OperationID) (application.ProvisioningExecutionRecord, error) {
	var record application.ProvisioningExecutionRecord
	var typeName, typeVersion, targetText, state, correlation, currentAttemptText, nextObservationText, versionText string
	var specVersion int
	var specBytes, submissionBytes, observationBytes []byte
	var handle, failureKind, failureReason, failureMessage *string
	var outputFailureReason, outputFailureMessage *string
	var outputMappingRef, outputResolution string
	var observedNS, providerObservedNS *int64
	err := r.tx.QueryRow(ctx, `SELECT operation_id,resource_id,provisioner_ref,resource_type_name,resource_type_version,capability,
		target_generation::text,spec_codec_version,submitted_spec,state,handle,acceptance_confirmed,correlation_status,
		submission,latest_observation,last_observed_at_ns,last_failure_kind,last_failure_reason,last_failure_message,
		current_attempt_number::text,next_observation_sequence::text,record_version::text,output_mapping_ref,output_resolution,output_failure_reason,output_failure_message,last_provider_observed_at_ns
		FROM provisioning_executions WHERE operation_id=$1 FOR UPDATE`, id).Scan(&record.OperationID, &record.ResourceID, &record.ProvisionerRef,
		&typeName, &typeVersion, &record.Capability, &targetText, &specVersion, &specBytes, &state, &handle, &record.AcceptanceConfirmed,
		&correlation, &submissionBytes, &observationBytes, &observedNS, &failureKind, &failureReason, &failureMessage,
		&currentAttemptText, &nextObservationText, &versionText, &outputMappingRef, &outputResolution, &outputFailureReason, &outputFailureMessage, &providerObservedNS)
	if err != nil {
		return application.ProvisioningExecutionRecord{}, translateError(err)
	}
	record.ResourceType = domain.ResourceTypeRef{Name: typeName, Version: typeVersion}
	record.State = application.ProvisioningAttemptState(state)
	record.Correlation = provisioning.RequestCorrelation(correlation)
	if record.Spec, err = decodeResourceSpec(specVersion, specBytes); err != nil {
		return application.ProvisioningExecutionRecord{}, err
	}
	if record.TargetGeneration, err = parseUint64(targetText); err != nil {
		return application.ProvisioningExecutionRecord{}, err
	}
	if record.CurrentAttempt, err = parseUint64(currentAttemptText); err != nil {
		return application.ProvisioningExecutionRecord{}, err
	}
	if record.NextObservation, err = parseUint64(nextObservationText); err != nil {
		return application.ProvisioningExecutionRecord{}, err
	}
	if record.Version, err = parseUint64(versionText); err != nil {
		return application.ProvisioningExecutionRecord{}, err
	}
	if handle != nil {
		value, err := provisioning.NewExecutionHandle(*handle)
		if err != nil {
			return application.ProvisioningExecutionRecord{}, err
		}
		record.Handle = &value
	}
	if record.Submission, err = decodeSubmission(submissionBytes); err != nil {
		return application.ProvisioningExecutionRecord{}, err
	}
	if record.LastObservation, err = decodeObservation(observationBytes); err != nil {
		return application.ProvisioningExecutionRecord{}, err
	}
	if observedNS != nil {
		record.LastObservedAt = time.Unix(0, *observedNS).UTC()
	}
	if failureKind != nil || failureReason != nil || failureMessage != nil {
		record.LastFailure = &provisioning.ExecutionFailure{}
		if failureKind != nil {
			record.LastFailure.Kind = provisioning.ExecutionFailureKind(*failureKind)
		}
		if failureReason != nil {
			record.LastFailure.Reason = *failureReason
		}
		if failureMessage != nil {
			record.LastFailure.Message = *failureMessage
		}
	}
	if providerObservedNS != nil {
		record.LastProviderObservedAt = time.Unix(0, *providerObservedNS).UTC()
	}
	record.OutputMappingRef = outputMappingRef
	if !application.ValidOutputResolution(application.OutputResolution(outputResolution)) {
		return application.ProvisioningExecutionRecord{}, fmt.Errorf("invalid persisted output resolution %q", outputResolution)
	}
	record.OutputResolution = application.OutputResolution(outputResolution)
	if outputFailureReason != nil {
		record.OutputFailureReason = *outputFailureReason
	}
	if outputFailureMessage != nil {
		record.OutputFailureMessage = *outputFailureMessage
	}
	return record, nil
}

func (r *repositories) CreateExecution(ctx context.Context, record application.ProvisioningExecutionRecord) error {
	codecVersion, spec, err := encodeResourceSpec(record.Spec)
	if err != nil {
		return err
	}
	submission, err := encodeSubmission(record.Submission)
	if err != nil {
		return err
	}
	observation, err := encodeObservation(record.LastObservation)
	if err != nil {
		return err
	}
	version := record.Version
	if version == 0 {
		version = 1
	}
	nextObservation := record.NextObservation
	if nextObservation == 0 {
		nextObservation = 1
	}
	correlation := record.Correlation
	if correlation == "" {
		correlation = provisioning.RequestCorrelationUnknown
	}
	typeRef := record.ResourceType
	resolution := string(record.OutputResolution)
	if resolution == "" {
		resolution = string(application.OutputResolutionNone)
	}
	if !application.ValidOutputResolution(application.OutputResolution(resolution)) {
		return fmt.Errorf("invalid output resolution %q", resolution)
	}
	outputFailureReason := nullableText(record.OutputFailureReason)
	outputFailureMessage := nullableText(record.OutputFailureMessage)
	_, err = r.tx.Exec(ctx, `INSERT INTO provisioning_executions
		(operation_id,resource_id,provisioner_ref,resource_type_name,resource_type_version,capability,target_generation,
		spec_codec_version,submitted_spec,state,handle,acceptance_confirmed,correlation_status,submission,latest_observation,
		last_observed_at_ns,last_provider_observed_at_ns,last_failure_kind,last_failure_reason,last_failure_message,current_attempt_number,next_observation_sequence,record_version,
		output_mapping_ref,output_resolution,output_failure_reason,output_failure_message)
		VALUES ($1,$2,$3,$4,$5,$6,$7::numeric,$8,$9,$10,$11,$12,$13,$14,$15,$16,$27,$17,$18,$19,$20::numeric,$21::numeric,$22::numeric,$23,$24,$25,$26)`,
		record.OperationID, record.ResourceID, record.ProvisionerRef, typeRef.Name, typeRef.Version, record.Capability, uintText(record.TargetGeneration),
		codecVersion, spec, record.State, handleValue(record.Handle), record.AcceptanceConfirmed, correlation, submission, observation,
		nullableUnixNano(record.LastObservedAt), failureValue(record.LastFailure, "kind"), failureValue(record.LastFailure, "reason"), failureValue(record.LastFailure, "message"),
		uintText(record.CurrentAttempt), uintText(nextObservation), uintText(version),
		record.OutputMappingRef, resolution, outputFailureReason, outputFailureMessage,
		nullableUnixNano(record.LastProviderObservedAt))
	return translateError(err)
}

func (r *repositories) SaveExecution(ctx context.Context, record application.ProvisioningExecutionRecord, expectedVersion uint64) error {
	submission, err := encodeSubmission(record.Submission)
	if err != nil {
		return err
	}
	observation, err := encodeObservation(record.LastObservation)
	if err != nil {
		return err
	}
	command, err := r.tx.Exec(ctx, `UPDATE provisioning_executions SET state=$2,handle=$3,acceptance_confirmed=$4,
		correlation_status=$5,submission=$6,latest_observation=$7,last_observed_at_ns=$8,last_provider_observed_at_ns=$18,last_failure_kind=$9,last_failure_reason=$10,
		last_failure_message=$11,current_attempt_number=$12::numeric,next_observation_sequence=$13::numeric,
		record_version=record_version+1,updated_at=clock_timestamp(),
		output_resolution=$15,output_failure_reason=$16,output_failure_message=$17
		WHERE operation_id=$1 AND record_version=$14::numeric`, record.OperationID, record.State, handleValue(record.Handle), record.AcceptanceConfirmed,
		record.Correlation, submission, observation, nullableUnixNano(record.LastObservedAt), failureValue(record.LastFailure, "kind"),
		failureValue(record.LastFailure, "reason"), failureValue(record.LastFailure, "message"), uintText(record.CurrentAttempt), uintText(record.NextObservation), uintText(expectedVersion),
		string(record.OutputResolution), nullableText(record.OutputFailureReason), nullableText(record.OutputFailureMessage),
		nullableUnixNano(record.LastProviderObservedAt))
	if err != nil {
		return translateError(err)
	}
	if command.RowsAffected() != 1 {
		return application.ErrConcurrencyConflict
	}
	return nil
}

func handleValue(handle *provisioning.ExecutionHandle) any {
	if handle == nil {
		return nil
	}
	return handle.String()
}

func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func failureValue(failure *provisioning.ExecutionFailure, field string) any {
	if failure == nil {
		return nil
	}
	switch field {
	case "kind":
		return failure.Kind
	case "reason":
		return failure.Reason
	default:
		return failure.Message
	}
}
