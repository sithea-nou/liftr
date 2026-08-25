// SPDX-License-Identifier: Apache-2.0

package worker

import (
	"errors"
	"fmt"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
)

// Work kinds reported to telemetry.
const (
	WorkKindDrive           = "drive"
	WorkKindDispatch        = "dispatch"
	WorkKindObserve         = "observe"
	WorkKindPassiveObserve  = "passive_observe"
	WorkKindExpiredRecovery = "expired_dispatch_recovery"
)

// Outcomes are bounded dispositions of one claimed work item. They mirror the
// outbox disposition exactly: a stale or replayed item that finds an
// already-terminal Operation settles as "stale" and never reports a terminal
// transition (ADR-0018). "ambiguous" and "lease_lost" are deliberately
// distinct diagnoses: ambiguous means the external submission outcome is
// uncertain while this worker still owns the lease; lease_lost means fenced
// ownership was provably lost.
const (
	OutcomeSuccess   = "success"
	OutcomeRetry     = "retry"
	OutcomeStale     = "stale"
	OutcomeFailed    = "failed"
	OutcomeAmbiguous = "ambiguous"
	OutcomeLeaseLos  = "lease_lost"
	OutcomePanic     = "panic"
)

// WorkEvent describes one processed work item for telemetry sinks.
type WorkEvent struct {
	Kind          string
	Outcome       string
	OperationID   string
	ResourceID    string
	AttemptNumber uint64
	ErrorClass    string
}

// TerminalEvent describes one actual durable Pending/Running -> terminal
// transition committed by this process, with the persisted
// requestedAt -> completedAt latency.
type TerminalEvent struct {
	OperationID     string
	ResourceID      string
	Capability      string
	TerminalState   string
	DurationSeconds float64
}

// TelemetrySink receives worker events. Composition injects one
// implementation; the worker package never imports a telemetry library.
// All methods must be cheap and non-blocking enough for the polling loop;
// implementations must never influence durable outcomes.
type TelemetrySink interface {
	WorkCompleted(event WorkEvent)
	OperationTerminalized(event TerminalEvent)
	WorkerPanic(kind string, value string)
}

// ErrRecoveredPanic wraps every panic converted at the per-work boundary. The
// runtime suppresses its own error log for it because the sink already
// recorded a structured ERROR line.
var ErrRecoveredPanic = errors.New("worker panic recovered")

// PanicError carries a sanitized panic value. The lease of the affected item
// stays intact so expiry recovery routes it through the existing Unknown ->
// Observe machinery; a panic never marks work successful or failed.
type PanicError struct {
	Value string
}

func (e *PanicError) Error() string { return ErrRecoveredPanic.Error() + ": " + e.Value }
func (e *PanicError) Unwrap() error { return ErrRecoveredPanic }

const maxSanitizedPanicRunes = 512

func sanitizePanicValue(recovered any) string {
	raw := []rune(fmt.Sprintf("%v", recovered))
	if len(raw) > maxSanitizedPanicRunes {
		raw = raw[:maxSanitizedPanicRunes]
	}
	out := make([]rune, 0, len(raw))
	for _, char := range raw {
		if char < 0x20 || char == 0x7f {
			char = '?'
		}
		out = append(out, char)
	}
	return string(out)
}

// noteTerminalTransition buffers one Pending/Running -> terminal transition
// observed after its SaveOperation succeeded inside the current transaction.
// RunOnce flushes it only when that transaction commits; rollback discards it,
// so counters count real durable transitions only (ADR-0018).
func (w *Worker) noteTerminalTransition(pre, post domain.Operation, resourceID domain.ResourceID) {
	w.pendingTerminal = &operationSnapshot{
		operationID:   string(post.ID()),
		resourceID:    string(resourceID),
		capability:    string(pre.Capability()),
		requestedAt:   pre.RequestedAt(),
		completedAt:   post.CompletedAt(),
		terminalState: string(post.State()),
	}
}

// flushTerminal emits the buffered transition after a successful commit.
func (w *Worker) flushTerminal() {
	if w.Telemetry == nil || w.pendingTerminal == nil {
		w.pendingTerminal = nil
		return
	}
	snapshot := *w.pendingTerminal
	w.pendingTerminal = nil
	duration := snapshot.completedAt.Sub(snapshot.requestedAt)
	if duration < 0 {
		duration = 0
	}
	w.Telemetry.OperationTerminalized(TerminalEvent{
		OperationID:     snapshot.operationID,
		ResourceID:      snapshot.resourceID,
		Capability:      snapshot.capability,
		TerminalState:   snapshot.terminalState,
		DurationSeconds: duration.Seconds(),
	})
}

func (w *Worker) clearPendingTerminal() {
	w.pendingTerminal = nil
}

// operationSnapshot captures everything needed to report a terminal
// transition once its commit survives.
type operationSnapshot struct {
	operationID   string
	resourceID    string
	capability    string
	requestedAt   time.Time
	completedAt   time.Time
	terminalState string
}
