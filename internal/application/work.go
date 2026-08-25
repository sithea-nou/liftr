// SPDX-License-Identifier: Apache-2.0

package application

import (
	"context"
	"fmt"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

type SubmissionAttemptState string

const (
	SubmissionAttemptPending  SubmissionAttemptState = "Pending"
	SubmissionAttemptLeased   SubmissionAttemptState = "Leased"
	SubmissionAttemptAccepted SubmissionAttemptState = "Accepted"
	SubmissionAttemptRejected SubmissionAttemptState = "Rejected"
	SubmissionAttemptNotFound SubmissionAttemptState = "NotFound"
	SubmissionAttemptUnknown  SubmissionAttemptState = "Unknown"
)

type SubmissionAttemptRecord struct {
	OperationID     domain.OperationID
	AttemptNumber   uint64
	State           SubmissionAttemptState
	DispatchMessage string
	ClaimedAt       time.Time
	ResolvedAt      time.Time
	Failure         *provisioning.ExecutionFailure
}

// AttemptHistorySummary is the bounded current view of one Operation's
// attempt history: an honest total plus only the newest record. Diagnostics
// and recovery planning never require complete attempt collections
// (ADR-0021).
type AttemptHistorySummary struct {
	Count  uint64
	Latest SubmissionAttemptRecord // zero value when Count is zero
}

type SubmissionAttemptRepository interface {
	GetSubmissionAttempt(context.Context, domain.OperationID, uint64) (SubmissionAttemptRecord, error)
	// SummarizeSubmissionAttempts returns the total durable attempt count for
	// one Operation together with its highest-numbered attempt, in one
	// bounded query. It never loads the full collection.
	SummarizeSubmissionAttempts(context.Context, domain.OperationID) (AttemptHistorySummary, error)
	CreateSubmissionAttempt(context.Context, SubmissionAttemptRecord) error
	SaveSubmissionAttempt(context.Context, SubmissionAttemptRecord, SubmissionAttemptState) error
}

type OutboxKind string

const (
	OutboxDrive          OutboxKind = "Drive"
	OutboxDispatch       OutboxKind = "Dispatch"
	OutboxObserve        OutboxKind = "Observe"
	OutboxPassiveObserve OutboxKind = "PassiveObserve"
)

type OutboxState string

const (
	OutboxPending   OutboxState = "Pending"
	OutboxLeased    OutboxState = "Leased"
	OutboxCompleted OutboxState = "Completed"
	OutboxDead      OutboxState = "Dead"
)

type OutboxMessage struct {
	ID              string
	Kind            OutboxKind
	OperationID     domain.OperationID
	ResourceID      domain.ResourceID
	AttemptNumber   uint64
	DedupeKey       string
	ExpectedVersion uint64
	Sequence        uint64
	PayloadVersion  int
	Payload         []byte
	State           OutboxState
	Delay           time.Duration
	AvailableAt     time.Time
	CreatedAt       time.Time
	LeaseToken      string
	LeasedUntil     time.Time
	AttemptCount    int
	LastError       string
	TerminalReason  string
}

// WorkActiveLimit bounds the active set a repository returns per aggregate.
// The durable schema admits at most one active row per kind via partial
// unique indexes, so this cap is structurally unreachable today; summaries
// expose an explicit truncation flag if that ever changes honestly.
const WorkActiveLimit = 8

// WorkHistorySummary is the bounded current view of one aggregate's outbox
// work. Active holds every Pending/Leased message — structurally small, since
// the schema admits at most one active row per kind — while Counts totals the
// aggregate's complete history by state without loading it (ADR-0021).
type WorkHistorySummary struct {
	Active []OutboxMessage
	Counts map[OutboxState]int
}

// HasActive reports whether any active row of the given kind exists.
func (s WorkHistorySummary) HasActive(kind OutboxKind) bool {
	return s.ActiveID(kind) != ""
}

// ActiveID returns the oldest active row ID of the given kind, or "".
func (s WorkHistorySummary) ActiveID(kind OutboxKind) string {
	for _, message := range s.Active {
		if message.Kind == kind {
			return message.ID
		}
	}
	return ""
}

type OutboxRepository interface {
	Enqueue(context.Context, OutboxMessage) error
	GetOutbox(context.Context, string) (OutboxMessage, error)
	// SummarizeWorkByOperation returns bounded current work facts for one
	// Operation: its complete active set plus total counts by state. The
	// queries are LIMIT/GROUP BY-bounded at SQL level and never load the
	// aggregate's full historical collection.
	SummarizeWorkByOperation(context.Context, domain.OperationID) (WorkHistorySummary, error)
	// SummarizeWorkByResource is SummarizeWorkByOperation for Resource-targeted
	// aggregates.
	SummarizeWorkByResource(context.Context, domain.ResourceID) (WorkHistorySummary, error)
	ClaimOutbox(context.Context, string, time.Duration) (OutboxMessage, bool, error)
	FindExpiredDispatch(context.Context) (OutboxMessage, bool, error)
	RenewOutbox(context.Context, string, string, time.Duration) error
	RequeueExpiredOutbox(context.Context, string, string) error
	CompleteOutbox(context.Context, string, string, string) error
	CompleteExpiredOutbox(context.Context, string, string, string) error
	// RetryOutbox reschedules retryable work. It never quarantines: work that
	// exhausts its backoff window keeps retrying at the bounded backoff cap.
	RetryOutbox(context.Context, string, string, time.Duration, string) error
	// RetryDispatchOutbox reschedules the same conclusively not-attempted
	// Dispatch and refreshes its execution-version fence atomically with the
	// caller's attempt and execution rollback.
	RetryDispatchOutbox(context.Context, string, string, uint64, time.Duration, string) error
	// RetryExpiredDispatchOutbox restores an expired, fenced Dispatch after its
	// provider can safely re-enter the same durable attempt.
	RetryExpiredDispatchOutbox(context.Context, string, string, uint64, time.Duration, string) error
	// DeadOutbox quarantines work that is provably invalid and cannot succeed
	// on retry. It is the only path that moves work to the Dead state.
	DeadOutbox(context.Context, string, string, string) error
}

func DriveMessage(operationID domain.OperationID, expectedVersion uint64) OutboxMessage {
	key := fmt.Sprintf("drive:%s:%d", operationID, expectedVersion)
	return OutboxMessage{ID: key, Kind: OutboxDrive, OperationID: operationID, DedupeKey: key, ExpectedVersion: expectedVersion, PayloadVersion: 1, Payload: []byte(`{}`), State: OutboxPending}
}

func DispatchMessage(operationID domain.OperationID, attemptNumber, expectedVersion uint64) OutboxMessage {
	key := fmt.Sprintf("dispatch:%s:%d", operationID, attemptNumber)
	return OutboxMessage{ID: key, Kind: OutboxDispatch, OperationID: operationID, AttemptNumber: attemptNumber, DedupeKey: key, ExpectedVersion: expectedVersion, PayloadVersion: 1, Payload: []byte(`{}`), State: OutboxPending}
}

func ObserveMessage(operationID domain.OperationID, sequence, expectedVersion uint64) OutboxMessage {
	key := fmt.Sprintf("observe:%s:%d", operationID, sequence)
	return OutboxMessage{ID: key, Kind: OutboxObserve, OperationID: operationID, DedupeKey: key, ExpectedVersion: expectedVersion, Sequence: sequence, PayloadVersion: 1, Payload: []byte(`{}`), State: OutboxPending}
}

func PassiveObserveMessage(resourceID domain.ResourceID, sequence, expectedVersion uint64) OutboxMessage {
	key := fmt.Sprintf("passive-observe:%s:%d", resourceID, sequence)
	return OutboxMessage{ID: key, Kind: OutboxPassiveObserve, ResourceID: resourceID, DedupeKey: key, ExpectedVersion: expectedVersion, Sequence: sequence, PayloadVersion: 1, Payload: []byte(`{}`), State: OutboxPending}
}
