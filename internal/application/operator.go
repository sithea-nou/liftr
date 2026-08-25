// SPDX-License-Identifier: Apache-2.0

package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
)

// Operator-plane errors. Transports map each sentinel onto one curated admin
// Problem; underlying causes never cross to clients (ADR-0021).
var (
	// ErrDiagnosticStale reports that a client-supplied If-Match revision no
	// longer matches authoritative durable state. Correctness never depends on
	// it: every mutation revalidates transactionally regardless.
	ErrDiagnosticStale = errors.New("diagnostic revision is stale")
	// ErrActionNotApplicable reports that the requested recovery action has no
	// safe meaning against current durable state (for example the target became
	// terminal while the operator was reading diagnostics).
	ErrActionNotApplicable = errors.New("operator action is not applicable")
	// ErrRecoveryUnsafe reports that the RecoveryPlanner classified the current
	// situation as requiring manual intervention or as unsupported for
	// automated repair.
	ErrRecoveryUnsafe = errors.New("recovery action is unsafe")
	// ErrRecoveryAlreadyActive reports that equivalent work already exists.
	// ExistingWorkID carries the active work identity when known.
	ErrRecoveryAlreadyActive = errors.New("equivalent recovery work is already active")
)

// RecoveryAlreadyActiveError carries the identity of the existing equivalent
// work item so the transport can return it instead of a bare conflict.
type RecoveryAlreadyActiveError struct{ ExistingWorkID string }

func (e *RecoveryAlreadyActiveError) Error() string {
	if e.ExistingWorkID == "" {
		return ErrRecoveryAlreadyActive.Error()
	}
	return fmt.Sprintf("%s: %s", ErrRecoveryAlreadyActive, e.ExistingWorkID)
}

func (e *RecoveryAlreadyActiveError) Unwrap() error { return ErrRecoveryAlreadyActive }

// NotApplicableError carries the bounded diagnostic reason class explaining
// why the action does not apply. Reasons are stable runbook codes, never raw
// provider text (ADR-0021).
type NotApplicableError struct{ Reason string }

func (e *NotApplicableError) Error() string {
	if e.Reason == "" {
		return ErrActionNotApplicable.Error()
	}
	return fmt.Sprintf("%s: %s", ErrActionNotApplicable, e.Reason)
}

func (e *NotApplicableError) Unwrap() error { return ErrActionNotApplicable }

// UnsafeRecoveryError carries the bounded runbook code describing what Liftr
// cannot safely automate.
type UnsafeRecoveryError struct{ Reason string }

func (e *UnsafeRecoveryError) Error() string {
	if e.Reason == "" {
		return ErrRecoveryUnsafe.Error()
	}
	return fmt.Sprintf("%s: %s", ErrRecoveryUnsafe, e.Reason)
}

func (e *UnsafeRecoveryError) Unwrap() error { return ErrRecoveryUnsafe }

// OperatorAuthorizer decides whether an authenticated principal holds the
// platform-administrative permission for one closed operator action. It is a
// separate port from the developer Authorizer by design (ADR-0021): the two
// vocabularies are disjoint, neither implies the other, and there is no owner
// membership dimension on this side. Deny-by-default, including unknown
// actions. Like admission authorization, it is admission-time policy only and
// is never consulted by worker execution.
type OperatorAuthorizer interface {
	AuthorizeOperator(ctx context.Context, principal identity.Principal, action identity.Action, target identity.OperatorTarget) error
}

// OperatorAuditAction is the closed set of accepted privileged mutations that
// produce an immutable OperatorAction record. Diagnostic reads never persist
// audit rows.
type OperatorAuditAction string

const (
	AuditTriggerObserve        OperatorAuditAction = "trigger_observe"
	AuditTriggerPassiveObserve OperatorAuditAction = "trigger_passive_observe"
	AuditRecoverDeadWork       OperatorAuditAction = "recover_dead_work"
)

// OperatorActionRecord is one durably accepted privileged mutation. Row
// existence IS the persisted result semantics: an OperatorAction means Liftr
// durably accepted and scheduled exactly one recovery mutation (ADR-0021,
// correction 1). There is no mutable result column — idempotent replays are a
// request/telemetry outcome and never mutate or duplicate the audit row, and
// rejected attempts are deliberately not persisted at all in M20.
//
// It never carries bearer tokens, raw idempotency keys, email, groups,
// ResourceSpec, state, plans, outputs, provider diagnostics, or credentials.
type OperatorActionRecord struct {
	ID string
	// Actor is the pseudonymous issuer-qualified principal from M11 plus its
	// kind. Nothing more about the human or workload is persisted.
	ActorPrincipalID identity.PrincipalID
	ActorKind        identity.PrincipalKind
	Action           OperatorAuditAction
	TargetKind       identity.OperatorTargetKind
	TargetID         string
	// SourceWorkID references the immutable Dead work row being recovered;
	// empty for observation triggers.
	SourceWorkID string
	// CreatedWorkID references the single new canonical work item this action
	// scheduled. Exactly one work unit per accepted mutation (ADR-0021,
	// correction 3); a future multi-work workflow requires an explicit schema
	// change.
	CreatedWorkID string
	// IdempotencyDigest binds the (scope,key) pair without storing the raw key:
	// sha256 over length-framed scope||key.
	IdempotencyDigest []byte
	RequestID         string
	CreatedAt         time.Time
}

// OperatorAuditRepository persists append-only operator-action provenance.
// Implementations must reject updates and deletes.
type OperatorAuditRepository interface {
	GetOperatorAction(context.Context, string) (OperatorActionRecord, error)
	InsertOperatorAction(context.Context, OperatorActionRecord) error
}

// OperatorIdempotencyRecord scopes one bound operator idempotency key to the
// accepted OperatorAction that first consumed it.
type OperatorIdempotencyRecord struct {
	Scope            string // operator PrincipalID
	Key              string
	Fingerprint      []byte
	OperatorActionID string
}

// OperatorIdempotencyRepository resolves and binds operator-scoped
// idempotency keys. Only successfully applied mutations ever bind a key;
// rejected requests leave no row so the same key may be retried later,
// matching Liftr admission philosophy (ADR-0021).
type OperatorIdempotencyRepository interface {
	// GetOperatorIdempotency serializes concurrent same-key mutations with an
	// advisory lock before reading. Returns ErrOperatorIdempotencyNotFound
	// when unbound.
	GetOperatorIdempotency(ctx context.Context, scope, key string) (OperatorIdempotencyRecord, error)
	PutOperatorIdempotency(ctx context.Context, record OperatorIdempotencyRecord) error
}

// ErrOperatorIdempotencyNotFound reports that the scoped key was never bound.
var ErrOperatorIdempotencyNotFound = errors.New("operator idempotency record not found")

// StateIdentitySummary is the curated OpenTofu control-state view available
// to operator diagnostics. It exposes identities and presence facts only —
// never state bytes, digests in full, lineage values, or lock contents
// (ADR-0020/0021).
type StateIdentitySummary struct {
	ProvisionerRef string
	Engine         string
	Program        string
	Backend        string
	StateKey       string
	LineagePresent bool
	Serial         uint64
	DigestPrefix   string // bounded hex prefix of the unkeyed digest
	Version        uint64
}

// StateIdentityReader optionally supplies the private per-Resource control
// state binding summary. Composition wires it onto the OpenTofu evidence
// store; absence simply omits the section from diagnostics.
type StateIdentityReader interface {
	StateIdentity(ctx context.Context, resourceID domain.ResourceID) (StateIdentitySummary, bool, error)
}

// SpecDigestReader optionally supplies a stable digest of the stored Resource
// intent snapshot so diagnostics can bind content identity without ever
// returning spec material.
type SpecDigestReader interface {
	SpecDigest(ctx context.Context, resourceID domain.ResourceID) (string, bool, error)
}

// OperatorDiagnosticRepository keeps private identity and desired-state
// digest reads on the same transaction snapshot as lifecycle diagnostics.
type OperatorDiagnosticRepository interface {
	StateIdentityReader
	SpecDigestReader
}

// FingerprintOperatorRequest derives the versioned fingerprint that binds an
// operator idempotency key to one logical request: action kind, target kind,
// target ID, and the request representation version. Empty bodies are part of
// the representation contract; any future body-bearing variant MUST bump the
// version so old replays cannot alias new semantics.
func FingerprintOperatorRequest(action OperatorAuditAction, targetKind identity.OperatorTargetKind, targetID string, requestVersion int) []byte {
	return operatorDigest("liftr/operator-fingerprint/v1",
		string(action), string(targetKind), targetID, fmt.Sprintf("%d", requestVersion))
}

// DigestOperatorIdempotencyScope derives the stored digest of a scoped key so
// audit rows can prove which key bound them without persisting the raw value.
func DigestOperatorIdempotencyScope(scope, key string) []byte {
	return operatorDigest("liftr/operator-idempotency-digest/v1", scope, key)
}

// operatorDigest digests length-prefixed parts exactly like
// fingerprintHash: the encoding is injective and delimiter-free.
func operatorDigest(namespace string, parts ...string) []byte {
	h := sha256.New()
	writeFrame(h, namespace)
	for _, part := range parts {
		writeFrame(h, part)
	}
	return h.Sum(nil)
}

func writeFrame(h interface{ Write([]byte) (int, error) }, part string) {
	fmt.Fprintf(h, "%08x", len(part))
	_, _ = h.Write([]byte(part))
}
