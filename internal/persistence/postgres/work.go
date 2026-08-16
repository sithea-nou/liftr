// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

func (r *repositories) GetSubmissionAttempt(ctx context.Context, operationID domain.OperationID, attempt uint64) (application.SubmissionAttemptRecord, error) {
	var record application.SubmissionAttemptRecord
	var attemptText, state string
	var failureKind, failureReason, failureMessage *string
	var claimedAt, resolvedAt *time.Time
	err := r.tx.QueryRow(ctx, `SELECT operation_id,attempt_number::text,state,dispatch_message_id,claimed_at,resolved_at,
		failure_kind,failure_reason,failure_message FROM provisioning_submission_attempts
		WHERE operation_id=$1 AND attempt_number=$2::numeric FOR UPDATE`, operationID, uintText(attempt)).Scan(&record.OperationID, &attemptText,
		&state, &record.DispatchMessage, &claimedAt, &resolvedAt, &failureKind, &failureReason, &failureMessage)
	if err != nil {
		return application.SubmissionAttemptRecord{}, translateError(err)
	}
	record.AttemptNumber, err = parseUint64(attemptText)
	if err != nil {
		return application.SubmissionAttemptRecord{}, err
	}
	record.State = application.SubmissionAttemptState(state)
	if claimedAt != nil {
		record.ClaimedAt = *claimedAt
	}
	if resolvedAt != nil {
		record.ResolvedAt = *resolvedAt
	}
	if failureKind != nil || failureReason != nil || failureMessage != nil {
		record.Failure = &provisioning.ExecutionFailure{}
		if failureKind != nil {
			record.Failure.Kind = provisioning.ExecutionFailureKind(*failureKind)
		}
		if failureReason != nil {
			record.Failure.Reason = *failureReason
		}
		if failureMessage != nil {
			record.Failure.Message = *failureMessage
		}
	}
	return record, nil
}

func (r *repositories) CreateSubmissionAttempt(ctx context.Context, record application.SubmissionAttemptRecord) error {
	_, err := r.tx.Exec(ctx, `INSERT INTO provisioning_submission_attempts
		(operation_id,attempt_number,state,dispatch_message_id,claimed_at,resolved_at,failure_kind,failure_reason,failure_message)
		VALUES ($1,$2::numeric,$3,$4,$5,$6,$7,$8,$9)`, record.OperationID, uintText(record.AttemptNumber), record.State, record.DispatchMessage,
		nullableTime(record.ClaimedAt), nullableTime(record.ResolvedAt), failureValue(record.Failure, "kind"), failureValue(record.Failure, "reason"), failureValue(record.Failure, "message"))
	return translateError(err)
}

func (r *repositories) SaveSubmissionAttempt(ctx context.Context, record application.SubmissionAttemptRecord, expected application.SubmissionAttemptState) error {
	command, err := r.tx.Exec(ctx, `UPDATE provisioning_submission_attempts SET state=$3,
		claimed_at=CASE WHEN $3='Leased' AND $4::timestamptz IS NULL THEN clock_timestamp() ELSE $4::timestamptz END,resolved_at=$5,
		failure_kind=$6,failure_reason=$7,failure_message=$8
		WHERE operation_id=$1 AND attempt_number=$2::numeric AND state=$9`, record.OperationID, uintText(record.AttemptNumber), record.State,
		nullableTime(record.ClaimedAt), nullableTime(record.ResolvedAt), failureValue(record.Failure, "kind"), failureValue(record.Failure, "reason"), failureValue(record.Failure, "message"), expected)
	if err != nil {
		return translateError(err)
	}
	if command.RowsAffected() != 1 {
		return application.ErrConcurrencyConflict
	}
	return nil
}

func (r *repositories) Enqueue(ctx context.Context, message application.OutboxMessage) error {
	var resourceID, operationID, attempt, expected, sequence any
	if message.ResourceID != "" {
		resourceID = message.ResourceID
	}
	if message.OperationID != "" {
		operationID = message.OperationID
	}
	if message.AttemptNumber != 0 {
		attempt = uintText(message.AttemptNumber)
	}
	if message.ExpectedVersion != 0 {
		expected = uintText(message.ExpectedVersion)
	}
	if message.Sequence != 0 {
		sequence = uintText(message.Sequence)
	}
	payload := message.Payload
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	payloadVersion := message.PayloadVersion
	if payloadVersion == 0 {
		payloadVersion = 1
	}
	_, err := r.tx.Exec(ctx, `INSERT INTO outbox_messages
		(id,kind,operation_id,resource_id,attempt_number,dedupe_key,expected_version,sequence,payload_version,payload,state,available_at)
		VALUES ($1,$2,$3,$4,$5::numeric,$6,$7::numeric,$8::numeric,$9,$10,'Pending',clock_timestamp()+($11::bigint * interval '1 millisecond'))
		ON CONFLICT (dedupe_key) DO NOTHING`, message.ID, message.Kind, operationID, resourceID, attempt, message.DedupeKey, expected, sequence, payloadVersion, payload, message.Delay.Milliseconds())
	return translateError(err)
}

func (r *repositories) GetOutbox(ctx context.Context, id string) (application.OutboxMessage, error) {
	return scanOutbox(r.tx.QueryRow(ctx, outboxSelect+" WHERE id=$1 FOR UPDATE", id))
}

func (r *repositories) ClaimOutbox(ctx context.Context, token string, lease time.Duration) (application.OutboxMessage, bool, error) {
	row := r.tx.QueryRow(ctx, `WITH candidate AS (
		SELECT id FROM outbox_messages WHERE
			(state='Pending' AND available_at <= clock_timestamp()) OR
			(state='Leased' AND kind <> 'Dispatch' AND leased_until <= clock_timestamp())
		ORDER BY available_at,created_at FOR UPDATE SKIP LOCKED LIMIT 1
	) UPDATE outbox_messages o SET state='Leased',lease_token=$1,
		leased_until=clock_timestamp()+($2::bigint * interval '1 millisecond'),attempt_count=attempt_count+1
		FROM candidate WHERE o.id=candidate.id RETURNING `+outboxColumns("o"), token, lease.Milliseconds())
	message, err := scanOutbox(row)
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, application.ErrResourceNotFound) {
		return application.OutboxMessage{}, false, nil
	}
	return message, err == nil, err
}

func (r *repositories) FindExpiredDispatch(ctx context.Context) (application.OutboxMessage, bool, error) {
	row := r.tx.QueryRow(ctx, outboxSelect+` WHERE kind='Dispatch' AND state='Leased' AND leased_until <= clock_timestamp()
		ORDER BY leased_until FOR UPDATE SKIP LOCKED LIMIT 1`)
	message, err := scanOutbox(row)
	if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, application.ErrResourceNotFound) {
		return application.OutboxMessage{}, false, nil
	}
	return message, err == nil, err
}

func (r *repositories) CompleteOutbox(ctx context.Context, id, token, reason string) error {
	command, err := r.tx.Exec(ctx, `UPDATE outbox_messages SET state='Completed',lease_token=NULL,leased_until=NULL,
		terminal_reason=$3,completed_at=clock_timestamp() WHERE id=$1 AND state='Leased' AND lease_token=$2 AND leased_until > clock_timestamp()`, id, token, reason)
	if err != nil {
		return translateError(err)
	}
	if command.RowsAffected() != 1 {
		return application.ErrConcurrencyConflict
	}
	return nil
}

func (r *repositories) CompleteExpiredOutbox(ctx context.Context, id, token, reason string) error {
	command, err := r.tx.Exec(ctx, `UPDATE outbox_messages SET state='Completed',lease_token=NULL,leased_until=NULL,
		terminal_reason=$3,completed_at=clock_timestamp() WHERE id=$1 AND state='Leased' AND lease_token=$2 AND leased_until <= clock_timestamp()`, id, token, reason)
	if err != nil {
		return translateError(err)
	}
	if command.RowsAffected() != 1 {
		return application.ErrConcurrencyConflict
	}
	return nil
}

func (r *repositories) RetryOutbox(ctx context.Context, id, token string, delay time.Duration, lastError string, maxAttempts int) error {
	command, err := r.tx.Exec(ctx, `UPDATE outbox_messages SET
		state=CASE WHEN attempt_count >= $5 THEN 'Dead' ELSE 'Pending' END,
		available_at=CASE WHEN attempt_count >= $5 THEN available_at ELSE clock_timestamp()+($3::bigint * interval '1 millisecond') END,
		lease_token=NULL,leased_until=NULL,last_error=$4,
		dead_at=CASE WHEN attempt_count >= $5 THEN clock_timestamp() ELSE NULL END,
		terminal_reason=CASE WHEN attempt_count >= $5 THEN 'AttemptsExhausted' ELSE NULL END
		WHERE id=$1 AND state='Leased' AND lease_token=$2 AND leased_until > clock_timestamp()`, id, token, delay.Milliseconds(), lastError, maxAttempts)
	if err != nil {
		return translateError(err)
	}
	if command.RowsAffected() != 1 {
		return application.ErrConcurrencyConflict
	}
	return nil
}

const outboxSelect = `SELECT id,kind,operation_id,resource_id,attempt_number::text,dedupe_key,
	expected_version::text,sequence::text,payload_version,payload,state,available_at,lease_token,leased_until,
	attempt_count,last_error,terminal_reason FROM outbox_messages`

func outboxColumns(alias string) string {
	return alias + `.id,` + alias + `.kind,` + alias + `.operation_id,` + alias + `.resource_id,` + alias + `.attempt_number::text,` + alias + `.dedupe_key,` +
		alias + `.expected_version::text,` + alias + `.sequence::text,` + alias + `.payload_version,` + alias + `.payload,` + alias + `.state,` + alias + `.available_at,` +
		alias + `.lease_token,` + alias + `.leased_until,` + alias + `.attempt_count,` + alias + `.last_error,` + alias + `.terminal_reason`
}

type rowScanner interface{ Scan(...any) error }

func scanOutbox(row rowScanner) (application.OutboxMessage, error) {
	var message application.OutboxMessage
	var operationID, resourceID, attemptText, expectedText, sequenceText, leaseToken, lastError, terminalReason *string
	var leasedUntil *time.Time
	if err := row.Scan(&message.ID, &message.Kind, &operationID, &resourceID, &attemptText, &message.DedupeKey, &expectedText,
		&sequenceText, &message.PayloadVersion, &message.Payload, &message.State, &message.AvailableAt, &leaseToken, &leasedUntil,
		&message.AttemptCount, &lastError, &terminalReason); err != nil {
		return application.OutboxMessage{}, translateError(err)
	}
	if operationID != nil {
		message.OperationID = domain.OperationID(*operationID)
	}
	if resourceID != nil {
		message.ResourceID = domain.ResourceID(*resourceID)
	}
	var err error
	if attemptText != nil {
		message.AttemptNumber, err = parseUint64(*attemptText)
	}
	if err == nil && expectedText != nil {
		message.ExpectedVersion, err = parseUint64(*expectedText)
	}
	if err == nil && sequenceText != nil {
		message.Sequence, err = parseUint64(*sequenceText)
	}
	if err != nil {
		return application.OutboxMessage{}, err
	}
	if leaseToken != nil {
		message.LeaseToken = *leaseToken
	}
	if leasedUntil != nil {
		message.LeasedUntil = *leasedUntil
	}
	if lastError != nil {
		message.LastError = *lastError
	}
	if terminalReason != nil {
		message.TerminalReason = *terminalReason
	}
	return message, nil
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}
