// SPDX-License-Identifier: Apache-2.0

package application

import (
	"context"
	"fmt"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/lifecycle"
)

// M21 dependency classification. It is a closed, provider-neutral vocabulary
// derived exclusively from durable domain state — never from provisioner kind.
type DependencyClass string

const (
	DependencyReady           DependencyClass = "READY"
	DependencyWaiting         DependencyClass = "WAIT"
	DependencyTerminalFailure DependencyClass = "TERMINAL_DEPENDENCY_FAILURE"
	DependencyInvalid         DependencyClass = "INVALID"
)

// DependencyEvaluation is the outcome of one gate evaluation over the source's
// current desired references at its exact generation.
type DependencyEvaluation struct {
	Class DependencyClass
	// Blocking holds the targets that are not yet READY. It is empty exactly
	// when Class is DependencyReady.
	Blocking []domain.ResourceID
	// TargetVersions carries the record version observed per evaluated target,
	// used for wait registration provenance.
	TargetVersions map[domain.ResourceID]uint64
}

// EvaluateDependencies is the pre-Submit execution gate (ADR-0022). The caller
// must already hold the source Resource row lock and MUST then have locked
// every desired target row in ascending ID order through
// Resources().LockResources before calling this function: readiness facts are
// read after the locks so wait registration cannot lose a concurrent target
// transition (the lost-wake invariant). No provider call may occur while the
// rows are held. The owner admission lock is deliberately NOT taken here:
// worker gating never serializes through it.
func EvaluateDependencies(ctx context.Context, tx UnitOfWork, types ResourceTypeCatalog, resource ResourceRecord) (DependencyEvaluation, bool, error) {
	if types == nil {
		return DependencyEvaluation{}, false, nil
	}
	contract, err := types.Get(ctx, resource.Resource.Type())
	if err != nil {
		return DependencyEvaluation{}, false, fmt.Errorf("%w: %v", ErrResourceTypeNotFound, err)
	}
	referenceContract := contract.ReferenceContract()
	if referenceContract == nil {
		return DependencyEvaluation{}, false, nil
	}
	desired, err := tx.References().DesiredReferences(ctx, resource.Resource.ID())
	if err != nil {
		return DependencyEvaluation{}, false, err
	}
	// Desired rows are rewritten atomically with generation increments; any
	// stale-generation residue would be persistence corruption and fails
	// closed rather than gating against the wrong intent.
	for _, edge := range desired {
		if edge.Generation != resource.Resource.Generation() {
			return DependencyEvaluation{}, false, fmt.Errorf("%w: desired reference generation %d does not match resource generation %d",
				ErrReferenceInvariant, edge.Generation, resource.Resource.Generation())
		}
	}
	if len(desired) == 0 {
		return DependencyEvaluation{}, false, nil
	}
	targets := distinctEdgeTargets(desired)
	lockedTargets, err := tx.Resources().LockResources(ctx, targets)
	if err != nil {
		return DependencyEvaluation{}, false, err
	}
	evaluation := DependencyEvaluation{Class: DependencyReady, TargetVersions: map[domain.ResourceID]uint64{}}
	for _, target := range lockedTargets {
		class := classifyDependency(ctx, tx, types, target)
		evaluation.TargetVersions[target.Resource.ID()] = target.Version
		switch class {
		case DependencyReady:
			continue
		case DependencyWaiting:
			evaluation.Class = DependencyWaiting
			evaluation.Blocking = append(evaluation.Blocking, target.Resource.ID())
		case DependencyTerminalFailure:
			if evaluation.Class != DependencyInvalid {
				evaluation.Class = DependencyTerminalFailure
			}
			evaluation.Blocking = append(evaluation.Blocking, target.Resource.ID())
		default:
			evaluation.Class = DependencyInvalid
			evaluation.Blocking = append(evaluation.Blocking, target.Resource.ID())
		}
	}
	return evaluation, true, nil
}

// classifyDependency maps one locked target's durable state to the closed
// class vocabulary. Convergence requires the generation-matched Reconciled
// condition plus published required outputs — ObservedGeneration advances at
// Request admission and therefore proves nothing about convergence.
func classifyDependency(ctx context.Context, tx UnitOfWork, types ResourceTypeCatalog, target ResourceRecord) DependencyClass {
	state := target.Status.State()
	switch state {
	case domain.ResourceStateDeleting, domain.ResourceStateDeleted:
		return DependencyInvalid
	case domain.ResourceStatePending, domain.ResourceStateUnknown:
		return DependencyWaiting
	case domain.ResourceStateFailed, domain.ResourceStateReady:
		// Fall through to the shared analysis below.
	default:
		return DependencyInvalid
	}

	var requiredOutputs bool
	if contract, err := types.Get(ctx, target.Resource.Type()); err == nil && contract != nil {
		if outputContract := contract.OutputContract(); outputContract != nil {
			requiredOutputs = outputContract.RequiresOutputs()
		}
	}
	activeOperation, hasActive, err := tx.Operations().ActiveForResource(ctx, target.Resource.ID())
	if err != nil {
		return DependencyInvalid
	}
	if hasActive && activeOperation.Operation.IsTerminal() {
		hasActive = false
	}
	// A Failed target with an active retry/output-recovery Operation is still
	// converging: WAIT, never terminal failure.
	if state == domain.ResourceStateFailed && !hasActive {
		return DependencyTerminalFailure
	}
	if state != domain.ResourceStateReady {
		return DependencyWaiting
	}
	reconciled := false
	for _, condition := range target.Status.Conditions() {
		if condition.Type() == lifecycle.ConditionReconciled &&
			condition.Status() == domain.ConditionStatusTrue &&
			condition.ObservedGeneration() == target.Resource.Generation() {
			reconciled = true
			break
		}
	}
	if !reconciled || hasActive {
		return DependencyWaiting
	}
	if requiredOutputs {
		record, found, err := tx.Outputs().LatestResourceOutputs(ctx, target.Resource.ID())
		if err != nil || !found || record.Values.ObservedGeneration() != target.Resource.Generation() {
			return DependencyWaiting
		}
	}
	return DependencyReady
}
