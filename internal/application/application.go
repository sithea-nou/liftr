// SPDX-License-Identifier: Apache-2.0

// Package application coordinates Liftr domain, lifecycle, provisioning, and
// persistence ports. It does not implement persistence or transport.
package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
	"github.com/sithea-nou/liftr/internal/lifecycle"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

var (
	ErrResourceNotFound       = errors.New("resource not found")
	ErrOperationNotFound      = errors.New("operation not found")
	ErrResourceTypeNotFound   = errors.New("resource type not found")
	ErrProvisionerNotFound    = errors.New("provisioner not found")
	ErrIdempotencyConflict    = errors.New("idempotency key conflict")
	ErrIdempotencyNotFound    = errors.New("idempotency record not found")
	ErrConcurrencyConflict    = errors.New("concurrency conflict")
	ErrRetryablePersistence   = errors.New("retryable persistence error")
	ErrOperationNotRetryable  = errors.New("operation not retryable")
	ErrInvalidApplicationCall = errors.New("invalid application call")
	// ErrEagerExecutionBlockedByDependencies reports that the eager
	// synchronous test composition reached a reference-bearing Operation whose
	// hard dependencies are not all READY. Eager execution must NEVER bypass
	// M21 dependency safety (ADR-0022): the operation is handed to the durable
	// worker with a fresh canonical Drive and the inline Submit path is
	// refused. Production compositions always run the durable worker.
	ErrEagerExecutionBlockedByDependencies = errors.New("eager execution blocked by unsatisfied dependencies")
)

// ProvisionerRef is private platform metadata. It is never part of ResourceSpec
// or the developer-facing Resource contract.
type ProvisionerRef string

func NewProvisionerRef(value string) (ProvisionerRef, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("provisioner reference is required")
	}
	return ProvisionerRef(value), nil
}

type ResourceRecord struct {
	Resource       domain.Resource
	Status         domain.ResourceStatus
	ProvisionerRef ProvisionerRef
	Version        uint64
}

type ProvisioningAttemptState string

const (
	AttemptPending     ProvisioningAttemptState = "Pending"
	AttemptDispatching ProvisioningAttemptState = "Dispatching"
	AttemptAccepted    ProvisioningAttemptState = "Accepted"
	AttemptSucceeded   ProvisioningAttemptState = "Succeeded"
	AttemptFailed      ProvisioningAttemptState = "Failed"
	AttemptUnknown     ProvisioningAttemptState = "Unknown"
)

type ProvisioningExecutionRecord struct {
	OperationID               domain.OperationID
	ProvisionerRef            ProvisionerRef
	ResourceID                domain.ResourceID
	ResourceType              domain.ResourceTypeRef
	Capability                domain.Capability
	TargetGeneration          uint64
	Spec                      domain.ResourceSpec
	OutputMappingRef          string
	OutputResolution          OutputResolution
	OutputFailureReason       string
	OutputFailureMessage      string
	RecoverySourceOperationID domain.OperationID
	RecoverySourceAttempt     uint64
	Handle                    *provisioning.ExecutionHandle
	State                     ProvisioningAttemptState
	Submission                *provisioning.Submission
	AcceptanceConfirmed       bool
	LastObservation           *provisioning.ExecutionObservation
	LastObservedAt            time.Time
	// LastProviderObservedAt is the newest backend-supplied evidence
	// timestamp accepted for this execution. Provider clocks and Liftr
	// receipt clocks are separate dimensions: backend evidence freshness is
	// judged only against prior backend evidence, never against Liftr
	// receipt instants, so coarser backend granularity cannot regress.
	LastProviderObservedAt time.Time
	LastFailure            *provisioning.ExecutionFailure
	Correlation            provisioning.RequestCorrelation
	CurrentAttempt         uint64
	NextObservation        uint64
	Version                uint64
}

// OutputMappingIsBound reports whether a durable output-mapping identity has
// been persisted for this execution. The identity is assigned once, at the
// dispatch claim, before any provider work; it never changes afterwards.
func (r ProvisioningExecutionRecord) OutputMappingIsBound() bool {
	return r.OutputMappingRef != ""
}

func (r ProvisioningExecutionRecord) IsOutputRecovery() bool {
	return r.RecoverySourceOperationID != "" && r.RecoverySourceAttempt != 0
}

type IdempotencyRecord struct {
	// Scope is the idempotency namespace. It is the authenticated caller's
	// PrincipalID, so keys are unique per principal and one principal can
	// never replay another's recorded result (ADR-0012). Records persisted
	// before Milestone 11 retain the legacy "control-plane" scope and are
	// unreachable by every post-M11 principal; they are deliberately left in
	// place rather than migrated.
	Scope       string
	Key         string
	Fingerprint string
	CommandKind string
	ResourceID  domain.ResourceID
	OperationID domain.OperationID
}

type ProvisionerSelector interface {
	Select(context.Context, domain.ResourceTypeRef, domain.Capability) (ProvisionerRef, error)
}

type ProvisionerResolver interface {
	Resolve(context.Context, ProvisionerRef) (provisioning.Provisioner, error)
}

type ResourceRepository interface {
	// LookupResource returns a Resource without locking it. Admission uses it
	// only to authorize immutable owner/type identity before idempotency locks.
	LookupResource(context.Context, domain.ResourceID) (ResourceRecord, error)
	GetResource(context.Context, domain.ResourceID) (ResourceRecord, error)
	// LockResourceID serializes creation of one Resource identity and reports
	// whether any retained Resource or tombstone already owns it.
	LockResourceID(context.Context, domain.ResourceID) (bool, error)
	CreateResource(context.Context, ResourceRecord) error
	SaveResource(context.Context, ResourceRecord, uint64) error
	// ListResources returns one keyset page of the trusted ResourceListQuery.
	// The query is built exclusively by the ListResources use case after its
	// single authoritative authorization decision; repositories execute it
	// mechanically and evaluate no authorization policy (ADR-0016). The
	// returned items are summary read models: spec and conditions are never
	// loaded, and the private ordering sequence is never serialized publicly.
	ListResources(context.Context, ResourceListQuery) (ResourceInventoryPage, error)
	// LockResources row-locks every named Resource in deterministic ascending
	// ID order inside the caller's transaction and returns their current
	// records. Reference admission and dependency gating lock targets through
	// this method so multi-target schedules never deadlock on ordering.
	LockResources(context.Context, []domain.ResourceID) ([]ResourceRecord, error)
}

// MaxResourcePageSize bounds one inventory page. It mirrors the operation
// history bound: bounded responses, no unbounded fetches anywhere.
const MaxResourcePageSize = 100

// DefaultResourcePageSize is the inventory page size used when a client does
// not request one.
const DefaultResourcePageSize = 20

// ResourceListQuery is the trusted selection handed to persistence. Every
// field originates in either the authorized visibility scope or an already
// validated transport filter that can only narrow that scope; owner filters
// never grant access (ADR-0016).
type ResourceListQuery struct {
	// AllowedOwners is the complete normalized authorized owner set copied
	// from identity.ResourceVisibility.Owners. Persistence receives plain
	// domain values — never principals, claims, tokens, or visibility types.
	AllowedOwners []domain.OwnerRef
	// Unrestricted mirrors Visibility.AllOwners: only explicit insecure
	// development composition produces it; no secured policy may set it.
	Unrestricted bool
	// OwnerFilter narrows within AllowedOwners. Out-of-scope values simply
	// select nothing; they never widen authorization.
	OwnerFilter    *domain.OwnerRef
	TypeName       string
	TypeVersion    string
	StateFilter    *domain.ResourceState
	IncludeDeleted bool
	// AfterSequence is the exclusive keyset boundary: zero starts at the
	// newest Resources, any other value continues strictly below it so later
	// inserts cannot shift an open traversal window.
	AfterSequence uint64
	Limit         int
}

// ResourceInventoryStatus is the summary observation of one Resource: state
// and freshness only. Conditions belong to the detail representation.
type ResourceInventoryStatus struct {
	State              domain.ResourceState
	ObservedGeneration uint64
	UpdatedAt          time.Time
}

// ResourceInventoryLatestOperation is the four-field latest-Operation
// projection carried by inventory summaries. Inventory deliberately loads
// these fields alone; a partially populated domain.Operation would be an
// invalid read model (ADR-0016).
type ResourceInventoryLatestOperation struct {
	ID               domain.OperationID
	Capability       domain.Capability
	State            domain.OperationState
	TargetGeneration uint64
}

// ResourceInventoryItem is one summary row of the ownership-scoped inventory.
// Spec, conditions, outputs, provisioner bindings, and execution metadata are
// absent structurally: the repository SELECT never reads them, so summaries
// cannot disclose them by construction (ADR-0016).
type ResourceInventoryItem struct {
	ID         domain.ResourceID
	Type       domain.ResourceTypeRef
	Owner      domain.OwnerRef
	Generation uint64
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Status     ResourceInventoryStatus
	// Sequence is the private immutable insertion sequence. It orders the
	// traversal and binds cursors; it has no public representation anywhere.
	Sequence uint64
	// Latest is nil when the Resource has no Operation yet.
	Latest *ResourceInventoryLatestOperation
}

// ResourceInventoryPage is one ordered inventory page plus the private
// continuation position. NextSequence is zero when no further page exists;
// the use case encodes it into the opaque public cursor before returning.
type ResourceInventoryPage struct {
	Items        []ResourceInventoryItem
	NextSequence uint64
}

type OperationRecord struct {
	Operation domain.Operation
	Sequence  uint64
	Version   uint64
}

type OperationPage struct {
	Records      []OperationRecord
	NextSequence uint64
}

type OperationRepository interface {
	// LookupOperation returns an Operation without taking a row lock. Mutation
	// paths use it only to discover the Resource ID before locking Resource then
	// Operation in the canonical order.
	LookupOperation(context.Context, domain.OperationID) (OperationRecord, error)
	GetOperation(context.Context, domain.OperationID) (OperationRecord, error)
	ActiveForResource(context.Context, domain.ResourceID) (OperationRecord, bool, error)
	// LatestForResource returns the Operation with the greatest insertion
	// sequence for a Resource.
	LatestForResource(context.Context, domain.ResourceID) (OperationRecord, bool, error)
	// PageForResource returns at most limit Operations in descending insertion
	// sequence. beforeSequence is an exclusive keyset cursor; zero starts at the
	// newest Operation. NextSequence is zero when no further page exists.
	PageForResource(context.Context, domain.ResourceID, uint64, int) (OperationPage, error)
	CreateOperation(context.Context, OperationRecord) error
	SaveOperation(context.Context, OperationRecord, uint64) error
}

type EventRepository interface {
	Append(context.Context, domain.Event) error
}

type ExecutionRepository interface {
	// LookupExecution returns an execution without taking a row lock. Mutation
	// paths use it only to discover the Resource and recovery source IDs before
	// acquiring locks in the canonical order.
	LookupExecution(context.Context, domain.OperationID) (ProvisioningExecutionRecord, error)
	GetExecution(context.Context, domain.OperationID) (ProvisioningExecutionRecord, error)
	CreateExecution(context.Context, ProvisioningExecutionRecord) error
	SaveExecution(context.Context, ProvisioningExecutionRecord, uint64) error
}

type IdempotencyRepository interface {
	GetIdempotency(ctx context.Context, scope, key string) (IdempotencyRecord, error)
	PutIdempotency(ctx context.Context, record IdempotencyRecord) error
}

type UnitOfWork interface {
	Resources() ResourceRepository
	Operations() OperationRepository
	Events() EventRepository
	Executions() ExecutionRepository
	Idempotency() IdempotencyRepository
	SubmissionAttempts() SubmissionAttemptRepository
	Outbox() OutboxRepository
	Outputs() ResourceOutputRepository
	Quotas() QuotaRepository
	OperatorActions() OperatorAuditRepository
	OperatorIdempotency() OperatorIdempotencyRepository
	OperatorDiagnostics() OperatorDiagnosticRepository
	// References persists the desired/applied relationship sets (M21).
	References() ReferenceRepository
	// DependencyWaits persists the private dependency wait registrations.
	DependencyWaits() DependencyWaitRepository
}

type TransactionRunner interface {
	Within(context.Context, func(UnitOfWork) error) error
}

type Service struct {
	Types        ResourceTypeCatalog
	Selector     ProvisionerSelector
	Resolver     ProvisionerResolver
	Transactions TransactionRunner
	// Authorizer decides admission-time authorization for exported business
	// use cases. It is never consulted by worker execution paths (ADR-0012).
	Authorizer Authorizer
	// OperatorAuthorizer decides the separate closed platform-administrative
	// vocabulary for /admin/v1 use cases (ADR-0021). Nil denies every operator
	// action. It shares no vocabulary with Authorizer.
	OperatorAuthorizer OperatorAuthorizer
	// AdmissionPolicy is a restrictive, pure admission overlay. Workers never
	// consult it, and operator recovery never consults it either (ADR-0021):
	// recovery of admitted state is not new desired intent.
	AdmissionPolicy AdmissionPolicy
	Lifecycle       lifecycle.Engine
	// Now supplies read-side wall time for diagnostic ages only. It never
	// enters lifecycle decisions or ETag revisions.
	Now   func() time.Time
	eager bool
}

func (s *Service) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// EnableEagerExecutionForTesting preserves the Milestone 4 synchronous test
// harness. Production services must use the durable outbox worker.
func (s *Service) EnableEagerExecutionForTesting() { s.eager = true }

func NewService(types ResourceTypeCatalog, selector ProvisionerSelector, resolver ProvisionerResolver, transactions TransactionRunner, authorizer Authorizer, policies ...AdmissionPolicy) (*Service, error) {
	if isNilInterface(types) || isNilInterface(selector) || isNilInterface(resolver) || isNilInterface(transactions) || authorizer == nil {
		return nil, fmt.Errorf("%w: application dependencies are required", ErrInvalidApplicationCall)
	}
	if len(policies) > 1 || len(policies) == 1 && isNilInterface(policies[0]) {
		return nil, fmt.Errorf("%w: at most one non-nil admission policy is allowed", ErrInvalidApplicationCall)
	}
	admissionPolicy := AdmissionPolicy(NoRestrictionsAdmissionPolicy{})
	if len(policies) == 1 {
		admissionPolicy = policies[0]
	}
	return &Service{Types: types, Selector: selector, Resolver: resolver, Transactions: transactions, Authorizer: authorizer, AdmissionPolicy: admissionPolicy}, nil
}

type CreateResourceCommand struct {
	// Actor is the authenticated principal admitting this request. It is
	// authorized against the requested owner before any durable effect and
	// recorded on the admission audit Event (ADR-0012).
	Actor identity.Principal
	ID    domain.ResourceID
	Type  domain.ResourceTypeRef
	Owner domain.OwnerRef
	Spec  domain.ResourceSpec
	// References is the submitted desired reference binding, slot -> target
	// IDs. Nil means none were submitted; canonicalization makes ordering
	// irrelevant and rejects duplicates.
	References     map[string][]string
	OperationID    domain.OperationID
	EventID        domain.EventID
	RequestedAt    time.Time
	IdempotencyKey string
}

type UpdateResourceCommand struct {
	// Actor is the authenticated principal admitting this request. It is
	// authorized against the Resource's stored owner before generation,
	// replay, and conflict semantics are evaluated (ADR-0012).
	Actor              identity.Principal
	ID                 domain.ResourceID
	ExpectedGeneration uint64
	Spec               domain.ResourceSpec
	// ReferencesPresent distinguishes an explicitly supplied references field
	// from an absent one. Absent PRESERVES the stored desired references so
	// pre-M21 clients that send only spec can never accidentally destroy
	// relationships. Present (including an empty object) fully replaces the
	// desired set.
	ReferencesPresent bool
	References        map[string][]string
	OperationID       domain.OperationID
	EventID           domain.EventID
	RequestedAt       time.Time
	IdempotencyKey    string
}

type DeleteResourceCommand struct {
	// Actor is the authenticated principal admitting this request. It is
	// authorized against the Resource's stored owner before generation,
	// replay, and conflict semantics are evaluated (ADR-0012).
	Actor              identity.Principal
	ID                 domain.ResourceID
	ExpectedGeneration uint64
	OperationID        domain.OperationID
	EventID            domain.EventID
	RequestedAt        time.Time
	IdempotencyKey     string
}

type AdvanceOperationCommand struct {
	OperationID domain.OperationID
	Phase       domain.OperationPhase
	EventID     domain.EventID
	ChangedAt   time.Time
}

type ObserveOperationCommand struct {
	OperationID domain.OperationID
	ObservedAt  time.Time
}

type ObserveResourceCommand struct {
	ID         domain.ResourceID
	ObservedAt time.Time
}

type RetryOperationCommand struct {
	// Actor is the authenticated principal admitting this retry. It is
	// authorized against the Resource's stored owner with resource:retry
	// (ADR-0012).
	Actor              identity.Principal
	OperationID        domain.OperationID
	ExpectedGeneration uint64
	NewOperationID     domain.OperationID
	EventID            domain.EventID
	RequestedAt        time.Time
	IdempotencyKey     string
}

type Result struct {
	Resource  ResourceRecord
	Operation domain.Operation
	Execution *ProvisioningExecutionRecord
	Event     *domain.Event
	Replay    bool
	// OutputsPending reports that backend success was recorded while output
	// materialization is still outstanding; the operation remains active.
	OutputsPending bool
}

func (s *Service) CreateResource(ctx context.Context, cmd CreateResourceCommand) (Result, error) {
	if !s.eager {
		return s.AdmitCreateResource(ctx, cmd)
	}
	if err := validateExternalEventID(cmd.EventID); err != nil {
		return Result{}, err
	}
	result, err := s.persistCreateRequest(ctx, cmd)
	if err != nil {
		return Result{}, err
	}
	if result.Replay {
		return result, nil
	}
	return s.drive(ctx, result.Operation.ID())
}

// AdmitCreateResource durably records a create request without executing
// provider work. A worker advances the resulting outbox message.
//
// Authorization precedes every other evaluation: the requested owner is
// checked before idempotency resolution, catalog lookup, contract validation,
// or admission, so possession of an Idempotency-Key never bypasses policy and
// an unauthorized caller cannot probe the catalog through mutation errors
// (ADR-0012).
func (s *Service) AdmitCreateResource(ctx context.Context, cmd CreateResourceCommand) (Result, error) {
	if err := s.authorize(ctx, cmd.Actor, identity.ActionResourceCreate, identity.ResourceTarget{Type: cmd.Type, Owner: cmd.Owner}); err != nil {
		return Result{}, err
	}
	if err := validateExternalEventID(cmd.EventID); err != nil {
		return Result{}, err
	}
	return s.persistCreateRequest(ctx, cmd)
}

func (s *Service) UpdateResource(ctx context.Context, cmd UpdateResourceCommand) (Result, error) {
	if !s.eager {
		return s.AdmitUpdateResource(ctx, cmd)
	}
	if err := validateExternalEventID(cmd.EventID); err != nil {
		return Result{}, err
	}
	result, err := s.persistExistingRequest(ctx, existingRequest{
		id: cmd.ID, expectedGeneration: cmd.ExpectedGeneration, spec: &cmd.Spec,
		capability: domain.CapabilityUpdate, operationID: cmd.OperationID, eventID: cmd.EventID,
		requestedAt: cmd.RequestedAt, idempotencyKey: cmd.IdempotencyKey, actor: cmd.Actor,
	})
	if err != nil {
		return Result{}, err
	}
	if result.Replay {
		return result, nil
	}
	return s.drive(ctx, result.Operation.ID())
}

func (s *Service) AdmitUpdateResource(ctx context.Context, cmd UpdateResourceCommand) (Result, error) {
	if err := validateExternalEventID(cmd.EventID); err != nil {
		return Result{}, err
	}
	return s.persistExistingRequest(ctx, existingRequest{id: cmd.ID, expectedGeneration: cmd.ExpectedGeneration, spec: &cmd.Spec, referencesPresent: cmd.ReferencesPresent, references: cmd.References, capability: domain.CapabilityUpdate, operationID: cmd.OperationID, eventID: cmd.EventID, requestedAt: cmd.RequestedAt, idempotencyKey: cmd.IdempotencyKey, actor: cmd.Actor})
}

func (s *Service) DeleteResource(ctx context.Context, cmd DeleteResourceCommand) (Result, error) {
	if !s.eager {
		return s.AdmitDeleteResource(ctx, cmd)
	}
	if err := validateExternalEventID(cmd.EventID); err != nil {
		return Result{}, err
	}
	result, err := s.persistExistingRequest(ctx, existingRequest{
		id: cmd.ID, expectedGeneration: cmd.ExpectedGeneration,
		capability: domain.CapabilityDelete, operationID: cmd.OperationID, eventID: cmd.EventID,
		requestedAt: cmd.RequestedAt, idempotencyKey: cmd.IdempotencyKey, actor: cmd.Actor,
	})
	if err != nil {
		return Result{}, err
	}
	if result.Replay {
		return result, nil
	}
	return s.drive(ctx, result.Operation.ID())
}

func (s *Service) AdmitDeleteResource(ctx context.Context, cmd DeleteResourceCommand) (Result, error) {
	if err := validateExternalEventID(cmd.EventID); err != nil {
		return Result{}, err
	}
	return s.persistExistingRequest(ctx, existingRequest{id: cmd.ID, expectedGeneration: cmd.ExpectedGeneration, capability: domain.CapabilityDelete, operationID: cmd.OperationID, eventID: cmd.EventID, requestedAt: cmd.RequestedAt, idempotencyKey: cmd.IdempotencyKey, actor: cmd.Actor})
}

func (s *Service) AdvanceOperation(ctx context.Context, cmd AdvanceOperationCommand) (Result, error) {
	if !s.eager {
		return Result{}, fmt.Errorf("%w: direct operation advancement is disabled; use the durable worker", ErrInvalidApplicationCall)
	}
	if err := validateExternalEventID(cmd.EventID); err != nil {
		return Result{}, err
	}
	return s.advanceOperation(ctx, cmd)
}

func (s *Service) advanceOperation(ctx context.Context, cmd AdvanceOperationCommand) (Result, error) {
	var result Result
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		preflight, err := tx.Operations().LookupOperation(ctx, cmd.OperationID)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrOperationNotFound, err)
		}
		resourceRecord, err := tx.Resources().GetResource(ctx, preflight.Operation.ResourceID())
		if err != nil {
			return fmt.Errorf("%w: %v", ErrResourceNotFound, err)
		}
		opRecord, err := tx.Operations().GetOperation(ctx, cmd.OperationID)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrOperationNotFound, err)
		}
		transition, err := s.Lifecycle.Advance(resourceRecord.Resource, resourceRecord.Status, opRecord.Operation, cmd.Phase, cmd.EventID, cmd.ChangedAt)
		if err != nil {
			return err
		}
		resourceRecord.Status = transition.Status
		if err := tx.Resources().SaveResource(ctx, resourceRecord, resourceRecord.Version); err != nil {
			return err
		}
		resourceRecord.Version++
		if err := tx.Operations().SaveOperation(ctx, OperationRecord{Operation: transition.Operation}, opRecord.Version); err != nil {
			return err
		}
		if err := tx.Events().Append(ctx, transition.Event); err != nil {
			return err
		}
		event := transition.Event
		result = Result{Resource: resourceRecord, Operation: transition.Operation, Event: &event}
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return result, nil
}

func (s *Service) ObserveResource(ctx context.Context, cmd ObserveResourceCommand) (Result, error) {
	if !s.eager {
		return s.schedulePassiveObservation(ctx, cmd.ID)
	}
	record, err := s.loadResource(ctx, cmd.ID)
	if err != nil {
		return Result{}, err
	}
	provider, err := s.Resolver.Resolve(ctx, record.ProvisionerRef)
	if err != nil || isNilInterface(provider) {
		return Result{}, fmt.Errorf("%w: %v", ErrProvisionerNotFound, err)
	}
	observation, err := provider.Observe(ctx, provisioning.ObservationRequest{ResourceID: record.Resource.ID(), ResourceType: record.Resource.Type(), Spec: record.Resource.Spec(), TargetGeneration: record.Resource.Generation()})
	if err != nil {
		return Result{}, err
	}
	var result Result
	err = s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		current, loadErr := tx.Resources().GetResource(ctx, cmd.ID)
		if loadErr != nil {
			return fmt.Errorf("%w: %v", ErrResourceNotFound, loadErr)
		}
		if current.Version != record.Version {
			return ErrConcurrencyConflict
		}
		status, applyErr := s.Lifecycle.ApplyObservation(current.Resource, current.Status, observation.Resource, observedAt(observation.ObservedAt, cmd.ObservedAt))
		if applyErr != nil {
			return applyErr
		}
		current.Status = status
		if saveErr := tx.Resources().SaveResource(ctx, current, current.Version); saveErr != nil {
			return saveErr
		}
		current.Version++
		result = Result{Resource: current}
		return nil
	})
	return result, err
}

func (s *Service) schedulePassiveObservation(ctx context.Context, id domain.ResourceID) (Result, error) {
	var result Result
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		record, err := tx.Resources().GetResource(ctx, id)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrResourceNotFound, err)
		}
		message := PassiveObserveMessage(id, record.Version, record.Version)
		if err := tx.Outbox().Enqueue(ctx, message); err != nil {
			return err
		}
		result = Result{Resource: record}
		return nil
	})
	return result, err
}

func (s *Service) ObserveOperation(ctx context.Context, cmd ObserveOperationCommand) (Result, error) {
	if !s.eager {
		return s.scheduleOperationObservation(ctx, cmd.OperationID)
	}
	opRecord, record, execution, err := s.loadOperationContext(ctx, cmd.OperationID)
	if err != nil {
		return Result{}, err
	}
	if err := validateExecutionContext(record, opRecord.Operation, execution); err != nil {
		return Result{}, err
	}
	if opRecord.Operation.IsTerminal() {
		if err := validatePersistedTerminalEvidence(execution, record.Resource, record.Status, opRecord.Operation); err != nil {
			return Result{}, err
		}
		if sanitizePersistedFacts(&execution) {
			if err := s.saveExecution(ctx, execution); err != nil {
				return Result{}, err
			}
			execution.Version++
		}
		return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, nil
	}
	if execution.IsOutputRecovery() && execution.State == AttemptSucceeded && execution.OutputResolution == OutputResolutionPending {
		return s.resolvePendingOutputs(ctx, record, opRecord, execution)
	}
	if execution.State == AttemptSucceeded || execution.State == AttemptFailed {
		if err := validatePersistedTerminalEvidence(execution, record.Resource, record.Status, opRecord.Operation); err != nil {
			return Result{}, err
		}
		sanitizePersistedFacts(&execution)
		facts, at := persistedTerminalFacts(execution, record.Status.UpdatedAt())
		if execution.State == AttemptSucceeded {
			switch execution.OutputResolution {
			case OutputResolutionPending:
				// Backend success is already proven; only the output dimension
				// is outstanding. Re-drive extraction without re-executing the
				// backend and without regressing evidence timestamps.
				return s.resolvePendingOutputs(ctx, record, opRecord, execution)
			case OutputResolutionRejected:
				return s.finishSubmitted(ctx, record, opRecord, execution, false,
					execution.OutputFailureReason, execution.OutputFailureMessage, at, facts, nil)
			default:
				return s.finishSubmitted(ctx, record, opRecord, execution, true, "RecoveredTerminalOutcome", "", at, facts, nil)
			}
		}
		failure := normalizeExecutionFailure(persistedTerminalExecution(execution).Failure)
		execution.LastFailure = failure
		return s.finishSubmitted(ctx, record, opRecord, execution, false, failure.Reason, failure.Message, at, facts, nil)
	}
	if execution.State == AttemptPending || execution.State == AttemptDispatching {
		return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, nil
	}
	provider, err := s.Resolver.Resolve(ctx, execution.ProvisionerRef)
	if err != nil || isNilInterface(provider) {
		return Result{}, fmt.Errorf("%w: %v", ErrProvisionerNotFound, err)
	}
	request := provisioning.ObservationRequest{OperationID: execution.OperationID, AttemptNumber: execution.CurrentAttempt, ResourceID: execution.ResourceID, ResourceType: execution.ResourceType, Spec: execution.Spec, Capability: execution.Capability, TargetGeneration: execution.TargetGeneration, Handle: execution.Handle, OutputMappingRef: execution.OutputMappingRef}
	observation, observeErr := provider.Observe(ctx, request)
	if observeErr != nil && !explicitTerminalObservation(observation) {
		execution.LastFailure = normalizeExecutionFailure(failureFromError(observeErr))
		execution.State = AttemptUnknown
		if saveErr := s.saveExecution(ctx, execution); saveErr != nil {
			return Result{}, fmt.Errorf("%w: %v; recording observation failure: %v", observeErr, observeErr, saveErr)
		}
		return Result{}, provisioning.ObservationError{Failure: *execution.LastFailure}
	}
	observationAt := observedAt(observation.ObservedAt, cmd.ObservedAt)
	if err := validateObservationTime(record.Resource, record.Status, observationAt); err != nil {
		return Result{}, err
	}
	if factsErr := validateObservedFacts(observation.Resource); factsErr != nil {
		if !explicitTerminalObservation(observation) {
			return Result{}, factsErr
		}
		observation.Resource = domain.ObservedFacts{}
		execution.LastFailure = &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "MalformedObservedFacts", Message: factsErr.Error()}
	}
	previousObservedAt := execution.LastObservedAt
	if !previousObservedAt.IsZero() && !observationAt.After(previousObservedAt) {
		return Result{}, fmt.Errorf("%w: observation time precedes the previous execution observation", lifecycle.ErrInvalidTransition)
	}
	previousObservation := execution.LastObservation
	execution.LastObservation = &observation
	execution.LastObservedAt = observationAt
	execution.Correlation = observation.Correlation
	// A nil Execution only means no execution is currently active. It cannot
	// prove that this OperationID was never accepted and never authorizes an
	// in-place retry of an ambiguous submission attempt.
	if observation.Execution != nil && observation.Execution.Handle != nil {
		execution.Handle = observation.Execution.Handle
	}
	if observation.Execution != nil {
		if !validExecutionState(observation.Execution.State) {
			execution.State = AttemptUnknown
			execution.LastFailure = &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "MalformedExecutionState", Message: fmt.Sprintf("provider reported invalid execution state %q", observation.Execution.State)}
			execution.LastObservation = previousObservation
			execution.LastObservedAt = previousObservedAt
			if saveErr := s.saveExecution(ctx, execution); saveErr != nil {
				return Result{}, saveErr
			}
			return Result{}, execution.LastFailure
		}
		execution.State = attemptState(observation.Execution.State)
		if executionConfirmsAcceptance(*observation.Execution) {
			execution.AcceptanceConfirmed = true
		}
		if observation.Execution.State == provisioning.ExecutionStateFailed {
			failure := normalizeExecutionFailure(observation.Execution.Failure)
			observation.Execution.Failure = failure
			execution.LastFailure = failure
			execution.State = AttemptFailed
		}
	}
	if observation.Correlation == provisioning.RequestCorrelationNotFound && execution.State == AttemptUnknown && !execution.AcceptanceConfirmed {
		return s.scheduleResubmission(ctx, record, opRecord, execution)
	}
	if shouldComplete(observation) {
		return s.finishSubmitted(ctx, record, opRecord, execution, true, "ObservationSucceeded", "", observationAt, observation.Resource, observation.Outputs)
	}
	if shouldFail(observation) {
		failure := observation.Execution.Failure
		execution.LastFailure = failure
		return s.finishSubmitted(ctx, record, opRecord, execution, false, failure.Reason, failure.Message, observationAt, observation.Resource, nil)
	}
	execution, err = s.saveObservation(ctx, execution)
	if err != nil {
		return Result{}, err
	}
	return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, nil
}

func (s *Service) scheduleOperationObservation(ctx context.Context, operationID domain.OperationID) (Result, error) {
	var result Result
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		preflight, err := tx.Operations().LookupOperation(ctx, operationID)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrOperationNotFound, err)
		}
		resource, err := tx.Resources().GetResource(ctx, preflight.Operation.ResourceID())
		if err != nil {
			return fmt.Errorf("%w: %v", ErrResourceNotFound, err)
		}
		operation, err := tx.Operations().GetOperation(ctx, operationID)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrOperationNotFound, err)
		}
		execution, err := tx.Executions().GetExecution(ctx, operationID)
		if err != nil {
			return err
		}
		if operation.Operation.IsTerminal() {
			result = Result{Resource: resource, Operation: operation.Operation, Execution: &execution}
			return nil
		}
		observable := execution.State == AttemptUnknown || execution.State == AttemptAccepted ||
			(execution.State == AttemptSucceeded && execution.OutputResolution == OutputResolutionPending)
		if !observable {
			return fmt.Errorf("%w: execution is not observable from state %s", ErrInvalidApplicationCall, execution.State)
		}
		sequence := execution.NextObservation
		if sequence == 0 {
			sequence = 1
		}
		execution.NextObservation = sequence + 1
		message := ObserveMessage(operationID, sequence, execution.Version+1)
		if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
			return err
		}
		if err := tx.Outbox().Enqueue(ctx, message); err != nil {
			return err
		}
		execution.Version++
		result = Result{Resource: resource, Operation: operation.Operation, Execution: &execution}
		return nil
	})
	return result, err
}

func (s *Service) scheduleResubmission(ctx context.Context, record ResourceRecord, opRecord OperationRecord, observed ProvisioningExecutionRecord) (Result, error) {
	var result Result
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		currentOperation, err := tx.Operations().GetOperation(ctx, observed.OperationID)
		if err != nil {
			return err
		}
		current, err := tx.Executions().GetExecution(ctx, observed.OperationID)
		if err != nil {
			return err
		}
		if currentOperation.Version != opRecord.Version || current.Version != observed.Version || current.CurrentAttempt != observed.CurrentAttempt || current.State != AttemptUnknown || current.AcceptanceConfirmed {
			return ErrConcurrencyConflict
		}
		oldAttempt, err := tx.SubmissionAttempts().GetSubmissionAttempt(ctx, current.OperationID, current.CurrentAttempt)
		if err != nil {
			return err
		}
		oldState := oldAttempt.State
		if oldState != SubmissionAttemptUnknown && oldState != SubmissionAttemptLeased {
			return ErrConcurrencyConflict
		}
		oldAttempt.State = SubmissionAttemptNotFound
		oldAttempt.ResolvedAt = observed.LastObservedAt
		if err := tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, oldAttempt, oldState); err != nil {
			return err
		}
		current.CurrentAttempt++
		current.State = AttemptPending
		current.Correlation = provisioning.RequestCorrelationNotFound
		current.LastObservation = observed.LastObservation
		current.LastObservedAt = observed.LastObservedAt
		message := DispatchMessage(current.OperationID, current.CurrentAttempt, current.Version+1)
		if err := tx.SubmissionAttempts().CreateSubmissionAttempt(ctx, SubmissionAttemptRecord{OperationID: current.OperationID, AttemptNumber: current.CurrentAttempt, State: SubmissionAttemptPending, DispatchMessage: message.ID}); err != nil {
			return err
		}
		if err := tx.Executions().SaveExecution(ctx, current, current.Version); err != nil {
			return err
		}
		if err := tx.Outbox().Enqueue(ctx, message); err != nil {
			return err
		}
		current.Version++
		result = Result{Resource: record, Operation: currentOperation.Operation, Execution: &current}
		return nil
	})
	return result, err
}

func (s *Service) RetryOperation(ctx context.Context, cmd RetryOperationCommand) (Result, error) {
	if !s.eager {
		return s.AdmitRetryOperation(ctx, cmd)
	}
	if err := validateExternalEventID(cmd.EventID); err != nil {
		return Result{}, err
	}
	result, err := s.persistRetryRequest(ctx, cmd)
	if err != nil {
		return Result{}, err
	}
	if result.Replay {
		return result, nil
	}
	return s.drive(ctx, result.Operation.ID())
}

func (s *Service) AdmitRetryOperation(ctx context.Context, cmd RetryOperationCommand) (Result, error) {
	if err := validateExternalEventID(cmd.EventID); err != nil {
		return Result{}, err
	}
	return s.persistRetryRequest(ctx, cmd)
}

// persistRetryRequest admits a retry of one failed Operation. Authorization
// against the stored owner precedes replay and every lifecycle evaluation;
// retries require their own action independently of the failed capability
// (ADR-0012).
func (s *Service) persistRetryRequest(ctx context.Context, cmd RetryOperationCommand) (Result, error) {
	var result Result
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		preflight, err := tx.Operations().LookupOperation(ctx, cmd.OperationID)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrOperationNotFound, err)
		}
		preflightResource, err := tx.Resources().LookupResource(ctx, preflight.Operation.ResourceID())
		if err != nil {
			return fmt.Errorf("%w: %v", ErrResourceNotFound, err)
		}
		if err := s.authorize(ctx, cmd.Actor, identity.ActionResourceRetry, resourceTargetOf(preflightResource)); err != nil {
			return err
		}
		if cmd.ExpectedGeneration == 0 {
			return fmt.Errorf("%w: expected generation is required", ErrInvalidApplicationCall)
		}
		fingerprint := retryCommandFingerprint(cmd)
		if replay, found, err := replayWithin(ctx, tx, idempotencyScope(cmd.Actor), cmd.IdempotencyKey, fingerprint, "retry"); err != nil {
			return err
		} else if found {
			result = replay
			return nil
		}
		record, err := tx.Resources().GetResource(ctx, preflight.Operation.ResourceID())
		if err != nil {
			return fmt.Errorf("%w: %v", ErrResourceNotFound, err)
		}
		if resourceTargetOf(record) != resourceTargetOf(preflightResource) {
			return ErrConcurrencyConflict
		}
		failed, err := tx.Operations().GetOperation(ctx, cmd.OperationID)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrOperationNotFound, err)
		}
		if failed.Operation.ResourceID() != record.Resource.ID() {
			return ErrConcurrencyConflict
		}
		if record.Resource.Generation() != cmd.ExpectedGeneration {
			return fmt.Errorf("%w: expected generation %d, got %d", ErrConcurrencyConflict, cmd.ExpectedGeneration, record.Resource.Generation())
		}
		if failed.Operation.State() != domain.OperationStateFailed {
			return fmt.Errorf("%w: source operation is not failed", ErrOperationNotRetryable)
		}
		if active, found, err := tx.Operations().ActiveForResource(ctx, record.Resource.ID()); err != nil {
			return err
		} else if found {
			return fmt.Errorf("%w: resource %q has operation %q", lifecycle.ErrOperationActive, record.Resource.ID(), active.Operation.ID())
		}
		latest, found, err := tx.Operations().LatestForResource(ctx, record.Resource.ID())
		if err != nil {
			return err
		}
		if !found || latest.Sequence != failed.Sequence || latest.Operation.ID() != failed.Operation.ID() {
			return fmt.Errorf("%w: source operation is not latest", ErrOperationNotRetryable)
		}
		if failed.Operation.TargetGeneration() != record.Resource.Generation() {
			return fmt.Errorf("%w: source operation does not target the current generation", ErrOperationNotRetryable)
		}
		if !retryStateCompatible(failed.Operation.Capability(), record.Status.State()) {
			return fmt.Errorf("%w: %s cannot be retried from resource state %s", ErrOperationNotRetryable, failed.Operation.Capability(), record.Status.State())
		}
		resourceType, err := s.Types.Get(ctx, record.Resource.Type())
		if err != nil {
			return fmt.Errorf("%w: %v", ErrResourceTypeNotFound, err)
		}
		if !resourceType.Domain().Supports(failed.Operation.Capability()) {
			return fmt.Errorf("%w: resource type no longer supports source capability", ErrOperationNotRetryable)
		}
		sourceExecution, err := tx.Executions().GetExecution(ctx, failed.Operation.ID())
		if err != nil {
			return fmt.Errorf("%w: source execution is unavailable", ErrOperationNotRetryable)
		}
		if err := validateRetryProvenance(record, failed.Operation, sourceExecution); err != nil {
			return err
		}

		childExecution := ProvisioningExecutionRecord{
			ProvisionerRef: sourceExecution.ProvisionerRef, ResourceID: sourceExecution.ResourceID,
			ResourceType: sourceExecution.ResourceType, Capability: sourceExecution.Capability,
			TargetGeneration: sourceExecution.TargetGeneration, Spec: sourceExecution.Spec,
			State: AttemptPending, Correlation: provisioning.RequestCorrelationUnknown,
			NextObservation: 1, Version: 1,
		}
		if outputRecoveryFailure(failed.Operation) {
			if evidenceErr := validatePersistedTerminalEvidence(sourceExecution, record.Resource, record.Status, failed.Operation); evidenceErr != nil {
				return fmt.Errorf("%w: malformed source terminal evidence: %v", ErrOperationNotRetryable, evidenceErr)
			}
			sourceAttempt, attemptErr := tx.SubmissionAttempts().GetSubmissionAttempt(ctx, sourceExecution.OperationID, sourceExecution.CurrentAttempt)
			if attemptErr != nil {
				return fmt.Errorf("%w: source submission attempt is unavailable", ErrOperationNotRetryable)
			}
			if evidenceErr := validateOutputRecoveryAdmission(failed.Operation, sourceExecution, sourceAttempt); evidenceErr != nil {
				return evidenceErr
			}
			provider, resolveErr := s.Resolver.Resolve(ctx, sourceExecution.ProvisionerRef)
			if resolveErr != nil || isNilInterface(provider) {
				return fmt.Errorf("%w: %v", ErrProvisionerNotFound, resolveErr)
			}
			selector, ok := provider.(interface {
				SelectOutputRecoveryMapping(domain.ResourceTypeRef, domain.Capability, string) (string, bool)
			})
			if !ok {
				return fmt.Errorf("%w: provisioner does not support output recovery", ErrProvisionerNotFound)
			}
			mapping, selected := selector.SelectOutputRecoveryMapping(sourceExecution.ResourceType, sourceExecution.Capability, sourceExecution.OutputMappingRef)
			if !selected || strings.TrimSpace(mapping) == "" || mapping == sourceExecution.OutputMappingRef {
				return fmt.Errorf("%w: no compatible output recovery mapping", ErrProvisionerNotFound)
			}
			childExecution.OutputMappingRef = mapping
			childExecution.OutputResolution = OutputResolutionPending
			childExecution.State = AttemptSucceeded
			childExecution.RecoverySourceOperationID = sourceExecution.OperationID
			childExecution.RecoverySourceAttempt = sourceExecution.CurrentAttempt
		}
		transition, err := s.Lifecycle.Request(record.Resource, resourceType.Domain(), record.Status, &failed.Operation, failed.Operation.Capability(), cmd.NewOperationID, cmd.EventID, cmd.RequestedAt)
		if err != nil {
			return err
		}
		transition.Event, err = stampedAdmissionEvent(transition, cmd.Actor)
		if err != nil {
			return err
		}
		record.Status = transition.Status
		if err := tx.Resources().SaveResource(ctx, record, record.Version); err != nil {
			return err
		}
		record.Version++
		childExecution.OperationID = transition.Operation.ID()
		result, err = persistExistingTransitionWithExecution(ctx, tx, record, transition, childExecution, idempotencyScope(cmd.Actor), cmd.IdempotencyKey, fingerprint, "retry")
		return err
	})
	return result, err
}

func retryStateCompatible(capability domain.Capability, state domain.ResourceState) bool {
	switch capability {
	case domain.CapabilityCreate:
		return state == domain.ResourceStateFailed
	case domain.CapabilityUpdate:
		return state == domain.ResourceStateReady
	case domain.CapabilityDelete:
		return state == domain.ResourceStateReady || state == domain.ResourceStateFailed
	default:
		return false
	}
}

func validateRetryProvenance(resource ResourceRecord, operation domain.Operation, execution ProvisioningExecutionRecord) error {
	if execution.OperationID != operation.ID() || execution.ResourceID != resource.Resource.ID() ||
		execution.ResourceType != resource.Resource.Type() || execution.Capability != operation.Capability() ||
		execution.TargetGeneration != operation.TargetGeneration() || execution.ProvisionerRef != resource.ProvisionerRef ||
		!reflect.DeepEqual(execution.Spec.Values(), resource.Resource.Spec().Values()) {
		return fmt.Errorf("%w: source execution provenance does not match current resource intent", ErrOperationNotRetryable)
	}
	return nil
}

func outputRecoveryFailure(operation domain.Operation) bool {
	failure, failed := operation.Failure()
	return failed && failure.Reason() == ReasonOutputPostconditionRejected
}

func validateOutputRecoveryAdmission(operation domain.Operation, execution ProvisioningExecutionRecord, attempt SubmissionAttemptRecord) error {
	failure, _ := operation.Failure()
	if execution.State != AttemptSucceeded || execution.OutputResolution != OutputResolutionRejected ||
		execution.OutputFailureReason != failure.Reason() || execution.OutputFailureMessage != failure.Message() ||
		(operation.Capability() != domain.CapabilityCreate && operation.Capability() != domain.CapabilityUpdate) ||
		execution.CurrentAttempt == 0 || execution.ProvisionerRef == "" || strings.TrimSpace(execution.OutputMappingRef) == "" ||
		attempt.OperationID != execution.OperationID || attempt.AttemptNumber != execution.CurrentAttempt {
		return fmt.Errorf("%w: source output-recovery provenance is inconsistent", ErrOperationNotRetryable)
	}
	switch attempt.State {
	case SubmissionAttemptAccepted:
		return nil
	case SubmissionAttemptUnknown:
		evidence := persistedTerminalExecution(execution)
		if execution.AcceptanceConfirmed && execution.Correlation == provisioning.RequestCorrelationFound &&
			evidence != nil && evidence.State == provisioning.ExecutionStateSucceeded {
			return nil
		}
	}
	return fmt.Errorf("%w: source submission attempt has unsafe terminal correlation state %s", ErrOperationNotRetryable, attempt.State)
}

func (s *Service) drive(ctx context.Context, operationID domain.OperationID) (Result, error) {
	if s.eager {
		// M21 eager-safety rule (ADR-0022): classify the dependency gate
		// BEFORE any inline phase advance. A reference-bearing Operation whose
		// hard dependencies are not all READY must never reach the inline
		// Submit path. Refusing here leaves the Operation untouched at its
		// admitted state with its ORIGINAL canonical Drive still valid, so the
		// durable worker owns waiting, waking, and pre-submission failure
		// outcomes through the standard gate with zero special-casing.
		handoff, err := s.eagerDependencyHandoff(ctx, operationID)
		if err != nil {
			return Result{}, err
		}
		if handoff {
			return Result{}, fmt.Errorf("%w: operation %q", ErrEagerExecutionBlockedByDependencies, operationID)
		}
	}
	for {
		opRecord, err := s.loadOperation(ctx, operationID)
		if err != nil {
			return Result{}, err
		}
		switch opRecord.Operation.Phase() {
		case domain.OperationPhaseRequested:
			if _, err := s.advanceOperation(ctx, AdvanceOperationCommand{OperationID: operationID, Phase: domain.OperationPhaseValidating, EventID: internalEventID(operationID, InternalTransitionLabel(domain.OperationPhaseValidating)), ChangedAt: opRecord.Operation.RequestedAt().Add(time.Nanosecond)}); err != nil {
				return Result{}, err
			}
		case domain.OperationPhaseValidating:
			if _, err := s.advanceOperation(ctx, AdvanceOperationCommand{OperationID: operationID, Phase: nextPhase(opRecord.Operation), EventID: internalEventID(operationID, InternalTransitionLabel(nextPhase(opRecord.Operation))), ChangedAt: opRecord.Operation.PhaseChangedAt().Add(time.Nanosecond)}); err != nil {
				return Result{}, err
			}
		case domain.OperationPhasePlanning:
			if _, err := s.advanceOperation(ctx, AdvanceOperationCommand{OperationID: operationID, Phase: domain.OperationPhaseApplying, EventID: internalEventID(operationID, InternalTransitionLabel(domain.OperationPhaseApplying)), ChangedAt: opRecord.Operation.PhaseChangedAt().Add(time.Nanosecond)}); err != nil {
				return Result{}, err
			}
		case domain.OperationPhaseApplying, domain.OperationPhaseDestroying:
			execution, err := s.loadExecution(ctx, operationID)
			if err != nil {
				return Result{}, err
			}
			if execution.IsOutputRecovery() {
				opRecord, record, _, loadErr := s.loadOperationContext(ctx, operationID)
				if loadErr != nil {
					return Result{}, loadErr
				}
				return s.resolvePendingOutputs(ctx, record, opRecord, execution)
			}
			return s.DispatchOperation(ctx, operationID)
		default:
			return Result{}, fmt.Errorf("%w: unsupported operation phase %s", lifecycle.ErrInvalidTransition, opRecord.Operation.Phase())
		}
	}
}

func nextPhase(operation domain.Operation) domain.OperationPhase {
	if operation.Capability() == domain.CapabilityDelete {
		return domain.OperationPhaseDestroying
	}
	return domain.OperationPhasePlanning
}

// eagerDependencyHandoff classifies the reference-bearing gate for the eager
// synchronous composition. Zero-reference Resources (the overwhelmingly common
// case, including every released built-in type) return false and keep today's
// eager behavior byte-for-byte. A reference-bearing Operation whose
// dependencies are not all READY returns true WITHOUT mutating any durable
// state: the admitted Operation and its original canonical Drive remain
// exactly as admission produced them, so a durable worker processes the full
// wait/wake/pre-submission-failure semantics later. Lock order follows
// loadOperationContext (unlocked lookup, then Resource row, then Operation
// row); target rows are locked by EvaluateDependencies in ascending ID order.
func (s *Service) eagerDependencyHandoff(ctx context.Context, operationID domain.OperationID) (bool, error) {
	var handoff bool
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		preflight, err := tx.Operations().LookupOperation(ctx, operationID)
		if err != nil {
			return err
		}
		if preflight.Operation.Capability() == domain.CapabilityDelete {
			// Delete bypasses dependency readiness unconditionally.
			return nil
		}
		resource, err := tx.Resources().GetResource(ctx, preflight.Operation.ResourceID())
		if err != nil {
			return err
		}
		if _, err := tx.Operations().GetOperation(ctx, operationID); err != nil {
			return err
		}
		if s.Types == nil {
			return nil
		}
		contract, err := s.Types.Get(ctx, resource.Resource.Type())
		if err != nil || isNilInterface(contract) || contract.ReferenceContract() == nil {
			return nil
		}
		desired, err := tx.References().DesiredReferences(ctx, resource.Resource.ID())
		if err != nil {
			return err
		}
		if len(desired) == 0 {
			return nil
		}
		evaluation, _, err := EvaluateDependencies(ctx, tx, s.Types, resource)
		if err != nil {
			return err
		}
		if evaluation.Class == DependencyReady {
			return nil
		}
		handoff = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return handoff, nil
}

// DispatchOperation advances a persisted provisioning attempt without creating
// a new lifecycle Operation. Unknown attempts are observed before any future
// same-OperationID resubmission can be considered.
func (s *Service) DispatchOperation(ctx context.Context, operationID domain.OperationID) (Result, error) {
	if !s.eager {
		return Result{}, fmt.Errorf("%w: direct dispatch is disabled; use the durable worker", ErrInvalidApplicationCall)
	}
	opRecord, record, execution, err := s.loadOperationContext(ctx, operationID)
	if err != nil {
		return Result{}, err
	}
	if err := validateExecutionContext(record, opRecord.Operation, execution); err != nil {
		return Result{}, err
	}
	if opRecord.Operation.IsTerminal() || (opRecord.Operation.Phase() != domain.OperationPhaseApplying && opRecord.Operation.Phase() != domain.OperationPhaseDestroying) {
		return Result{}, fmt.Errorf("%w: operation is not dispatchable", lifecycle.ErrInvalidTransition)
	}
	if execution.State == AttemptUnknown {
		return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, nil
	}
	if execution.State == AttemptDispatching {
		return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, nil
	}
	if execution.State != AttemptPending {
		return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, nil
	}
	provider, err := s.Resolver.Resolve(ctx, execution.ProvisionerRef)
	if err != nil || isNilInterface(provider) {
		return Result{}, fmt.Errorf("%w: %v", ErrProvisionerNotFound, err)
	}
	if execution.OutputMappingRef == "" {
		if source, ok := provider.(interface {
			OutputMappingRef(domain.ResourceTypeRef, domain.Capability) string
		}); ok {
			execution.OutputMappingRef = source.OutputMappingRef(execution.ResourceType, execution.Capability)
		}
	}
	execution, claimed, err := s.claimPendingDispatch(ctx, operationID, execution)
	if err != nil {
		return Result{}, err
	}
	if !claimed {
		return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, nil
	}
	submission, submitErr := provider.Submit(ctx, executionRequest(execution))
	if factsErr := validateObservedFacts(submission.Observation.Resource); factsErr != nil {
		if explicitTerminalExecution(submission) {
			submission.Observation.Resource = domain.ObservedFacts{}
			execution.LastFailure = &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "MalformedObservedFacts", Message: factsErr.Error()}
		} else {
			execution.State = AttemptUnknown
			execution.LastFailure = &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "MalformedObservedFacts", Message: factsErr.Error()}
			if saveErr := s.saveExecution(ctx, execution); saveErr != nil {
				return Result{}, saveErr
			}
			execution.Version++
			return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, factsErr
		}
	}
	if submission.Observation.Execution != nil {
		if !validExecutionState(submission.Observation.Execution.State) {
			execution.State = AttemptUnknown
			execution.LastFailure = &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "MalformedExecutionState", Message: fmt.Sprintf("provider reported invalid execution state %q", submission.Observation.Execution.State)}
			if saveErr := s.saveExecution(ctx, execution); saveErr != nil {
				return Result{}, saveErr
			}
			execution.Version++
			return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, execution.LastFailure
		}
		execution.State = attemptState(submission.Observation.Execution.State)
		if submission.Observation.Execution.Handle != nil {
			execution.Handle = submission.Observation.Execution.Handle
		}
		execution.AcceptanceConfirmed = executionConfirmsAcceptance(*submission.Observation.Execution)
	}
	execution.Submission = &submission
	if submission.Observation.Execution != nil {
		observationAt := observationTime(submission.Observation.ObservedAt, record.Status.UpdatedAt())
		if !submission.Observation.ObservedAt.IsZero() && !EvidenceFresh(execution.LastObservedAt, observationAt) {
			execution.State = AttemptUnknown
			execution.Correlation = provisioning.RequestCorrelationUnknown
			execution.LastFailure = &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "StaleSubmissionEvidence", Message: "submission evidence does not advance the persisted observation timeline"}
			if saveErr := s.saveExecution(ctx, execution); saveErr != nil {
				return Result{}, saveErr
			}
			execution.Version++
			return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, execution.LastFailure
		}
		if observationErr := validateObservationTime(record.Resource, record.Status, observationAt); observationErr != nil {
			execution.State = AttemptUnknown
			execution.LastFailure = &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "StaleSubmissionObservation", Message: observationErr.Error()}
			if saveErr := s.saveExecution(ctx, execution); saveErr != nil {
				return Result{}, saveErr
			}
			execution.Version++
			return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, observationErr
		}
		if EvidenceFresh(execution.LastObservedAt, observationAt) {
			execution.LastObservedAt = observationAt
		}
	}
	if submitErr != nil && !conclusiveNonAcceptance(submission) && !explicitTerminalExecution(submission) {
		execution.State = AttemptUnknown
		execution.LastFailure = normalizeExecutionFailure(failureFromError(submitErr))
		if err := s.saveExecution(ctx, execution); err != nil {
			return Result{}, err
		}
		execution.Version++
		return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, submitErr
	}
	if submitErr != nil {
		if submission.Observation.Execution.State == provisioning.ExecutionStateSucceeded {
			return s.finishSubmitted(ctx, record, opRecord, execution, true, "SubmissionSucceeded", "", observationTime(submission.Observation.ObservedAt, record.Status.UpdatedAt()), submission.Observation.Resource, submission.Observation.Outputs)
		}
		if ConclusiveManagedAbsence(execution.Capability, submission.Observation.Correlation, submission.Observation.Execution, execution.AcceptanceConfirmed) {
			return s.finishSubmitted(ctx, record, opRecord, execution, true, ManagedTargetAbsentReason, "",
				observationTime(submission.Observation.ObservedAt, record.Status.UpdatedAt()),
				domain.ObservedFacts{Presence: domain.ResourcePresenceNotFound, Readiness: domain.ResourceReadinessUnknown, Drift: domain.ResourceDriftUnknown}, nil)
		}
		failure := submission.Observation.Execution.Failure
		failure = normalizeExecutionFailure(failure)
		submission.Observation.Execution.Failure = failure
		execution.Submission = &submission
		execution.LastFailure = failure
		return s.finishSubmitted(ctx, record, opRecord, execution, false, failure.Reason, failure.Message, observationTime(submission.Observation.ObservedAt, record.Status.UpdatedAt()), submission.Observation.Resource, nil)
	}
	if submission.Observation.Execution == nil {
		failure := &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "MalformedSubmission", Message: "submission omitted execution result"}
		execution.State = AttemptUnknown
		execution.LastFailure = failure
		if err := s.saveExecution(ctx, execution); err != nil {
			return Result{}, err
		}
		execution.Version++
		return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, failure
	}
	if submission.Observation.Execution.State == provisioning.ExecutionStateSucceeded || submission.Observation.Execution.State == provisioning.ExecutionStateFailed {
		observationAt := observationTime(submission.Observation.ObservedAt, record.Status.UpdatedAt())
		if submission.Observation.Execution.State == provisioning.ExecutionStateSucceeded {
			return s.finishSubmitted(ctx, record, opRecord, execution, true, "SubmissionSucceeded", "", observationAt, submission.Observation.Resource, submission.Observation.Outputs)
		}
		if ConclusiveManagedAbsence(execution.Capability, submission.Observation.Correlation, submission.Observation.Execution, execution.AcceptanceConfirmed) {
			return s.finishSubmitted(ctx, record, opRecord, execution, true, ManagedTargetAbsentReason, "", observationAt,
				domain.ObservedFacts{Presence: domain.ResourcePresenceNotFound, Readiness: domain.ResourceReadinessUnknown, Drift: domain.ResourceDriftUnknown}, nil)
		}
		failure := submission.Observation.Execution.Failure
		failure = normalizeExecutionFailure(failure)
		submission.Observation.Execution.Failure = failure
		execution.Submission = &submission
		execution.LastFailure = failure
		return s.finishSubmitted(ctx, record, opRecord, execution, false, failure.Reason, failure.Message, observationAt, submission.Observation.Resource, nil)
	}
	if err := s.saveExecution(ctx, execution); err != nil {
		return Result{}, err
	}
	execution.Version++
	return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, nil
}

// RecoverOperation observes an Unknown attempt using an explicit application
// receipt timestamp. It never changes or recovers a Dispatching claim.
func (s *Service) RecoverOperation(ctx context.Context, operationID domain.OperationID, observedAt time.Time) (Result, error) {
	if !s.eager {
		return s.scheduleOperationObservation(ctx, operationID)
	}
	execution, err := s.loadExecution(ctx, operationID)
	if err != nil {
		return Result{}, err
	}
	if execution.State != AttemptUnknown {
		return Result{}, fmt.Errorf("%w: execution is not recoverable from state %s", ErrInvalidApplicationCall, execution.State)
	}
	return s.ObserveOperation(ctx, ObserveOperationCommand{OperationID: operationID, ObservedAt: observedAt})
}

// planTerminalSuccess resolves the output dimension of an imminent successful
// completion. It is a pure decision over immutable inputs (contract,
// capability, evidence, generation), so it can run outside the finish
// transaction; fencing inside the transaction still guards persistence.
func (s *Service) planTerminalSuccess(ctx context.Context, execution ProvisioningExecutionRecord, outputs *provisioning.OutputEvidence, at time.Time) (OutputPlan, error) {
	contract, err := s.Types.Get(ctx, execution.ResourceType)
	if err != nil || isNilInterface(contract) {
		return OutputPlan{}, fmt.Errorf("resource type catalog unavailable for %s/%s: %w", execution.ResourceType.Name, execution.ResourceType.Version, err)
	}
	return PlanTerminalOutputs(contract, execution.Capability, outputs, execution.TargetGeneration, at)
}

// deferOutputPublication durably records backend success with Pending output
// resolution. The operation stays active so extraction can be retried without
// re-executing the backend.
func (s *Service) deferOutputPublication(ctx context.Context, record ResourceRecord, opRecord OperationRecord, execution ProvisioningExecutionRecord) (Result, error) {
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		currentOperation, err := tx.Operations().GetOperation(ctx, execution.OperationID)
		if err != nil {
			return err
		}
		if currentOperation.Operation.IsTerminal() {
			return fmt.Errorf("%w: cannot defer outputs of a terminal operation", lifecycle.ErrInvalidTransition)
		}
		currentExecution, err := tx.Executions().GetExecution(ctx, execution.OperationID)
		if err != nil {
			return err
		}
		if currentExecution.Version != execution.Version || currentExecution.State == AttemptSucceeded && currentExecution.OutputResolution == OutputResolutionPending {
			if currentExecution.Version != execution.Version {
				return ErrConcurrencyConflict
			}
		}
		execution.Version = currentExecution.Version
		execution.State = AttemptSucceeded
		if execution.OutputResolution != OutputResolutionRejected {
			execution.OutputResolution = OutputResolutionPending
		}
		if err := tx.Executions().SaveExecution(ctx, execution, currentExecution.Version); err != nil {
			return err
		}
		execution.Version++
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution, OutputsPending: true}, nil
}

// resolvePendingOutputs re-drives output extraction for an execution whose
// backend success is already durable. It never re-executes the backend: the
// provider observes existing state, and the finish path validates and
// publishes or rejects the output dimension only.
func (s *Service) resolvePendingOutputs(ctx context.Context, record ResourceRecord, opRecord OperationRecord, execution ProvisioningExecutionRecord) (Result, error) {
	provider, err := s.Resolver.Resolve(ctx, execution.ProvisionerRef)
	if err != nil || isNilInterface(provider) {
		return Result{}, fmt.Errorf("%w: %v", ErrProvisionerNotFound, err)
	}
	request := provisioning.ObservationRequest{OperationID: execution.OperationID, AttemptNumber: execution.CurrentAttempt, ResourceID: execution.ResourceID,
		ResourceType: execution.ResourceType, Spec: execution.Spec, Capability: execution.Capability, TargetGeneration: execution.TargetGeneration,
		Handle: execution.Handle, OutputMappingRef: execution.OutputMappingRef}
	if execution.IsOutputRecovery() {
		var source ProvisioningExecutionRecord
		err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
			var loadErr error
			source, loadErr = tx.Executions().GetExecution(ctx, execution.RecoverySourceOperationID)
			if loadErr != nil {
				return loadErr
			}
			sourceOperation, loadErr := tx.Operations().GetOperation(ctx, execution.RecoverySourceOperationID)
			if loadErr != nil {
				return loadErr
			}
			sourceAttempt, loadErr := tx.SubmissionAttempts().GetSubmissionAttempt(ctx, execution.RecoverySourceOperationID, execution.RecoverySourceAttempt)
			if loadErr != nil {
				return loadErr
			}
			return ValidateOutputRecoverySource(execution, source, sourceOperation.Operation, sourceAttempt)
		})
		if err != nil {
			return Result{}, err
		}
		request = provisioning.ObservationRequest{OperationID: source.OperationID, AttemptNumber: execution.RecoverySourceAttempt,
			ResourceID: source.ResourceID, ResourceType: source.ResourceType, Spec: source.Spec, Capability: source.Capability,
			TargetGeneration: source.TargetGeneration, Handle: source.Handle, OutputMappingRef: execution.OutputMappingRef,
			OutputSourceMappingRef: source.OutputMappingRef}
	}
	observation, observeErr := provider.Observe(ctx, request)
	if observeErr != nil {
		return Result{}, observeErr
	}
	if observation.Correlation != provisioning.RequestCorrelationFound || observation.Execution == nil || observation.Execution.State != provisioning.ExecutionStateSucceeded {
		if execution.IsOutputRecovery() {
			return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution, OutputsPending: true}, nil
		}
		return Result{}, fmt.Errorf("%w: pending-output observation is not positively correlated terminal success", lifecycle.ErrInvalidTransition)
	}
	// Evidence timestamps stay pinned to the persisted terminal instant:
	// repeated observations of the same backend success may carry the same
	// provider time, and completion must continue to match that instant.
	finishAt := execution.LastObservedAt
	if execution.IsOutputRecovery() {
		finishAt = observedAt(observation.ObservedAt, record.Status.UpdatedAt())
		execution.LastObservation = &observation
		execution.LastObservedAt = finishAt
		execution.Correlation = observation.Correlation
	}
	return s.finishSubmitted(ctx, record, opRecord, execution, true, "ObservationSucceeded", "", finishAt, observation.Resource, observation.Outputs)
}

func (s *Service) finishSubmitted(ctx context.Context, record ResourceRecord, opRecord OperationRecord, execution ProvisioningExecutionRecord, succeeded bool, reason, message string, at time.Time, facts domain.ObservedFacts, outputs *provisioning.OutputEvidence) (Result, error) {
	var publish *domain.ResourceOutputs
	if succeeded {
		if err := ValidateOutputEvidenceMapping(execution.OutputMappingRef, outputs); err != nil {
			return Result{}, err
		}
		switch execution.Capability {
		case domain.CapabilityCreate, domain.CapabilityUpdate:
			plan, err := s.planTerminalSuccess(ctx, execution, outputs, at)
			if err != nil {
				// Catalog unavailability must never complete reconciliation
				// blindly: fall back to terminal-success evidence with Pending
				// resolution so recovery resolves the output dimension later.
				execution.State = AttemptSucceeded
				execution.OutputResolution = OutputResolutionPending
				execution.LastFailure = failureFromError(err)
				if saveErr := s.saveExecution(ctx, execution); saveErr != nil {
					return Result{}, fmt.Errorf("%w; recording deferred outcome: %v", err, saveErr)
				}
				execution.Version++
				return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution, OutputsPending: true}, err
			}
			switch plan.Action {
			case OutputPlanDefer:
				return s.deferOutputPublication(ctx, record, opRecord, execution)
			case OutputPlanReject:
				succeeded = false
				reason = plan.Failure.Reason
				message = plan.Failure.Message
				execution.OutputResolution = OutputResolutionRejected
				execution.OutputFailureReason = plan.Failure.Reason
				execution.OutputFailureMessage = plan.Failure.Message
			case OutputPlanPublish:
				publish = &plan.Snapshot
				execution.OutputResolution = OutputResolutionPublished
			default:
				execution.OutputResolution = OutputResolutionNone
			}
		default:
			execution.OutputResolution = OutputResolutionNone
		}
	}
	result, err := s.finishObserved(ctx, record, opRecord, execution, succeeded, reason, message, at, facts, publish)
	if err == nil {
		return result, nil
	}
	if succeeded {
		execution.State = AttemptSucceeded
		execution.LastFailure = failureFromError(err)
	} else {
		execution.State = AttemptFailed
		if execution.LastFailure == nil {
			execution.LastFailure = failureFromError(err)
		}
	}
	if saveErr := s.saveExecution(ctx, execution); saveErr != nil {
		return Result{}, fmt.Errorf("%w; recording rejected submission outcome: %v", err, saveErr)
	}
	execution.Version++
	return Result{Resource: record, Operation: opRecord.Operation, Execution: &execution}, err
}

func (s *Service) finishObserved(ctx context.Context, record ResourceRecord, opRecord OperationRecord, execution ProvisioningExecutionRecord, succeeded bool, reason, message string, at time.Time, facts domain.ObservedFacts, publish *domain.ResourceOutputs) (Result, error) {
	if at.IsZero() {
		at = record.Status.UpdatedAt()
	}
	var result Result
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		current, err := tx.Resources().GetResource(ctx, record.Resource.ID())
		if err != nil {
			return err
		}
		currentOperation, err := tx.Operations().GetOperation(ctx, opRecord.Operation.ID())
		if err != nil {
			return err
		}
		currentExecution, err := tx.Executions().GetExecution(ctx, currentOperation.Operation.ID())
		if err != nil {
			return err
		}
		if err := validateExecutionContext(current, currentOperation.Operation, currentExecution); err != nil {
			return err
		}
		// Correlated terminal evidence can never genuinely precede state Liftr
		// advanced after launching this execution; coarser backend clocks are
		// lifted to the persisted frontier so lifecycle monotonicity holds.
		if at.Before(current.Status.UpdatedAt()) {
			at = current.Status.UpdatedAt()
			execution.LastObservedAt = at
		}
		if currentOperation.Operation.IsTerminal() {
			incomingState := AttemptFailed
			if succeeded {
				incomingState = AttemptSucceeded
			}
			if currentExecution.State != incomingState {
				return fmt.Errorf("%w: terminal execution evidence contradicts persisted operation outcome", lifecycle.ErrInvalidTransition)
			}
			result = Result{Resource: current, Operation: currentOperation.Operation, Execution: &currentExecution}
			return nil
		}
		retryingPersistedTerminal := (currentExecution.State == AttemptSucceeded || currentExecution.State == AttemptFailed) && execution.LastObservedAt.Equal(currentExecution.LastObservedAt)
		if !retryingPersistedTerminal && !currentExecution.LastObservedAt.IsZero() && !execution.LastObservedAt.IsZero() && !execution.LastObservedAt.After(currentExecution.LastObservedAt) {
			return fmt.Errorf("%w: terminal observation is not newer than persisted execution evidence", lifecycle.ErrInvalidTransition)
		}
		if currentExecution.AcceptanceConfirmed {
			execution.AcceptanceConfirmed = true
		}
		if execution.Handle == nil {
			execution.Handle = currentExecution.Handle
		}
		execution.Version = currentExecution.Version
		var transition struct {
			Operation domain.Operation
			Status    domain.ResourceStatus
			Event     domain.Event
		}
		finished, finishErr := BuildFinishEvidence(s.Lifecycle, currentOperation.Operation, current.Resource, current.Status, Finish{Succeeded: succeeded, Reason: reason, Message: message, Facts: facts}, at)
		if finishErr != nil {
			return finishErr
		}
		transition.Operation, transition.Status, transition.Event = finished.Operation, finished.Status, finished.Event
		if succeeded {
			execution.State = AttemptSucceeded
			execution.LastFailure = nil
		} else if execution.OutputResolution != OutputResolutionRejected {
			// A rejected output postcondition fails the operation while the
			// backend success evidence stays intact; only ordinary failures
			// flip the attempt to Failed.
			execution.State = AttemptFailed
		}
		current.Status = transition.Status
		if err := tx.Resources().SaveResource(ctx, current, current.Version); err != nil {
			return err
		}
		current.Version++
		if publish != nil {
			contractDigest, digestErr := s.outputContractDigestFor(ctx, current.Resource.Type())
			if digestErr != nil {
				return digestErr
			}
			valuesDigest, digestErr := ValuesDigest(publish.Values())
			if digestErr != nil {
				return digestErr
			}
			outputRecord := ResourceOutputRecord{
				ResourceID:           current.Resource.ID(),
				ObservedGeneration:   publish.ObservedGeneration(),
				OperationID:          currentOperation.Operation.ID(),
				Capability:           currentOperation.Operation.Capability(),
				OutputMappingRef:     execution.OutputMappingRef,
				OutputContractDigest: contractDigest,
				Values:               *publish,
				ValuesDigest:         valuesDigest,
			}
			if err := tx.Outputs().SaveResourceOutputs(ctx, outputRecord); err != nil {
				return err
			}
		}
		if err := tx.Operations().SaveOperation(ctx, OperationRecord{Operation: transition.Operation}, currentOperation.Version); err != nil {
			return err
		}
		if err := tx.Events().Append(ctx, transition.Event); err != nil {
			return err
		}
		if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
			return err
		}
		execution.Version++
		if succeeded {
			// Eager twin of the durable terminal-success side effects (M21):
			// applied references advance ONLY here — with proven convergence
			// including output postconditions — and a successful delete
			// releases both sets atomically with the Deleted tombstone.
			capability := currentOperation.Operation.Capability()
			if capability == domain.CapabilityCreate || capability == domain.CapabilityUpdate {
				if currentOperation.Operation.TargetGeneration() == current.Resource.Generation() {
					desired, refErr := tx.References().DesiredReferences(ctx, current.Resource.ID())
					if refErr != nil {
						return refErr
					}
					applied := make([]ReferenceEdge, 0, len(desired))
					for _, edge := range desired {
						if edge.Generation != currentOperation.Operation.TargetGeneration() {
							return fmt.Errorf("%w: desired reference generation %d does not match converged generation %d",
								ErrReferenceInvariant, edge.Generation, currentOperation.Operation.TargetGeneration())
						}
						applied = append(applied, edge)
					}
					if err := tx.References().AdvanceAppliedReferences(ctx, current.Resource.ID(), currentOperation.Operation.TargetGeneration(), applied); err != nil {
						return err
					}
				}
			} else if capability == domain.CapabilityDelete && transition.Operation.State() == domain.OperationStateSucceeded {
				if err := tx.References().DeleteReferencesForSource(ctx, current.Resource.ID()); err != nil {
					return err
				}
			}
		}
		event := transition.Event
		result = Result{Resource: current, Operation: transition.Operation, Execution: &execution, Event: &event}
		return nil
	})
	return result, err
}

// outputContractDigestFor derives the provenance digest of a resource type's
// declared output contract.
func (s *Service) outputContractDigestFor(ctx context.Context, ref domain.ResourceTypeRef) (string, error) {
	contract, err := s.Types.Get(ctx, ref)
	if err != nil || isNilInterface(contract) {
		return "", fmt.Errorf("resource type catalog unavailable for %s/%s: %w", ref.Name, ref.Version, err)
	}
	return OutputContractDigest(contract.OutputContract())
}

type existingRequest struct {
	id                 domain.ResourceID
	expectedGeneration uint64
	spec               *domain.ResourceSpec
	referencesPresent  bool
	references         map[string][]string
	capability         domain.Capability
	operationID        domain.OperationID
	eventID            domain.EventID
	requestedAt        time.Time
	idempotencyKey     string
	actor              identity.Principal
}

// persistCreateRequest admits a create inside one transaction. Authorization
// of the requested owner precedes idempotency resolution, catalog lookup,
// contract validation, and admission (ADR-0012).
func (s *Service) persistCreateRequest(ctx context.Context, cmd CreateResourceCommand) (Result, error) {
	var result Result
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		if err := s.authorize(ctx, cmd.Actor, identity.ActionResourceCreate, identity.ResourceTarget{Type: cmd.Type, Owner: cmd.Owner}); err != nil {
			return err
		}
		fingerprint, err := createCommandFingerprint(cmd)
		if err != nil {
			return err
		}
		scope := idempotencyScope(cmd.Actor)
		if replay, found, err := replayWithin(ctx, tx, scope, cmd.IdempotencyKey, fingerprint, string(domain.CapabilityCreate)); err != nil {
			return err
		} else if found {
			result = replay
			return nil
		}
		resourceType, err := s.Types.Get(ctx, cmd.Type)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrResourceTypeNotFound, err)
		}
		if err := validateCommandSpec(resourceType, cmd.Spec); err != nil {
			return err
		}
		referenceContract := resourceType.ReferenceContract()
		edges, err := CanonicalizeReferences(cmd.References)
		if err != nil {
			return err
		}
		if err := ValidateReferenceShape(referenceContract, cmd.ID, edges); err != nil {
			return err
		}
		resource, err := domain.NewResource(cmd.ID, cmd.Type, cmd.Owner, cmd.Spec, cmd.RequestedAt)
		if err != nil {
			return err
		}
		status, err := domain.NewResourceStatus(resource.ID(), 0, domain.ResourceStateUnknown, nil, cmd.RequestedAt)
		if err != nil {
			return err
		}
		transition, err := s.Lifecycle.Request(resource, resourceType.Domain(), status, nil, domain.CapabilityCreate, cmd.OperationID, cmd.EventID, cmd.RequestedAt)
		if err != nil {
			return err
		}
		plan, err := s.AdmissionPolicy.Plan(AdmissionIntent{Mutation: AdmissionCreate, Owner: cmd.Owner, ResourceType: cmd.Type})
		if err != nil {
			return fmt.Errorf("%w: %v", ErrPolicyEvaluation, err)
		}
		facts := ResourceCountFacts{}
		// The M18 owner advisory lock doubles as the owner-scoped structural
		// admission lock for relationship-bearing creates. It is always held
		// before any Resource row lock; the key derivation and SQL are
		// byte-identical to the quota path (ADR-0019/0022).
		if plan.RequiresResourceCounts() || len(edges) > 0 || referenceContract != nil {
			if err := tx.Quotas().LockOwnerQuota(ctx, cmd.Owner); err != nil {
				return fmt.Errorf("%w: quota owner lock failed: %v", ErrPersistenceUnavailable, err)
			}
		}
		// The owner quota lock, when required, is always held before this
		// Resource identity lock. No fresh quota-bearing create can invert it.
		exists, err := tx.Resources().LockResourceID(ctx, cmd.ID)
		if err != nil {
			return err
		}
		if active, found, err := tx.Operations().ActiveForResource(ctx, cmd.ID); err != nil {
			return err
		} else if found {
			return fmt.Errorf("%w: resource %q has operation %q", lifecycle.ErrOperationActive, cmd.ID, active.Operation.ID())
		}
		if exists {
			return fmt.Errorf("%w: resource already exists", ErrConcurrencyConflict)
		}
		// Relationship admission runs after the owner structural lock and the
		// source identity lock, with every target row locked in deterministic
		// ID order. Target failures render one generic refusal so an
		// inaccessible target is indistinguishable from a missing one.
		if len(edges) > 0 {
			if err := s.validateReferenceTargets(ctx, tx, cmd.Actor, cmd.Owner, referenceContract, edges); err != nil {
				return err
			}
			if err := DetectDependencyCycle(ctx, tx, cmd.ID, distinctEdgeTargets(edges)); err != nil {
				return err
			}
		}
		if plan.RequiresResourceCounts() {
			facts, err = tx.Quotas().ResourceCountFacts(ctx, cmd.Owner, cmd.Type)
			if err != nil {
				if errors.Is(err, ErrQuotaInvariant) {
					return err
				}
				return fmt.Errorf("%w: quota fact query failed: %v", ErrPersistenceUnavailable, err)
			}
		}
		decision, err := s.AdmissionPolicy.Decide(plan, facts)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrPolicyEvaluation, err)
		}
		if decision.Outcome != AdmissionAllowed {
			if decision.Denial == nil {
				return fmt.Errorf("%w: denial omitted reason", ErrPolicyEvaluation)
			}
			return &PolicyAdmissionError{Revision: decision.Revision, Denial: *decision.Denial}
		}
		ref, err := s.Selector.Select(ctx, cmd.Type, domain.CapabilityCreate)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrProvisionerNotFound, err)
		}
		if _, err := NewProvisionerRef(string(ref)); err != nil {
			return fmt.Errorf("%w: %v", ErrProvisionerNotFound, err)
		}
		transition.Event, err = stampedAdmissionEvent(transition, cmd.Actor, decision.Revision)
		if err != nil {
			return err
		}
		result, err = persistNewRequest(ctx, tx, ResourceRecord{Resource: resource, Status: transition.Status, ProvisionerRef: ref, Version: 1}, transition, scope, cmd.IdempotencyKey, fingerprint, edges)
		return err
	})
	return result, err
}

// persistExistingRequest admits an update or delete against a stored Resource.
// The stored owner is authorized before generation preconditions, replay,
// active-operation checks, lifecycle legality, or contract validation are
// evaluated, so a denied caller learns nothing about current generation,
// operation activity, or outputs through conflict responses (ADR-0012).
// Replay resolution then precedes generation comparison per the ADR-0008 and
// ADR-0010 replay-precedence rules.
func (s *Service) persistExistingRequest(ctx context.Context, request existingRequest) (Result, error) {
	if request.expectedGeneration == 0 {
		return Result{}, fmt.Errorf("%w: expected generation is required", ErrInvalidApplicationCall)
	}
	var result Result
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		preflight, err := tx.Resources().LookupResource(ctx, request.id)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrResourceNotFound, err)
		}
		if err := s.authorize(ctx, request.actor, actionForCapability(request.capability), resourceTargetOf(preflight)); err != nil {
			return err
		}
		fingerprint, err := fingerprintForExistingRequest(request)
		if err != nil {
			return err
		}
		scope := idempotencyScope(request.actor)
		if replay, found, err := replayWithin(ctx, tx, scope, request.idempotencyKey, fingerprint, string(request.capability)); err != nil {
			return err
		} else if found {
			result = replay
			return nil
		}
		// Owner structural admission lock (M18 lock reused, ADR-0022). Deletes
		// always serialize because inbound protective references decide the
		// outcome; reference-bearing updates serialize whenever references are
		// explicitly supplied. It is acquired after the idempotency lock and
		// BEFORE any Resource row lock, preserving the canonical ladder.
		if request.capability == domain.CapabilityDelete || request.referencesPresent {
			if err := tx.Quotas().LockOwnerQuota(ctx, resourceTargetOf(preflight).Owner); err != nil {
				return fmt.Errorf("%w: owner admission lock failed: %v", ErrPersistenceUnavailable, err)
			}
		}
		stored, err := tx.Resources().GetResource(ctx, request.id)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrResourceNotFound, err)
		}
		if resourceTargetOf(stored) != resourceTargetOf(preflight) {
			return ErrConcurrencyConflict
		}
		if stored.Resource.Generation() != request.expectedGeneration {
			return fmt.Errorf("%w: expected generation %d, got %d", ErrConcurrencyConflict, request.expectedGeneration, stored.Resource.Generation())
		}
		active, found, err := tx.Operations().ActiveForResource(ctx, request.id)
		if err != nil {
			return err
		}
		var latest *domain.Operation
		if found {
			latest = &active.Operation
		}
		resourceType, err := s.Types.Get(ctx, stored.Resource.Type())
		if err != nil {
			return fmt.Errorf("%w: %v", ErrResourceTypeNotFound, err)
		}
		referenceContract := resourceType.ReferenceContract()
		if request.capability == domain.CapabilityDelete {
			// Target deletion protection (M21). Any inbound desired or applied
			// row is protective evidence and fails closed: rows owned by a
			// Deleted source indicate invariant corruption and refuse the
			// delete rather than being silently ignored. No cascade exists;
			// the caller must release the dependency by converging or deleting
			// the dependent first.
			protected, err := tx.References().HasInboundProtectiveReference(ctx, stored.Resource.ID())
			if err != nil {
				return err
			}
			if protected {
				return ErrResourceInUse
			}
		}
		var effectiveDesired []ReferenceEdge
		if request.capability == domain.CapabilityUpdate && referenceContract != nil || request.referencesPresent {
			oldEdges, err := tx.References().DesiredReferences(ctx, stored.Resource.ID())
			if err != nil {
				return err
			}
			effectiveDesired = oldEdges
			if request.referencesPresent {
				effectiveDesired, err = CanonicalizeReferences(request.references)
				if err != nil {
					return err
				}
				if err := ValidateReferenceShape(referenceContract, stored.Resource.ID(), effectiveDesired); err != nil {
					return err
				}
				added, _ := ReferenceDifference(oldEdges, effectiveDesired)
				if len(added) > 0 {
					// Newly added edges are new durable intent: they require
					// current same-owner, read-authorization, exact-type, and
					// eligibility validation. Preserved durable edges are
					// trusted admitted intent and are neither reauthorized nor
					// revalidated; removed edges require no target permission.
					if err := s.validateReferenceTargets(ctx, tx, request.actor, stored.Resource.Owner(), referenceContract, added); err != nil {
						return err
					}
					if err := DetectDependencyCycle(ctx, tx, stored.Resource.ID(), distinctEdgeTargets(added)); err != nil {
						return err
					}
				}
			}
		}
		if request.spec != nil {
			if err := validateCommandSpec(resourceType, *request.spec); err != nil {
				return err
			}
			if request.capability == domain.CapabilityUpdate {
				// Transition legality is contract semantics evaluated against
				// the stored desired state. It runs before UpdateSpec mutates
				// anything, so an illegal transition leaves zero durable side
				// effects. Idempotent replays never reach this point because
				// replay resolution precedes contract validation.
				if err := validateCommandTransition(resourceType, stored.Resource.Spec(), *request.spec); err != nil {
					return err
				}
			}
			if err := stored.Resource.UpdateSpec(*request.spec, request.requestedAt); err != nil {
				return err
			}
		}
		transition, err := s.Lifecycle.Request(stored.Resource, resourceType.Domain(), stored.Status, latest, request.capability, request.operationID, request.eventID, request.requestedAt)
		if err != nil {
			return err
		}
		var revision PolicyRevision
		if request.capability == domain.CapabilityUpdate {
			plan, planErr := s.AdmissionPolicy.Plan(AdmissionIntent{Mutation: AdmissionUpdate, Owner: stored.Resource.Owner(), ResourceType: stored.Resource.Type()})
			if planErr != nil {
				return fmt.Errorf("%w: %v", ErrPolicyEvaluation, planErr)
			}
			decision, decideErr := s.AdmissionPolicy.Decide(plan, ResourceCountFacts{})
			if decideErr != nil {
				return fmt.Errorf("%w: %v", ErrPolicyEvaluation, decideErr)
			}
			if decision.Outcome != AdmissionAllowed {
				if decision.Denial == nil {
					return fmt.Errorf("%w: denial omitted reason", ErrPolicyEvaluation)
				}
				return &PolicyAdmissionError{Revision: decision.Revision, Denial: *decision.Denial}
			}
			revision = decision.Revision
		}
		transition.Event, err = stampedAdmissionEvent(transition, request.actor, revision)
		if err != nil {
			return err
		}
		stored.Status = transition.Status
		if err := tx.Resources().SaveResource(ctx, stored, stored.Version); err != nil {
			return err
		}
		stored.Version++
		if request.capability == domain.CapabilityUpdate {
			// The desired set is rewritten atomically with every new
			// generation — including PRESERVED sets on absent-reference
			// updates, whose row generation must always equal the source's
			// current generation. The EDGE SET is unchanged in that case, so
			// no target is revalidated or reauthorized (ADR-0022). Applied
			// references are untouched here; the protective union covers both
			// until final successful convergence.
			if request.referencesPresent || len(effectiveDesired) > 0 {
				if err := tx.References().ReplaceDesiredReferences(ctx, stored.Resource.ID(), stored.Resource.Generation(), effectiveDesired); err != nil {
					return err
				}
			}
		}
		result, err = persistExistingTransition(ctx, tx, stored, transition, scope, request.idempotencyKey, fingerprint, string(request.capability))
		return err
	})
	return result, err
}

// actionForCapability maps an admitted lifecycle capability onto its
// authorization action.
func actionForCapability(capability domain.Capability) identity.Action {
	switch capability {
	case domain.CapabilityCreate:
		return identity.ActionResourceCreate
	case domain.CapabilityDelete:
		return identity.ActionResourceDelete
	default:
		return identity.ActionResourceUpdate
	}
}

// fingerprintForExistingRequest computes the update/delete command fingerprint.
// The update fingerprint covers only submitted content; delete fingerprints do
// not involve specs.
func fingerprintForExistingRequest(request existingRequest) (string, error) {
	if request.spec != nil && request.capability == domain.CapabilityUpdate {
		cmd := UpdateResourceCommand{ID: request.id, ExpectedGeneration: request.expectedGeneration, Spec: *request.spec, ReferencesPresent: request.referencesPresent, References: request.references}
		return updateCommandFingerprint(cmd)
	}
	if request.capability == domain.CapabilityDelete {
		return deleteCommandFingerprint(DeleteResourceCommand{ID: request.id, ExpectedGeneration: request.expectedGeneration}), nil
	}
	return "", fmt.Errorf("%w: unsupported existing request capability %s", ErrInvalidApplicationCall, request.capability)
}

// stampedAdmissionEvent stamps the authenticated principal onto the lifecycle
// admission Event so durable history answers "who requested this?" for every
// admitted user mutation (ADR-0012). Stamping happens before any persistence,
// so a malformed actor fails the request with zero durable effects.
func stampedAdmissionEvent(transition lifecycle.Result, principal identity.Principal, policyRevision ...PolicyRevision) (domain.Event, error) {
	event, err := transition.Event.WithActor(domain.EventActor{ID: string(principal.ID), Kind: string(principal.Kind)})
	if err != nil {
		return domain.Event{}, err
	}
	if len(policyRevision) == 0 || policyRevision[0] == "" {
		return event, nil
	}
	return event.WithAdmissionPolicyRevision(string(policyRevision[0]))
}

func persistNewRequest(ctx context.Context, tx UnitOfWork, record ResourceRecord, transition lifecycle.Result, scope, key, fingerprint string, edges []ReferenceEdge) (Result, error) {
	if err := tx.Resources().CreateResource(ctx, record); err != nil {
		return Result{}, err
	}
	// Desired references persist atomically with the created Resource. The
	// applied set starts empty and advances only with durable successful
	// convergence (ADR-0022).
	if err := tx.References().ReplaceDesiredReferences(ctx, record.Resource.ID(), record.Resource.Generation(), edges); err != nil {
		return Result{}, err
	}
	return persistExistingTransition(ctx, tx, record, transition, scope, key, fingerprint, string(transition.Operation.Capability()))
}

func persistExistingTransition(ctx context.Context, tx UnitOfWork, record ResourceRecord, transition lifecycle.Result, scope, key, fingerprint, commandKind string) (Result, error) {
	execution := ProvisioningExecutionRecord{
		OperationID: transition.Operation.ID(), ProvisionerRef: record.ProvisionerRef,
		ResourceID: record.Resource.ID(), ResourceType: record.Resource.Type(), Spec: record.Resource.Spec(),
		Capability: transition.Operation.Capability(), TargetGeneration: transition.Operation.TargetGeneration(),
		State: AttemptPending, Correlation: provisioning.RequestCorrelationUnknown, NextObservation: 1, Version: 1,
	}
	return persistExistingTransitionWithExecution(ctx, tx, record, transition, execution, scope, key, fingerprint, commandKind)
}

func persistExistingTransitionWithExecution(ctx context.Context, tx UnitOfWork, record ResourceRecord, transition lifecycle.Result, execution ProvisioningExecutionRecord, scope, key, fingerprint, commandKind string) (Result, error) {
	if err := tx.Operations().CreateOperation(ctx, OperationRecord{Operation: transition.Operation, Version: 1}); err != nil {
		return Result{}, err
	}
	if err := tx.Events().Append(ctx, transition.Event); err != nil {
		return Result{}, err
	}
	if err := tx.Executions().CreateExecution(ctx, execution); err != nil {
		return Result{}, err
	}
	if key != "" {
		if err := tx.Idempotency().PutIdempotency(ctx, IdempotencyRecord{Scope: scope, Key: key, Fingerprint: fingerprint, CommandKind: commandKind, ResourceID: record.Resource.ID(), OperationID: transition.Operation.ID()}); err != nil {
			return Result{}, err
		}
	}
	if err := tx.Outbox().Enqueue(ctx, DriveMessage(transition.Operation.ID(), 1)); err != nil {
		return Result{}, err
	}
	event := transition.Event
	return Result{Resource: record, Operation: transition.Operation, Execution: &execution, Event: &event}, nil
}

// replayWithin resolves a principal-scoped idempotency replay. The scope is
// the authenticated caller's PrincipalID namespace: possession of another
// principal's key finds nothing here and can never return their recorded
// result (ADR-0012).
func replayWithin(ctx context.Context, tx UnitOfWork, scope, key, fingerprint, commandKind string) (Result, bool, error) {
	if key == "" {
		return Result{}, false, nil
	}
	existing, err := tx.Idempotency().GetIdempotency(ctx, scope, key)
	if errors.Is(err, ErrIdempotencyNotFound) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	if existing.Fingerprint != fingerprint || existing.CommandKind != commandKind {
		return Result{}, false, ErrIdempotencyConflict
	}
	resource, err := tx.Resources().GetResource(ctx, existing.ResourceID)
	if err != nil {
		return Result{}, false, err
	}
	op, err := tx.Operations().GetOperation(ctx, existing.OperationID)
	if err != nil {
		return Result{}, false, err
	}
	if existing.Key != key || existing.OperationID != op.Operation.ID() || existing.ResourceID != resource.Resource.ID() || op.Operation.ResourceID() != resource.Resource.ID() {
		return Result{}, false, fmt.Errorf("%w: idempotency record associations are inconsistent", ErrInvalidApplicationCall)
	}
	execution, err := tx.Executions().GetExecution(ctx, existing.OperationID)
	if err != nil {
		return Result{}, false, err
	}
	if err := validateExecutionContext(resource, op.Operation, execution); err != nil {
		return Result{}, false, err
	}
	if op.Operation.IsTerminal() || !execution.IsOutputRecovery() && (execution.State == AttemptSucceeded || execution.State == AttemptFailed) {
		if err := validatePersistedTerminalEvidence(execution, resource.Resource, resource.Status, op.Operation); err != nil {
			return Result{}, false, err
		}
	}
	return Result{Resource: resource, Operation: op.Operation, Execution: &execution, Replay: true}, true, nil
}

func (s *Service) loadResource(ctx context.Context, id domain.ResourceID) (ResourceRecord, error) {
	var record ResourceRecord
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		var err error
		record, err = tx.Resources().GetResource(ctx, id)
		return err
	})
	if err != nil {
		return ResourceRecord{}, fmt.Errorf("%w: %v", ErrResourceNotFound, err)
	}
	return record, nil
}

func (s *Service) loadOperation(ctx context.Context, id domain.OperationID) (OperationRecord, error) {
	var record OperationRecord
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		var err error
		record, err = tx.Operations().GetOperation(ctx, id)
		return err
	})
	if err != nil {
		return OperationRecord{}, fmt.Errorf("%w: %v", ErrOperationNotFound, err)
	}
	return record, nil
}

func (s *Service) loadExecution(ctx context.Context, id domain.OperationID) (ProvisioningExecutionRecord, error) {
	var record ProvisioningExecutionRecord
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		var err error
		record, err = tx.Executions().GetExecution(ctx, id)
		return err
	})
	return record, err
}

func (s *Service) loadOperationContext(ctx context.Context, id domain.OperationID) (OperationRecord, ResourceRecord, ProvisioningExecutionRecord, error) {
	var operation OperationRecord
	var resource ResourceRecord
	var execution ProvisioningExecutionRecord
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		preflight, err := tx.Operations().LookupOperation(ctx, id)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrOperationNotFound, err)
		}
		resource, err = tx.Resources().GetResource(ctx, preflight.Operation.ResourceID())
		if err != nil {
			return fmt.Errorf("%w: %v", ErrResourceNotFound, err)
		}
		operation, err = tx.Operations().GetOperation(ctx, id)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrOperationNotFound, err)
		}
		execution, err = tx.Executions().GetExecution(ctx, id)
		return err
	})
	return operation, resource, execution, err
}

func (s *Service) saveExecution(ctx context.Context, execution ProvisioningExecutionRecord) error {
	return s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		return tx.Executions().SaveExecution(ctx, execution, execution.Version)
	})
}

func (s *Service) saveObservation(ctx context.Context, observation ProvisioningExecutionRecord) (ProvisioningExecutionRecord, error) {
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		operation, operationErr := tx.Operations().GetOperation(ctx, observation.OperationID)
		if operationErr != nil {
			return operationErr
		}
		if operation.Operation.IsTerminal() {
			return fmt.Errorf("%w: cannot persist nonterminal evidence for a terminal operation", lifecycle.ErrInvalidTransition)
		}
		current, loadErr := tx.Executions().GetExecution(ctx, observation.OperationID)
		if loadErr != nil {
			return loadErr
		}
		if !current.LastObservedAt.IsZero() && !observation.LastObservedAt.After(current.LastObservedAt) {
			return fmt.Errorf("%w: observation is not newer than persisted execution evidence", lifecycle.ErrInvalidTransition)
		}
		if current.AcceptanceConfirmed {
			observation.AcceptanceConfirmed = true
			if observation.LastObservation != nil && observation.LastObservation.Execution == nil && observation.State == AttemptPending {
				observation.State = current.State
			}
		}
		if observation.LastObservation == nil || observation.LastObservation.Execution == nil || observation.LastObservation.Execution.Handle == nil {
			observation.Handle = current.Handle
		}
		observation.Version = current.Version
		if saveErr := tx.Executions().SaveExecution(ctx, observation, current.Version); saveErr != nil {
			return saveErr
		}
		observation.Version++
		return nil
	})
	return observation, err
}

func (s *Service) claimPendingDispatch(ctx context.Context, operationID domain.OperationID, expected ProvisioningExecutionRecord) (ProvisioningExecutionRecord, bool, error) {
	var execution ProvisioningExecutionRecord
	claimed := false
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		preflight, operationErr := tx.Operations().LookupOperation(ctx, operationID)
		if operationErr != nil {
			return operationErr
		}
		resource, resourceErr := tx.Resources().GetResource(ctx, preflight.Operation.ResourceID())
		if resourceErr != nil {
			return resourceErr
		}
		operation, operationErr := tx.Operations().GetOperation(ctx, operationID)
		if operationErr != nil {
			return operationErr
		}
		if operation.Operation.ResourceID() != resource.Resource.ID() {
			return ErrConcurrencyConflict
		}
		if operation.Operation.IsTerminal() {
			return fmt.Errorf("%w: cannot dispatch a terminal operation", lifecycle.ErrInvalidTransition)
		}
		if operation.Operation.Phase() != domain.OperationPhaseApplying && operation.Operation.Phase() != domain.OperationPhaseDestroying {
			return fmt.Errorf("%w: operation is not in a dispatchable phase", lifecycle.ErrInvalidTransition)
		}
		var err error
		execution, err = tx.Executions().GetExecution(ctx, operationID)
		if err != nil {
			return err
		}
		if err := validateExecutionContext(resource, operation.Operation, execution); err != nil {
			return err
		}
		if execution.Version != expected.Version || execution.ProvisionerRef != expected.ProvisionerRef ||
			execution.OutputMappingRef != "" && execution.OutputMappingRef != expected.OutputMappingRef {
			return ErrConcurrencyConflict
		}
		if execution.OutputMappingRef == "" {
			execution.OutputMappingRef = expected.OutputMappingRef
		}
		if execution.State != AttemptPending {
			return nil
		}
		if execution.CurrentAttempt == 0 {
			execution.CurrentAttempt = 1
			message := DispatchMessage(execution.OperationID, execution.CurrentAttempt, execution.Version+1)
			if err := tx.SubmissionAttempts().CreateSubmissionAttempt(ctx, SubmissionAttemptRecord{OperationID: execution.OperationID, AttemptNumber: execution.CurrentAttempt, State: SubmissionAttemptLeased, DispatchMessage: message.ID}); err != nil {
				return err
			}
		} else {
			attempt, err := tx.SubmissionAttempts().GetSubmissionAttempt(ctx, execution.OperationID, execution.CurrentAttempt)
			if err != nil {
				return err
			}
			if attempt.State != SubmissionAttemptPending {
				return nil
			}
			attempt.State = SubmissionAttemptLeased
			if err := tx.SubmissionAttempts().SaveSubmissionAttempt(ctx, attempt, SubmissionAttemptPending); err != nil {
				return err
			}
		}
		execution.State = AttemptDispatching
		if err := tx.Executions().SaveExecution(ctx, execution, execution.Version); err != nil {
			return err
		}
		execution.Version++
		claimed = true
		return nil
	})
	return execution, claimed, err
}

// ValidateOutputRecoverySource verifies that the child still identifies the
// exact successful, output-rejected source evidence selected at admission.
func ValidateOutputRecoverySource(child, source ProvisioningExecutionRecord, operation domain.Operation, attempt SubmissionAttemptRecord) error {
	failure, failed := operation.Failure()
	if !child.IsOutputRecovery() || source.OperationID != child.RecoverySourceOperationID ||
		source.CurrentAttempt != child.RecoverySourceAttempt || source.ProvisionerRef != child.ProvisionerRef ||
		source.ResourceID != child.ResourceID || source.ResourceType != child.ResourceType ||
		source.Capability != child.Capability || source.TargetGeneration != child.TargetGeneration ||
		!reflect.DeepEqual(source.Spec.Values(), child.Spec.Values()) || source.State != AttemptSucceeded ||
		source.OutputResolution != OutputResolutionRejected || strings.TrimSpace(source.OutputMappingRef) == "" ||
		strings.TrimSpace(child.OutputMappingRef) == "" || child.OutputMappingRef == source.OutputMappingRef ||
		operation.ID() != source.OperationID || operation.ResourceID() != source.ResourceID ||
		operation.Capability() != source.Capability || operation.TargetGeneration() != source.TargetGeneration ||
		operation.State() != domain.OperationStateFailed || !failed || failure.Reason() != ReasonOutputPostconditionRejected ||
		source.OutputFailureReason != failure.Reason() || source.OutputFailureMessage != failure.Message() ||
		attempt.OperationID != source.OperationID || attempt.AttemptNumber != child.RecoverySourceAttempt ||
		!safeOutputRecoveryAttempt(source, attempt) {
		return fmt.Errorf("%w: output recovery source provenance changed", ErrInvalidApplicationCall)
	}
	return nil
}

func safeOutputRecoveryAttempt(execution ProvisioningExecutionRecord, attempt SubmissionAttemptRecord) bool {
	switch attempt.State {
	case SubmissionAttemptAccepted:
		return true
	case SubmissionAttemptUnknown:
		evidence := persistedTerminalExecution(execution)
		return execution.AcceptanceConfirmed && execution.Correlation == provisioning.RequestCorrelationFound &&
			evidence != nil && evidence.State == provisioning.ExecutionStateSucceeded
	default:
		return false
	}
}

func executionRequest(execution ProvisioningExecutionRecord) provisioning.ExecutionRequest {
	return provisioning.ExecutionRequest{
		OperationID: execution.OperationID, AttemptNumber: execution.CurrentAttempt, ResourceID: execution.ResourceID,
		ResourceType: execution.ResourceType, Spec: execution.Spec,
		Capability: execution.Capability, TargetGeneration: execution.TargetGeneration,
		OutputMappingRef: execution.OutputMappingRef,
	}
}

func conclusiveNonAcceptance(submission provisioning.Submission) bool {
	execution := submission.Observation.Execution
	if execution == nil || execution.State != provisioning.ExecutionStateFailed || execution.Failure == nil {
		return false
	}
	return execution.Failure.Kind == provisioning.FailureUnsupported || execution.Failure.Kind == provisioning.FailureInvalidRequest
}

func explicitTerminalExecution(submission provisioning.Submission) bool {
	execution := submission.Observation.Execution
	return execution != nil && (execution.State == provisioning.ExecutionStateSucceeded || execution.State == provisioning.ExecutionStateFailed)
}

func explicitTerminalObservation(observation provisioning.ExecutionObservation) bool {
	return observation.Execution != nil && (observation.Execution.State == provisioning.ExecutionStateSucceeded || observation.Execution.State == provisioning.ExecutionStateFailed)
}

func normalizeExecutionFailure(failure *provisioning.ExecutionFailure) *provisioning.ExecutionFailure {
	if failure == nil {
		return &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "MalformedExecutionFailure", Message: "provider reported failure without details"}
	}
	normalized := *failure
	if !validFailureKind(normalized.Kind) {
		normalized.Kind = provisioning.FailureUnknown
	}
	if strings.TrimSpace(normalized.Reason) == "" {
		normalized.Reason = "MalformedExecutionFailure"
		if normalized.Message == "" {
			normalized.Message = "provider reported failure without details"
		}
	}
	return &normalized
}

func validFailureKind(kind provisioning.ExecutionFailureKind) bool {
	switch kind {
	case provisioning.FailureInvalidRequest, provisioning.FailureUnsupported, provisioning.FailureUnavailable, provisioning.FailureTimeout, provisioning.FailureNotFound, provisioning.FailureExecution, provisioning.FailureUnknown:
		return true
	default:
		return false
	}
}

func hasObservedFacts(facts domain.ObservedFacts) bool {
	return facts.Presence != "" || facts.Readiness != "" || facts.Drift != ""
}

const internalEventPrefix = "liftr-internal-"

func validateExternalEventID(eventID domain.EventID) error {
	if strings.HasPrefix(string(eventID), internalEventPrefix) {
		return fmt.Errorf("%w: event ID uses reserved internal prefix", ErrInvalidApplicationCall)
	}
	return nil
}

func internalEventID(operationID domain.OperationID, transition string) domain.EventID {
	digest := sha256.Sum256([]byte(string(operationID) + "\x00" + transition))
	return domain.EventID(fmt.Sprintf("%s%x", internalEventPrefix, digest))
}

// InternalTransitionLabel renders a lifecycle phase as the canonical internal
// transition label. Every component that drives an operation through its
// phases must use it so eager and durable execution produce identical Event
// IDs for identical transitions.
func InternalTransitionLabel(phase domain.OperationPhase) string {
	return strings.ToLower(string(phase))
}

// InternalEventID is the deterministic Event ID mechanism shared by every
// Liftr-owned lifecycle transition. The digest is stable for a given
// (operation, transition) pair regardless of which component drives it.
func InternalEventID(operationID domain.OperationID, transition string) domain.EventID {
	return internalEventID(operationID, transition)
}

func attemptState(state provisioning.ExecutionState) ProvisioningAttemptState {
	switch state {
	case provisioning.ExecutionStateAccepted:
		return AttemptAccepted
	case provisioning.ExecutionStateRunning:
		return AttemptAccepted
	case provisioning.ExecutionStateSucceeded:
		return AttemptSucceeded
	case provisioning.ExecutionStateFailed:
		return AttemptFailed
	case provisioning.ExecutionStateUnknown:
		return AttemptUnknown
	default:
		return AttemptUnknown
	}
}

func executionConfirmsAcceptance(execution provisioning.Execution) bool {
	switch execution.State {
	case provisioning.ExecutionStateAccepted, provisioning.ExecutionStateRunning, provisioning.ExecutionStateSucceeded:
		return true
	case provisioning.ExecutionStateFailed:
		return execution.Failure != nil && execution.Failure.Kind == provisioning.FailureExecution
	default:
		return false
	}
}

func validExecutionState(state provisioning.ExecutionState) bool {
	switch state {
	case provisioning.ExecutionStateAccepted, provisioning.ExecutionStateRunning, provisioning.ExecutionStateSucceeded, provisioning.ExecutionStateFailed, provisioning.ExecutionStateUnknown:
		return true
	default:
		return false
	}
}

func failureFromError(err error) *provisioning.ExecutionFailure {
	var observationErrPointer *provisioning.ObservationError
	if errors.As(err, &observationErrPointer) {
		if observationErrPointer == nil {
			return normalizeExecutionFailure(nil)
		}
		return &observationErrPointer.Failure
	}
	var observationErr provisioning.ObservationError
	if errors.As(err, &observationErr) {
		return &observationErr.Failure
	}
	value := reflect.ValueOf(err)
	if value.Kind() == reflect.Pointer && value.IsNil() {
		return normalizeExecutionFailure(nil)
	}
	return &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "ProvisionerCallFailed", Message: err.Error()}
}

func shouldComplete(observation provisioning.ExecutionObservation) bool {
	return observation.Execution != nil && observation.Execution.State == provisioning.ExecutionStateSucceeded
}

func shouldFail(observation provisioning.ExecutionObservation) bool {
	return observation.Execution != nil && observation.Execution.State == provisioning.ExecutionStateFailed && observation.Execution.Failure != nil
}

func observationTime(observedAt, current time.Time) time.Time {
	if observedAt.IsZero() {
		return current
	}
	return observedAt
}

func observedAt(providerAt, fallback time.Time) time.Time {
	if providerAt.IsZero() {
		return fallback
	}
	return providerAt
}

func validateObservationTime(resource domain.Resource, status domain.ResourceStatus, at time.Time) error {
	if at.IsZero() || at.Before(resource.UpdatedAt()) || at.Before(status.UpdatedAt()) {
		return fmt.Errorf("%w: observation time precedes current resource state", lifecycle.ErrInvalidTransition)
	}
	return nil
}

func validateObservedFacts(facts domain.ObservedFacts) error {
	if !hasObservedFacts(facts) {
		return nil
	}
	if err := facts.Validate(); err != nil {
		return fmt.Errorf("%w: %v", lifecycle.ErrInvalidTransition, err)
	}
	return nil
}

func persistedTerminalFacts(execution ProvisioningExecutionRecord, fallback time.Time) (domain.ObservedFacts, time.Time) {
	if execution.LastObservation != nil {
		return execution.LastObservation.Resource, observedAt(execution.LastObservation.ObservedAt, observationTime(execution.LastObservedAt, fallback))
	}
	if execution.Submission != nil {
		return execution.Submission.Observation.Resource, observedAt(execution.Submission.Observation.ObservedAt, observationTime(execution.LastObservedAt, fallback))
	}
	return domain.ObservedFacts{}, observationTime(execution.LastObservedAt, fallback)
}

func sanitizePersistedFacts(execution *ProvisioningExecutionRecord) bool {
	changed := false
	if execution.LastObservation != nil {
		if validateObservedFacts(execution.LastObservation.Resource) != nil {
			observation := *execution.LastObservation
			observation.Resource = domain.ObservedFacts{}
			execution.LastObservation = &observation
			changed = true
		}
	}
	if execution.Submission != nil {
		if validateObservedFacts(execution.Submission.Observation.Resource) != nil {
			submission := *execution.Submission
			submission.Observation.Resource = domain.ObservedFacts{}
			execution.Submission = &submission
			changed = true
		}
	}
	return changed
}

// absenceCompletedDelete reports persisted proof of a cleanup delete whose
// managed target was conclusively absent before launch: the operation
// succeeded while the correlated submission evidence records the conclusive
// pre-acceptance NotFound rejection.
func absenceCompletedDelete(execution ProvisioningExecutionRecord, operation domain.Operation, evidence *provisioning.Execution) bool {
	return operation.Capability() == domain.CapabilityDelete &&
		operation.State() == domain.OperationStateSucceeded &&
		execution.State == AttemptSucceeded &&
		execution.Correlation == provisioning.RequestCorrelationNotFound &&
		!execution.AcceptanceConfirmed &&
		evidence != nil && evidence.State == provisioning.ExecutionStateFailed &&
		evidence.Failure != nil && evidence.Failure.Kind == provisioning.FailureNotFound
}

func validatePersistedTerminalEvidence(execution ProvisioningExecutionRecord, resource domain.Resource, status domain.ResourceStatus, operation domain.Operation) error {
	evidence := persistedTerminalExecution(execution)
	if evidence == nil {
		return fmt.Errorf("%w: terminal attempt has no terminal execution evidence", ErrInvalidApplicationCall)
	}
	absenceCompletion := absenceCompletedDelete(execution, operation, evidence)
	expected := provisioning.ExecutionStateFailed
	if execution.State == AttemptSucceeded {
		expected = provisioning.ExecutionStateSucceeded
	}
	if !absenceCompletion && evidence.State != expected {
		return fmt.Errorf("%w: terminal attempt contradicts persisted execution evidence", ErrInvalidApplicationCall)
	}
	if execution.LastObservedAt.IsZero() {
		return fmt.Errorf("%w: terminal attempt has no effective observation timestamp", ErrInvalidApplicationCall)
	}
	if !operation.IsTerminal() && (execution.LastObservedAt.Before(resource.UpdatedAt()) || execution.LastObservedAt.Before(status.UpdatedAt())) {
		return fmt.Errorf("%w: terminal evidence predates current resource state", ErrInvalidApplicationCall)
	}
	// Backend-supplied evidence time identifies the source instant of this
	// terminal outcome. It is judged against the backend dimension of the
	// execution timeline; Liftr receipts are a separate clock and may sit
	// later than any backend timestamp without implying staleness.
	if sourceObservedAt := persistedTerminalObservedAt(execution); !sourceObservedAt.IsZero() && !execution.LastProviderObservedAt.IsZero() {
		if !sourceObservedAt.Equal(execution.LastProviderObservedAt) {
			return fmt.Errorf("%w: terminal source timestamp differs from backend evidence time", ErrInvalidApplicationCall)
		}
	}
	if operation.IsTerminal() && !operation.CompletedAt().Equal(execution.LastObservedAt) {
		return fmt.Errorf("%w: terminal operation and execution timestamps differ", ErrInvalidApplicationCall)
	}
	if evidence.State == provisioning.ExecutionStateFailed && !absenceCompletion {
		failure := normalizeExecutionFailure(evidence.Failure)
		if execution.LastFailure == nil || execution.LastFailure.Kind != failure.Kind || execution.LastFailure.Reason != failure.Reason || execution.LastFailure.Message != failure.Message {
			return fmt.Errorf("%w: terminal failure evidence is inconsistent", ErrInvalidApplicationCall)
		}
	}
	outputRejected := execution.State == AttemptSucceeded && execution.OutputResolution == OutputResolutionRejected
	if operation.State() == domain.OperationStateSucceeded && execution.State != AttemptSucceeded {
		return fmt.Errorf("%w: succeeded operation contradicts execution attempt", ErrInvalidApplicationCall)
	}
	if operation.State() == domain.OperationStateFailed && execution.State != AttemptFailed && !outputRejected {
		return fmt.Errorf("%w: failed operation contradicts execution attempt", ErrInvalidApplicationCall)
	}
	if operation.State() == domain.OperationStateFailed {
		operationFailure, ok := operation.Failure()
		if !ok {
			return fmt.Errorf("%w: failed operation carries no failure details", ErrInvalidApplicationCall)
		}
		if outputRejected {
			// Backend success plus a rejected output postcondition is the one
			// honest shape where a failed operation coexists with successful
			// execution evidence. The stored curated reason must match exactly.
			if operationFailure.Reason() != execution.OutputFailureReason || operationFailure.Message() != execution.OutputFailureMessage || execution.OutputFailureReason == "" {
				return fmt.Errorf("%w: failed operation details contradict output postcondition rejection", ErrInvalidApplicationCall)
			}
			return nil
		}
		failure := normalizeExecutionFailure(evidence.Failure)
		if operationFailure.Reason() != failure.Reason || operationFailure.Message() != failure.Message {
			return fmt.Errorf("%w: failed operation details contradict execution evidence", ErrInvalidApplicationCall)
		}
	}
	return nil
}

func persistedTerminalExecution(execution ProvisioningExecutionRecord) *provisioning.Execution {
	if execution.LastObservation != nil && execution.LastObservation.Execution != nil {
		return execution.LastObservation.Execution
	}
	if execution.Submission != nil && execution.Submission.Observation.Execution != nil {
		return execution.Submission.Observation.Execution
	}
	return nil
}

func persistedTerminalObservedAt(execution ProvisioningExecutionRecord) time.Time {
	if execution.LastObservation != nil && execution.LastObservation.Execution != nil {
		return execution.LastObservation.ObservedAt
	}
	if execution.Submission != nil && execution.Submission.Observation.Execution != nil {
		return execution.Submission.Observation.ObservedAt
	}
	return time.Time{}
}

func validateExecutionContext(resource ResourceRecord, operation domain.Operation, execution ProvisioningExecutionRecord) error {
	if operation.ResourceID() != resource.Resource.ID() || execution.OperationID != operation.ID() || execution.ResourceID != resource.Resource.ID() || execution.ResourceType != resource.Resource.Type() || execution.Capability != operation.Capability() || execution.TargetGeneration != operation.TargetGeneration() || execution.ProvisionerRef != resource.ProvisionerRef {
		return fmt.Errorf("%w: provisioning execution associations are inconsistent", ErrInvalidApplicationCall)
	}
	if resource.Resource.Generation() == execution.TargetGeneration && !reflect.DeepEqual(execution.Spec.Values(), resource.Resource.Spec().Values()) {
		return fmt.Errorf("%w: provisioning execution intent does not match the target generation", ErrInvalidApplicationCall)
	}
	return nil
}

func isNilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
