// SPDX-License-Identifier: Apache-2.0

package application

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

// Operator diagnostics (ADR-0021). Every structure is a curated, operator-safe
// projection of durable truth. Structurally absent by construction:
// ResourceSpec wholesale, output values, raw provider diagnostics, outbox
// last_error text, opaque handle contents, state bytes, plans, environment,
// credentials, and JWT/group claims. Private implementation references
// (ProvisionerRef and the OpenTofu binding identities) are included because
// the plane is gated behind operator:diagnostics:read.

// OperationRefSummary identifies an Operation inside Resource diagnostics.
type OperationRefSummary struct {
	ID               domain.OperationID
	Capability       domain.Capability
	State            domain.OperationState
	Phase            domain.OperationPhase
	TargetGeneration uint64
}

// ExecutionDiagnostics summarizes one provisioning execution without handles,
// observations, spec, or failure message text.
type ExecutionDiagnostics struct {
	State                   ProvisioningAttemptState
	Correlation             string
	AcceptanceConfirmed     bool
	HandlePresent           bool
	IsOutputRecovery        bool
	OutputResolution        OutputResolution
	OutputFailureKind       string
	CurrentAttempt          uint64
	NextObservationSequence uint64
}

// AttemptDiagnostic is one immutable submission attempt summary.
type AttemptDiagnostic struct {
	Number uint64
	State  SubmissionAttemptState
	// ClaimedAt non-zero proves a dispatch claim was taken, i.e. the Submit
	// boundary may have been crossed.
	BoundaryCrossed bool
	ClaimedAt       time.Time
	ResolvedAt      time.Time
	// FailureKind is the bounded provider-neutral failure kind or empty. The
	// free-form failure message is deliberately never surfaced.
	FailureKind string
}

// OutstandingWork references one current outbox item for the target.
type OutstandingWork struct {
	ID           string
	Kind         OutboxKind
	State        OutboxState
	CreatedAt    time.Time
	AvailableAt  time.Time
	AttemptCount int
}

// ResourceDiagnostics is the operator diagnostic snapshot for one Resource.
type ResourceDiagnostics struct {
	ResourceID         domain.ResourceID
	TypeName           string
	TypeVersion        string
	OwnerKind          string
	OwnerID            string
	Generation         uint64
	ObservedGeneration uint64
	State              domain.ResourceState
	StatusUpdatedAt    time.Time
	ActiveOperation    *OperationRefSummary
	LatestOperation    *OperationRefSummary
	// Long-running and reconciliation-silence ages are independent diagnostic
	// fields (M17/ADR-0018): activity is not progress and neither implies
	// lifecycle failure.
	OperationAgeSeconds          float64
	ReconciliationSilenceSeconds float64
	OutputResolution             OutputResolution
	ProvisionerRef               ProvisionerRef
	RegistrationAvailable        bool
	StateIdentity                *StateIdentitySummary
	SpecDigest                   string
	SpecDigestAvailable          bool
	Assessment                   RecoveryAssessment
	Revision                     string
}

// OperationDiagnostics is the operator diagnostic snapshot for one Operation.
// History is deliberately NOT returned as collections: only the latest
// attempt, the complete structurally-small active work set, and honest total
// counts are exposed, so response size stays bounded regardless of how long
// the Operation's history grows (ADR-0021).
type OperationDiagnostics struct {
	OperationID      domain.OperationID
	ResourceID       domain.ResourceID
	Capability       domain.Capability
	TargetGeneration uint64
	State            domain.OperationState
	Phase            domain.OperationPhase
	RetryOf          domain.OperationID
	RequestedAt      time.Time
	StartedAt        time.Time
	CompletedAt      time.Time
	AgeSeconds       float64
	Execution        *ExecutionDiagnostics
	// LatestAttempt is the highest-numbered submission attempt; nil when the
	// Operation has none. Older attempts are investigation data outside this
	// snapshot.
	LatestAttempt *AttemptDiagnostic
	// AttemptCount totals every durable attempt for the Operation.
	AttemptCount uint64
	// ActiveWork holds every Pending/Leased outbox row for the Operation,
	// oldest first. Schema uniqueness admits at most one active row per kind;
	// ActiveWorkTruncated would report an unexpected overflow honestly rather
	// than silently hiding rows.
	ActiveWork          []OutstandingWork
	ActiveWorkTruncated bool
	// WorkCount totals every outbox row — active and historical — for the
	// Operation. Counts never enter the diagnostic revision.
	WorkCount int
	// ProvisionerRef and registration availability expose whether the private
	// registration that owns this execution still resolves in composition.
	ProvisionerRef        ProvisionerRef
	RegistrationAvailable bool
	Assessment            RecoveryAssessment
	Revision              string
}

// WorkDiagnostics is the operator diagnostic snapshot for one outbox work item.
type WorkDiagnostics struct {
	WorkID        string
	Kind          OutboxKind
	State         OutboxState
	OperationID   domain.OperationID
	ResourceID    domain.ResourceID
	AttemptNumber uint64
	CreatedAt     time.Time
	AvailableAt   time.Time
	AttemptCount  int
	LeaseActive   bool
	LeaseExpired  bool
	// TerminalReasonClass maps terminal_reason onto a bounded class. Raw
	// last_error text is never returned.
	TerminalReasonClass  string
	TargetTerminal       bool
	ActiveEquivalentWork bool
	Superseded           bool
	Assessment           RecoveryAssessment
	Revision             string
}

// AuthorizeOperatorAction is the exported preflight used by transports so a
// denial answers before any catalog, idempotency, or target lookup happens.
// Use cases repeat authorization inside their transactions.
func (s *Service) AuthorizeOperatorAction(ctx context.Context, principal identity.Principal, action identity.Action, target identity.OperatorTarget) error {
	return s.authorizeOperator(ctx, principal, action, target)
}

func (s *Service) authorizeOperator(ctx context.Context, principal identity.Principal, action identity.Action, target identity.OperatorTarget) error {
	if !identity.ValidOperatorAction(action) || !identity.ValidOperatorTargetKind(target.Kind) {
		return ErrNotAuthorized
	}
	if principal.ID == "" || principal.Kind == "" {
		return ErrNotAuthorized
	}
	if s.OperatorAuthorizer == nil {
		return ErrNotAuthorized
	}
	if err := s.OperatorAuthorizer.AuthorizeOperator(ctx, principal, action, target); err != nil {
		return ErrNotAuthorized
	}
	return nil
}

// revisionOf binds the diagnostic revision to meaningful durable versions
// only. Read-only traffic changes none of these inputs, so ETags are stable
// across reads (ADR-0021).
func revisionOf(parts ...string) string {
	return "diag_v1_" + fingerprintHash(append([]string{"liftr/operator-revision/v1"}, parts...)...)
}

// computeResourceRevision derives the Resource diagnostic revision from
// explicitly supplied durable facts so diagnostic reads and mutating
// transactions derive byte-identical revisions from the same locked rows.
func computeResourceRevision(
	resourceVersion uint64,
	statusUpdatedAt time.Time,
	generation, observedGeneration uint64,
	state domain.ResourceState,
	active, latest *OperationRefSummary,
	binding *StateIdentitySummary,
	execution *ExecutionDiagnostics,
	work WorkHistorySummary,
	registrationAvailable bool,
) string {
	parts := []string{
		"resource", strconv.FormatUint(resourceVersion, 10),
		"status", strconv.FormatInt(statusUpdatedAt.UnixNano(), 10),
		strconv.FormatUint(generation, 10), strconv.FormatUint(observedGeneration, 10),
		string(state),
	}
	appendOperation := func(label string, summary *OperationRefSummary) {
		if summary == nil {
			parts = append(parts, label, "none")
			return
		}
		parts = append(parts, label, string(summary.ID), string(summary.Capability), string(summary.State),
			string(summary.Phase), strconv.FormatUint(summary.TargetGeneration, 10))
	}
	appendOperation("active", active)
	appendOperation("latest", latest)
	if binding != nil {
		parts = append(parts, "binding", strconv.FormatUint(binding.Version, 10))
	}
	if execution != nil {
		parts = append(parts, "execution", string(execution.State), execution.Correlation,
			string(execution.OutputResolution), strconv.FormatUint(execution.CurrentAttempt, 10),
			strconv.FormatUint(execution.NextObservationSequence, 10))
	}
	parts = append(parts, activeWorkParts(work)...)
	parts = append(parts, "registration", boolFlag(registrationAvailable))
	return revisionOf(parts...)
}

// activeWorkParts projects only the ACTIVE work identities into a revision.
// Historical terminal rows are deliberately omitted: their existence cannot
// change current RecoveryPlanner semantics, so appending one must never
// invalidate a held diagnostic validator (ADR-0021).
func activeWorkParts(work WorkHistorySummary) []string {
	parts := make([]string, 0, len(work.Active)*5)
	for _, message := range work.Active {
		parts = append(parts, "active-work", message.ID, string(message.Kind), string(message.State), strconv.Itoa(message.AttemptCount))
	}
	return parts
}

// operationRevision binds the Operation diagnostic revision to CURRENT
// planner-relevant durable facts: Operation state, execution evidence, the
// newest attempt identity, and the active work set. Total attempt and work
// counts are excluded by design — they grow with completed history that
// cannot alter recovery safety (ADR-0021).
func operationRevision(operation OperationRecord, attempts AttemptHistorySummary, work WorkHistorySummary, execution *ExecutionDiagnostics, registrationAvailable bool) string {
	parts := []string{
		"operation", string(operation.Operation.ID()), strconv.FormatUint(operation.Version, 10),
		string(operation.Operation.State()), string(operation.Operation.Phase()),
	}
	if attempts.Count > 0 {
		parts = append(parts, "attempt",
			strconv.FormatUint(attempts.Latest.AttemptNumber, 10), string(attempts.Latest.State))
	}
	parts = append(parts, activeWorkParts(work)...)
	if execution != nil {
		parts = append(parts, "execution", string(execution.State), execution.Correlation,
			string(execution.OutputResolution), strconv.FormatUint(execution.CurrentAttempt, 10),
			strconv.FormatUint(execution.NextObservationSequence, 10), boolFlag(execution.AcceptanceConfirmed),
			boolFlag(execution.HandlePresent), boolFlag(execution.IsOutputRecovery), execution.OutputFailureKind)
	}
	parts = append(parts, "registration", boolFlag(registrationAvailable))
	return revisionOf(parts...)
}

func workRevision(message OutboxMessage, snapshot DeadWorkKindSnapshot) string {
	return revisionOf([]string{
		"work", message.ID, string(message.State), strconv.Itoa(message.AttemptCount),
		message.TerminalReason, boolFlag(message.LeaseToken != ""),
		"target", strconv.FormatUint(snapshot.TargetVersion, 10), string(snapshot.OperationState), string(snapshot.ResourceState),
		"execution", strconv.FormatUint(snapshot.ExecutionVersion, 10), string(snapshot.Execution.State), string(snapshot.Execution.Correlation),
		"facts", boolFlag(snapshot.ActiveEquivalentWork), boolFlag(snapshot.ActiveOperation),
		boolFlag(snapshot.Superseded), boolFlag(snapshot.RegistrationAvailable),
	}...)
}

func boolFlag(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func durationSeconds(d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return d.Seconds()
}

func clampPast(t time.Time, now time.Time) time.Time {
	if t.IsZero() || t.After(now) {
		return now
	}
	return t
}

// resolveRegistration reports whether a private provisioner reference still
// resolves against current composition. Resolve consults only the static
// registry — it never performs a provider call.
func (s *Service) resolveRegistration(ctx context.Context, ref ProvisionerRef) bool {
	provider, err := s.Resolver.Resolve(ctx, ref)
	return err == nil && !isNilInterface(provider)
}

// ResourceOperatorDiagnostics builds the authoritative operator-safe
// diagnostic snapshot for one Resource.
func (s *Service) ResourceOperatorDiagnostics(ctx context.Context, principal identity.Principal, id domain.ResourceID) (ResourceDiagnostics, error) {
	target := identity.OperatorTarget{Kind: identity.OperatorTargetResource, ID: string(id)}
	if err := s.authorizeOperator(ctx, principal, identity.ActionOperatorDiagnosticsRead, target); err != nil {
		return ResourceDiagnostics{}, err
	}
	var diag ResourceDiagnostics
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		record, err := tx.Resources().GetResource(ctx, id)
		if err != nil {
			return err
		}
		diag = ResourceDiagnostics{
			ResourceID:         record.Resource.ID(),
			TypeName:           record.Resource.Type().Name,
			TypeVersion:        record.Resource.Type().Version,
			OwnerKind:          record.Resource.Owner().Kind,
			OwnerID:            record.Resource.Owner().ID,
			Generation:         record.Resource.Generation(),
			State:              record.Status.State(),
			StatusUpdatedAt:    record.Status.UpdatedAt(),
			ObservedGeneration: record.Status.ObservedGeneration(),
			ProvisionerRef:     record.ProvisionerRef,
		}
		activeOpRecord, activeFound, err := tx.Operations().ActiveForResource(ctx, id)
		if err != nil {
			return err
		}
		latestOpRecord, latestFound, err := tx.Operations().LatestForResource(ctx, id)
		if err != nil {
			return err
		}
		now := s.clock()
		var observeSnapshot OperationRecoverySnapshot
		var passiveSnapshot ResourcePassiveObserveSnapshot
		var revisionExecution *ExecutionDiagnostics
		var revisionWork WorkHistorySummary
		passiveSnapshot.ResourceState = record.Status.State()

		if activeFound {
			operation := activeOpRecord.Operation
			diag.ActiveOperation = &OperationRefSummary{
				ID: operation.ID(), Capability: operation.Capability(),
				State: operation.State(), Phase: operation.Phase(),
				TargetGeneration: operation.TargetGeneration(),
			}
			diag.OperationAgeSeconds = durationSeconds(now.Sub(clampPast(operation.RequestedAt(), now)))
			execution, execErr := tx.Executions().GetExecution(ctx, operation.ID())
			if execErr == nil {
				summary := executionSummaryOf(execution)
				execDiag := executionDiagnosticOf(execution)
				revisionExecution = &execDiag
				diag.OutputResolution = summary.OutputResolution
				work, workErr := tx.Outbox().SummarizeWorkByOperation(ctx, operation.ID())
				if workErr != nil {
					return workErr
				}
				revisionWork = work
				lastActivity := lastActivityOf(operation, execution)
				diag.ReconciliationSilenceSeconds = durationSeconds(now.Sub(clampPast(lastActivity, now)))
				diag.RegistrationAvailable = s.resolveRegistration(ctx, execution.ProvisionerRef)
				dependencyBlocked, blockErr := tx.DependencyWaits().HasDependencyWaitsForOperation(ctx, operation.ID())
				if blockErr != nil {
					return blockErr
				}
				observeSnapshot = OperationRecoverySnapshot{
					OperationState:        operation.State(),
					HasExecution:          true,
					Execution:             summary,
					ActiveObserveWork:     work.HasActive(OutboxObserve),
					DependencyBlocked:     dependencyBlocked,
					RegistrationAvailable: diag.RegistrationAvailable,
				}
			} else if errors.Is(execErr, ErrResourceNotFound) || errors.Is(execErr, ErrOperationNotFound) {
				observeSnapshot = OperationRecoverySnapshot{OperationState: operation.State()}
				diag.RegistrationAvailable = false
			} else {
				return execErr
			}
			diag.Assessment = PlanOperationObserve(observeSnapshot)
		} else {
			passiveSnapshot.RegistrationAvailable = s.resolveRegistration(ctx, record.ProvisionerRef)
			work, workErr := tx.Outbox().SummarizeWorkByResource(ctx, id)
			if workErr != nil {
				return workErr
			}
			revisionWork = work
			passiveSnapshot.ActivePassiveObserveWork = work.HasActive(OutboxPassiveObserve)
			diag.RegistrationAvailable = passiveSnapshot.RegistrationAvailable
			diag.Assessment = PlanResourcePassiveObserve(passiveSnapshot)
		}
		if latestFound {
			operation := latestOpRecord.Operation
			diag.LatestOperation = &OperationRefSummary{
				ID: operation.ID(), Capability: operation.Capability(),
				State: operation.State(), Phase: operation.Phase(),
				TargetGeneration: operation.TargetGeneration(),
			}
		}
		identitySummary, found, err := tx.OperatorDiagnostics().StateIdentity(ctx, id)
		if err != nil {
			return err
		}
		if found {
			diag.StateIdentity = &identitySummary
		}
		diag.SpecDigest, diag.SpecDigestAvailable, err = tx.OperatorDiagnostics().SpecDigest(ctx, id)
		if err != nil {
			return err
		}
		diag.Revision = computeResourceRevision(
			record.Version, record.Status.UpdatedAt(), record.Resource.Generation(),
			record.Status.ObservedGeneration(), record.Status.State(),
			diag.ActiveOperation, diag.LatestOperation, diag.StateIdentity,
			revisionExecution, revisionWork, diag.RegistrationAvailable,
		)
		return nil
	})
	if err != nil {
		return ResourceDiagnostics{}, err
	}
	return diag, nil
}

// OperationOperatorDiagnostics builds the authoritative operator-safe
// diagnostic snapshot for one Operation.
func (s *Service) OperationOperatorDiagnostics(ctx context.Context, principal identity.Principal, id domain.OperationID) (OperationDiagnostics, error) {
	target := identity.OperatorTarget{Kind: identity.OperatorTargetOperation, ID: string(id)}
	if err := s.authorizeOperator(ctx, principal, identity.ActionOperatorDiagnosticsRead, target); err != nil {
		return OperationDiagnostics{}, err
	}
	var diag OperationDiagnostics
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		operation, err := tx.Operations().GetOperation(ctx, id)
		if err != nil {
			return err
		}
		op := operation.Operation
		now := s.clock()
		diag = OperationDiagnostics{
			OperationID:      op.ID(),
			ResourceID:       op.ResourceID(),
			Capability:       op.Capability(),
			TargetGeneration: op.TargetGeneration(),
			State:            op.State(),
			Phase:            op.Phase(),
			RequestedAt:      op.RequestedAt(),
			StartedAt:        op.StartedAt(),
			CompletedAt:      op.CompletedAt(),
			RetryOf:          op.RetryOfOperationID(),
			AgeSeconds:       durationSeconds(now.Sub(clampPast(op.RequestedAt(), now))),
		}
		attempts, err := tx.SubmissionAttempts().SummarizeSubmissionAttempts(ctx, id)
		if err != nil {
			return err
		}
		if attempts.Count > 0 {
			attempt := attempts.Latest
			diag.LatestAttempt = &AttemptDiagnostic{
				Number:          attempt.AttemptNumber,
				State:           attempt.State,
				BoundaryCrossed: !attempt.ClaimedAt.IsZero(),
				ClaimedAt:       attempt.ClaimedAt,
				ResolvedAt:      attempt.ResolvedAt,
				FailureKind:     failureKindOf(attempt.Failure),
			}
		}
		diag.AttemptCount = attempts.Count
		work, err := tx.Outbox().SummarizeWorkByOperation(ctx, id)
		if err != nil {
			return err
		}
		for _, message := range work.Active {
			diag.ActiveWork = append(diag.ActiveWork, OutstandingWork{
				ID: message.ID, Kind: message.Kind, State: message.State,
				CreatedAt: message.CreatedAt, AvailableAt: message.AvailableAt,
				AttemptCount: message.AttemptCount,
			})
		}
		diag.WorkCount = workTotal(work)
		diag.ActiveWorkTruncated = len(work.Active) >= WorkActiveLimit
		snapshot := OperationRecoverySnapshot{OperationState: op.State(), RegistrationAvailable: true}
		execution, execErr := tx.Executions().GetExecution(ctx, id)
		switch {
		case execErr == nil:
			summary := executionSummaryOf(execution)
			diag.Execution = &ExecutionDiagnostics{
				State: summary.State, Correlation: string(summary.Correlation),
				AcceptanceConfirmed:     summary.AcceptanceConfirmed,
				HandlePresent:           execution.Handle != nil,
				IsOutputRecovery:        summary.IsOutputRecovery,
				OutputResolution:        summary.OutputResolution,
				OutputFailureKind:       failureKindOf(execution.LastFailure),
				CurrentAttempt:          execution.CurrentAttempt,
				NextObservationSequence: execution.NextObservation,
			}
			diag.ProvisionerRef = execution.ProvisionerRef
			diag.RegistrationAvailable = s.resolveRegistration(ctx, execution.ProvisionerRef)
			snapshot.HasExecution = true
			snapshot.Execution = summary
			snapshot.ActiveObserveWork = work.HasActive(OutboxObserve)
			snapshot.RegistrationAvailable = diag.RegistrationAvailable
		case errors.Is(execErr, ErrResourceNotFound) || errors.Is(execErr, ErrOperationNotFound):
			diag.RegistrationAvailable = false
			snapshot.RegistrationAvailable = false
		default:
			return execErr
		}
		diag.Assessment = PlanOperationObserve(snapshot)
		diag.Revision = operationRevision(operation, attempts, work, diag.Execution, diag.RegistrationAvailable)
		return nil
	})
	if err != nil {
		return OperationDiagnostics{}, err
	}
	return diag, nil
}

// WorkOperatorDiagnostics builds the authoritative operator-safe diagnostic
// snapshot for one outbox work item.
func (s *Service) WorkOperatorDiagnostics(ctx context.Context, principal identity.Principal, workID string) (WorkDiagnostics, error) {
	target := identity.OperatorTarget{Kind: identity.OperatorTargetWork, ID: workID}
	if err := s.authorizeOperator(ctx, principal, identity.ActionOperatorDiagnosticsRead, target); err != nil {
		return WorkDiagnostics{}, err
	}
	var diag WorkDiagnostics
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		message, err := tx.Outbox().GetOutbox(ctx, workID)
		if err != nil {
			return err
		}
		now := s.clock()
		diag = WorkDiagnostics{
			WorkID: message.ID, Kind: message.Kind, State: message.State,
			OperationID: message.OperationID, ResourceID: message.ResourceID,
			AttemptNumber: message.AttemptNumber,
			CreatedAt:     message.CreatedAt, AvailableAt: message.AvailableAt,
			AttemptCount:        message.AttemptCount,
			TerminalReasonClass: terminalReasonClass(message.TerminalReason),
		}
		if message.State == OutboxLeased {
			diag.LeaseActive = message.LeasedUntil.After(now)
			diag.LeaseExpired = !diag.LeaseActive
		}
		snapshot := DeadWorkKindSnapshot{Kind: message.Kind, State: message.State}
		if message.OperationID != "" {
			snapshot.OperationTarget = true
			operation, err := tx.Operations().GetOperation(ctx, message.OperationID)
			if err != nil {
				return err
			}
			diag.TargetTerminal = operationStateIsTerminal(operation.Operation.State())
			snapshot.OperationState = operation.Operation.State()
			snapshot.TargetVersion = operation.Version
			if message.Kind == OutboxDrive {
				resource, resourceErr := tx.Resources().GetResource(ctx, operation.Operation.ResourceID())
				if resourceErr != nil {
					return resourceErr
				}
				diag.ActiveEquivalentWork, err = activeEquivalentOfWork(ctx, tx, message)
				if err != nil {
					return err
				}
				snapshot.ActiveEquivalentWork = diag.ActiveEquivalentWork
				snapshot.RegistrationAvailable = s.resolveRegistration(ctx, resource.ProvisionerRef)
				diag.Superseded = diag.TargetTerminal
				diag.Assessment = PlanDeadWorkRecovery(snapshot)
				diag.Revision = workRevision(message, snapshot)
				return nil
			}
			execution, execErr := tx.Executions().GetExecution(ctx, message.OperationID)
			switch {
			case execErr == nil:
				snapshot.HasExecution = true
				snapshot.Execution = executionSummaryOf(execution)
				snapshot.ExecutionVersion = execution.Version
				if message.Kind == OutboxDispatch {
					snapshot.Superseded = message.AttemptNumber != execution.CurrentAttempt
				}
				if message.Kind == OutboxObserve {
					snapshot.Superseded = message.Sequence == 0 || message.Sequence+1 != execution.NextObservation
				}
				diag.ActiveEquivalentWork, err = activeEquivalentOfWork(ctx, tx, message)
				if err != nil {
					return err
				}
				snapshot.ActiveEquivalentWork = diag.ActiveEquivalentWork
				snapshot.RegistrationAvailable = s.resolveRegistration(ctx, execution.ProvisionerRef)
			case errors.Is(execErr, ErrResourceNotFound) || errors.Is(execErr, ErrOperationNotFound):
				snapshot.RegistrationAvailable = false
			default:
				return execErr
			}
		} else {
			resourceID := message.ResourceID
			record, err := tx.Resources().GetResource(ctx, resourceID)
			if err != nil {
				return err
			}
			snapshot.ResourceState = record.Status.State()
			snapshot.TargetVersion = record.Version
			_, snapshot.ActiveOperation, err = tx.Operations().ActiveForResource(ctx, resourceID)
			if err != nil {
				return err
			}
			diag.TargetTerminal = snapshot.ResourceState == domain.ResourceStateDeleted
			diag.ActiveEquivalentWork, err = activeEquivalentOfWork(ctx, tx, message)
			if err != nil {
				return err
			}
			snapshot.ActiveEquivalentWork = diag.ActiveEquivalentWork
			snapshot.RegistrationAvailable = s.resolveRegistration(ctx, record.ProvisionerRef)
		}
		diag.Superseded = diag.TargetTerminal || snapshot.Superseded
		diag.Assessment = PlanDeadWorkRecovery(snapshot)
		diag.Revision = workRevision(message, snapshot)
		return nil
	})
	if err != nil {
		return WorkDiagnostics{}, err
	}
	return diag, nil
}

// activeEquivalentOfWork reports whether an active row of the dead message's
// recovery-equivalent kind exists for the same aggregate. The check reads the
// bounded active set only — never the aggregate's historical collection.
func activeEquivalentOfWork(ctx context.Context, tx UnitOfWork, message OutboxMessage) (bool, error) {
	equivalentKind := message.Kind
	if message.Kind == OutboxDispatch {
		equivalentKind = OutboxObserve
	}
	var summary WorkHistorySummary
	var err error
	if message.OperationID != "" {
		summary, err = tx.Outbox().SummarizeWorkByOperation(ctx, message.OperationID)
	} else {
		summary, err = tx.Outbox().SummarizeWorkByResource(ctx, message.ResourceID)
	}
	if err != nil {
		return false, err
	}
	for _, row := range summary.Active {
		if row.ID != message.ID && row.Kind == equivalentKind {
			return true, nil
		}
	}
	return false, nil
}

// workTotal sums the state counts into one honest total for display. Counts
// never enter diagnostic revisions.
func workTotal(summary WorkHistorySummary) int {
	total := 0
	for _, count := range summary.Counts {
		total += count
	}
	return total
}

func executionSummaryOf(execution ProvisioningExecutionRecord) executionObservationSummary {
	return executionObservationSummary{
		State:               execution.State,
		Correlation:         execution.Correlation,
		OutputResolution:    execution.OutputResolution,
		IsOutputRecovery:    execution.IsOutputRecovery(),
		AcceptanceConfirmed: execution.AcceptanceConfirmed,
	}
}

func failureKindOf(failure *provisioning.ExecutionFailure) string {
	if failure == nil {
		return ""
	}
	return string(failure.Kind)
}

func lastActivityOf(operation domain.Operation, execution ProvisioningExecutionRecord) time.Time {
	candidates := []time.Time{operation.RequestedAt()}
	if !operation.PhaseChangedAt().IsZero() {
		candidates = append(candidates, operation.PhaseChangedAt())
	}
	if !operation.StartedAt().IsZero() {
		candidates = append(candidates, operation.StartedAt())
	}
	if !execution.LastObservedAt.IsZero() {
		candidates = append(candidates, execution.LastObservedAt)
	}
	newest := candidates[0]
	for _, candidate := range candidates[1:] {
		if candidate.After(newest) {
			newest = candidate
		}
	}
	return newest
}

// knownTerminalReasons enumerates every curated completion reason the worker
// persists. Anything else collapses to UNCLASSIFIED without echoing content.
var knownTerminalReasons = map[string]bool{
	"StaleWork": true, "StaleDrive": true, "AlreadyDispatchable": true, "Driven": true,
	"Submitted": true, "AmbiguousSubmission": true, "OutputsPending": true,
	"OutputPostconditionRejected": true, "TerminalExecution": true,
	"ObservedNoCurrentExecution": true, "ObservedUncorrelatedExecution": true,
	"ObservedNonterminal": true, "StaleObservation": true, "SubmissionNotFound": true,
	"PassivelyObserved": true, "DispatchResultPersistenceFailed": true,
	"StaleExpiredDispatch": true, "LeaseExpiredAmbiguous": true, "ExpiredFencedDispatch": true,
}

func terminalReasonClass(reason string) string {
	if reason == "" {
		return ""
	}
	if knownTerminalReasons[reason] {
		return reason
	}
	return "UNCLASSIFIED"
}
