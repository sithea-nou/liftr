// SPDX-License-Identifier: Apache-2.0

package application

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
)

// M21 relationship bounds. They follow the repository's convention of small,
// code-defined limits and are pinned by tests.
const (
	// MaxReferencesPerResource bounds the total number of bound reference
	// targets on one Resource.
	MaxReferencesPerResource = 32
	// MaxDependencyDepth bounds the cycle-detection traversal depth.
	MaxDependencyDepth = 32
	// MaxCycleTraversalNodes bounds the total nodes visited by one cycle
	// proof so adversarially wide graphs fail conservatively instead of
	// exhausting the admission transaction.
	MaxCycleTraversalNodes = 4096
)

// Relationship sentinels. The transport maps them to curated Problems; raw
// causes never reach clients.
var (
	// ErrResourceInUse reports that a target delete was blocked by a live
	// protective reference. It never discloses dependents.
	ErrResourceInUse = errors.New("resource is referenced by another resource")
	// ErrDependencyCycle reports that the proposed desired graph would be
	// cyclic.
	ErrDependencyCycle = errors.New("resource references form a dependency cycle")
	// ErrReferenceGraphLimit reports that acyclicity could not be proven
	// within the supported traversal bounds. It fails closed: an incomplete
	// graph proof is never treated as safe.
	ErrReferenceGraphLimit = errors.New("dependency graph exceeds supported verification bounds")
	// ErrReferenceInvariant reports corrupted protective state — for example
	// reference rows owned by a Deleted source. It fails closed.
	ErrReferenceInvariant = errors.New("reference invariant violated")
)

// InvalidReferenceError carries sanitized reference violations to the
// transport. Paths are stable JSON-Pointer-style locations in the submitted
// references object; messages are curated client-safe sentences that never
// distinguish a missing target from an inaccessible one.
type InvalidReferenceError struct {
	Violations []SpecViolation
}

func (e *InvalidReferenceError) Error() string {
	if len(e.Violations) == 0 {
		return "references are invalid"
	}
	return e.Violations[0].Message
}

// ReferenceEdge is one canonical desired or applied dependency edge.
type ReferenceEdge struct {
	Slot       string            `json:"slot"`
	TargetID   domain.ResourceID `json:"targetId"`
	Generation uint64            `json:"-"`
}

// CanonicalizeReferences normalizes submitted slot->targets input into the
// canonical set form: slots sorted by name, target IDs deduplicated within a
// slot (duplicates are rejected) and sorted byte-wise. Nil input yields an
// empty set. Reordering alone is therefore fingerprint- and storage-neutral.
func CanonicalizeReferences(input map[string][]string) ([]ReferenceEdge, error) {
	if len(input) == 0 {
		return nil, nil
	}
	slots := make([]string, 0, len(input))
	for slot := range input {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	edges := make([]ReferenceEdge, 0)
	total := 0
	for _, slot := range slots {
		targets := input[slot]
		if len(targets) == 0 {
			continue
		}
		seen := make(map[domain.ResourceID]struct{}, len(targets))
		unique := make([]string, 0, len(targets))
		for _, target := range targets {
			id := domain.ResourceID(target)
			if _, exists := seen[id]; exists {
				return nil, &InvalidReferenceError{Violations: []SpecViolation{{
					Path: "/" + slot, Keyword: "duplicate-target",
					Message: fmt.Sprintf("reference slot %q contains duplicate target entries", slot),
				}}}
			}
			seen[id] = struct{}{}
			unique = append(unique, target)
		}
		sort.Strings(unique)
		for _, target := range unique {
			edges = append(edges, ReferenceEdge{Slot: slot, TargetID: domain.ResourceID(target)})
			total++
		}
	}
	if total > MaxReferencesPerResource {
		return nil, &InvalidReferenceError{Violations: []SpecViolation{{
			Path: "", Keyword: "maxItems",
			Message: fmt.Sprintf("a resource may declare at most %d bound references", MaxReferencesPerResource),
		}}}
	}
	return edges, nil
}

// ValidateReferenceShape validates a canonical set against the contract's
// declared slots: unknown slots, cardinality, self-references, and declared
// empty sets on types without slots.
func ValidateReferenceShape(contract *resourcecontract.ReferenceContract, sourceID domain.ResourceID, edges []ReferenceEdge) error {
	if len(edges) == 0 {
		return nil
	}
	if contract == nil {
		return &InvalidReferenceError{Violations: []SpecViolation{{
			Path: "", Keyword: "unknown-slot",
			Message: "the resource type does not declare any reference slots",
		}}}
	}
	counts := make(map[string]int)
	var violations []SpecViolation
	for _, edge := range edges {
		if _, ok := contract.Slot(edge.Slot); !ok {
			violations = append(violations, SpecViolation{
				Path: "/" + edge.Slot, Keyword: "unknown-slot",
				Message: fmt.Sprintf("reference slot %q is not declared by the resource type contract", edge.Slot),
			})
			continue
		}
		if string(edge.TargetID) == string(sourceID) {
			violations = append(violations, SpecViolation{
				Path: "/" + edge.Slot, Keyword: "self-reference",
				Message: "a resource cannot reference itself",
			})
			continue
		}
		counts[edge.Slot]++
	}
	if len(violations) > 0 {
		return &InvalidReferenceError{Violations: violations}
	}
	for slot, count := range counts {
		declaration, _ := contract.Slot(slot)
		if count < declaration.MinItems || count > declaration.MaxItems {
			violations = append(violations, SpecViolation{
				Path: "/" + slot, Keyword: "cardinality",
				Message: fmt.Sprintf("reference slot %q accepts between %d and %d targets", slot, declaration.MinItems, declaration.MaxItems),
			})
		}
	}
	if len(violations) > 0 {
		return &InvalidReferenceError{Violations: violations}
	}
	return nil
}

// SlotTargetTypeAllowed reports whether the exact target ref is allowed.
func SlotTargetTypeAllowed(contract *resourcecontract.ReferenceContract, slot string, ref domain.ResourceTypeRef) bool {
	if contract == nil {
		return false
	}
	declaration, ok := contract.Slot(slot)
	if !ok {
		return false
	}
	return declaration.AllowsTarget(ref)
}

// ReferenceDifference separates two canonical sets into preserved, added, and
// removed edges. Preserved durable edges remain trusted intent: they are
// neither reauthorized nor revalidated on later updates.
func ReferenceDifference(oldSet, newSet []ReferenceEdge) (added, removed []ReferenceEdge) {
	oldKeys := edgeKeySet(oldSet)
	newKeys := edgeKeySet(newSet)
	for _, edge := range newSet {
		if _, exists := oldKeys[edgeKey(edge)]; !exists {
			added = append(added, edge)
		}
	}
	for _, edge := range oldSet {
		if _, exists := newKeys[edgeKey(edge)]; !exists {
			removed = append(removed, edge)
		}
	}
	return added, removed
}

func edgeKey(edge ReferenceEdge) string { return edge.Slot + "\x00" + string(edge.TargetID) }

func edgeKeySet(edges []ReferenceEdge) map[string]struct{} {
	keys := make(map[string]struct{}, len(edges))
	for _, edge := range edges {
		keys[edgeKey(edge)] = struct{}{}
	}
	return keys
}

// EdgesEqual compares two canonical sets for exact equality.
func EdgesEqual(a, b []ReferenceEdge) bool {
	if len(a) != len(b) {
		return false
	}
	set := edgeKeySet(a)
	for _, edge := range b {
		if _, exists := set[edgeKey(edge)]; !exists {
			return false
		}
	}
	return true
}

// DesiredReferenceRepository persists the desired and applied reference sets
// of one Resource. All methods operate inside the caller's transaction.
type ReferenceRepository interface {
	// DesiredReferences returns the source's current desired edges in
	// deterministic (slot, target) order.
	DesiredReferences(ctx context.Context, source domain.ResourceID) ([]ReferenceEdge, error)
	// ReplaceDesiredReferences atomically rewrites the source's desired set
	// at the given generation.
	ReplaceDesiredReferences(ctx context.Context, source domain.ResourceID, generation uint64, edges []ReferenceEdge) error
	// AppliedReferences returns the source's applied edges in deterministic
	// order.
	AppliedReferences(ctx context.Context, source domain.ResourceID) ([]ReferenceEdge, error)
	// AdvanceAppliedReferences atomically replaces the source's applied set
	// with the given edges at the given generation. It runs only inside the
	// terminal-success transaction that proves convergence.
	AdvanceAppliedReferences(ctx context.Context, source domain.ResourceID, generation uint64, edges []ReferenceEdge) error
	// DeleteReferencesForSource removes both sets for one source. It runs
	// only inside the transaction that durably records the source as Deleted.
	DeleteReferencesForSource(ctx context.Context, source domain.ResourceID) error
	// HasInboundProtectiveReference reports whether ANY row of either table
	// names the target. Protective evidence fails closed: if an inbound edge
	// belongs to a Deleted source, normal lifecycle guarantees were violated
	// and ErrReferenceInvariant is returned rather than silently ignoring
	// the corruption.
	HasInboundProtectiveReference(ctx context.Context, target domain.ResourceID) (bool, error)
	// OutgoingReferenceTargets returns the distinct desired-edge targets of
	// one source. It backs bounded cycle traversal.
	OutgoingReferenceTargets(ctx context.Context, source domain.ResourceID) ([]domain.ResourceID, error)
}

// DependencyWaitKind marks why one wait row exists.
const DependencyWaitActive = "active"

// DependencyWait binds one blocked Operation to exactly one blocking target.
type DependencyWait struct {
	OperationID             domain.OperationID
	TargetID                domain.ResourceID
	WaitSequence            uint64
	OperationVersion        uint64
	RegisteredTargetVersion uint64
}

// DependencyWaitRepository persists the private durable wait registrations.
type DependencyWaitRepository interface {
	// RegisterDependencyWaits upserts one wait per (operation, target).
	RegisterDependencyWaits(ctx context.Context, operationID domain.OperationID, operationVersion uint64, targets map[domain.ResourceID]uint64) error
	// DeleteDependencyWaitsForOperation removes every wait of one Operation.
	DeleteDependencyWaitsForOperation(ctx context.Context, operationID domain.OperationID) error
	// HasDependencyWaiterForTarget reports whether any wait row names the
	// target. Gate-relevant transitions consult it before enqueueing wakes.
	HasDependencyWaiterForTarget(ctx context.Context, target domain.ResourceID) (bool, error)
	// HasDependencyWaitsForOperation reports whether any wait row belongs to
	// one Operation. Operator diagnostics consult it to classify
	// dependency-blocked work.
	HasDependencyWaitsForOperation(ctx context.Context, operationID domain.OperationID) (bool, error)
	// PageDependencyWaitersByTarget returns at most limit waits naming the
	// target in ascending WaitSequence order, strictly after afterSequence;
	// zero starts at the oldest. NextSequence is zero when no further page
	// exists. Bounded keyset paging keeps wake fanout safe for unbounded
	// inbound fan-out.
	PageDependencyWaitersByTarget(ctx context.Context, target domain.ResourceID, afterSequence uint64, limit int) (waits []DependencyWait, nextSequence uint64, err error)
}

// DetectDependencyCycle proves that adding edges from source to each candidate
// target keeps the desired graph acyclic. Per Correction 1 it traverses the
// OUTGOING desired edges beginning at each candidate target — the proposed
// edge S->T closes a cycle exactly when T already reaches S through existing
// desired edges. Traversal is bounded in both depth and visited-node budget;
// reaching either bound while work remains fails conservatively with
// ErrReferenceGraphLimit rather than declaring safety from an incomplete
// proof. The caller must hold the owner structural lock so no concurrent
// writer can mutate the traversed graph mid-proof.
func DetectDependencyCycle(ctx context.Context, tx UnitOfWork, source domain.ResourceID, candidateTargets []domain.ResourceID) error {
	targets := append([]domain.ResourceID(nil), candidateTargets...)
	sort.Slice(targets, func(a, b int) bool { return targets[a] < targets[b] })
	visited := make(map[domain.ResourceID]struct{})
	budget := MaxCycleTraversalNodes
	var walk func(node domain.ResourceID, depth int) error
	walk = func(node domain.ResourceID, depth int) error {
		if node == source {
			return ErrDependencyCycle
		}
		if _, seen := visited[node]; seen {
			return nil
		}
		if budget <= 0 {
			return ErrReferenceGraphLimit
		}
		budget--
		visited[node] = struct{}{}
		nexts, err := tx.References().OutgoingReferenceTargets(ctx, node)
		if err != nil {
			return err
		}
		if len(nexts) == 0 {
			return nil
		}
		if depth+1 > MaxDependencyDepth {
			// Further outgoing edges exist but the configured maximum frontier
			// is reached: the search would be truncated, which never proves
			// acyclicity. Fail conservatively.
			return ErrReferenceGraphLimit
		}
		for _, next := range nexts {
			if err := walk(next, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	for _, target := range targets {
		if err := walk(target, 1); err != nil {
			return err
		}
	}
	return nil
}

// distinctEdgeTargets returns the sorted unique target IDs of the edges.
func distinctEdgeTargets(edges []ReferenceEdge) []domain.ResourceID {
	set := make(map[domain.ResourceID]struct{}, len(edges))
	targets := make([]domain.ResourceID, 0, len(edges))
	for _, edge := range edges {
		if _, exists := set[edge.TargetID]; exists {
			continue
		}
		set[edge.TargetID] = struct{}{}
		targets = append(targets, edge.TargetID)
	}
	sort.Slice(targets, func(a, b int) bool { return targets[a] < targets[b] })
	return targets
}

// hiddenTargetViolations renders the single generic refusal used for every
// missing, cross-owner, unauthorized, ineligible, or wrong-typed target so
// reference validation can never become an existence oracle.
func hiddenTargetViolation(slot string) *InvalidReferenceError {
	return &InvalidReferenceError{Violations: []SpecViolation{{
		Path: "/" + slot, Keyword: "target",
		Message: fmt.Sprintf("the target referenced by slot %q is not an available dependency", slot),
	}}}
}

// validateReferenceTargets validates newly added reference intent against the
// locked target records: exact same durable OwnerRef, current-principal read
// authorization on every target, exact allowed target type per slot, and
// admission eligibility (Deleting/Deleted targets are refused; Pending and
// Failed targets are admissible because chains must be creatable and Failed
// Resources still exist and may recover). Every failure — including a missing
// row — renders the SAME generic refusal so reference validation can never
// become an existence oracle.
func (s *Service) validateReferenceTargets(ctx context.Context, tx UnitOfWork, actor identity.Principal, sourceOwner domain.OwnerRef, contract *resourcecontract.ReferenceContract, added []ReferenceEdge) error {
	targets := distinctEdgeTargets(added)
	lockedTargets, err := tx.Resources().LockResources(ctx, targets)
	if err != nil {
		if errors.Is(err, ErrResourceNotFound) {
			return hiddenTargetViolation(added[0].Slot)
		}
		return err
	}
	byID := make(map[domain.ResourceID]ResourceRecord, len(lockedTargets))
	for _, record := range lockedTargets {
		byID[record.Resource.ID()] = record
	}
	for _, edge := range added {
		record, found := byID[edge.TargetID]
		if !found {
			return hiddenTargetViolation(edge.Slot)
		}
		resource := record.Resource
		if resource.Owner() != sourceOwner {
			return hiddenTargetViolation(edge.Slot)
		}
		if !SlotTargetTypeAllowed(contract, edge.Slot, resource.Type()) {
			return hiddenTargetViolation(edge.Slot)
		}
		switch record.Status.State() {
		case domain.ResourceStateUnknown, domain.ResourceStatePending,
			domain.ResourceStateReady, domain.ResourceStateFailed:
			// Eligible at admission; readiness is enforced later by the
			// execution gate, not here.
		default:
			return hiddenTargetViolation(edge.Slot)
		}
		if err := s.authorize(ctx, actor, identity.ActionResourceRead, resourceTargetOf(record)); err != nil {
			return hiddenTargetViolation(edge.Slot)
		}
	}
	return nil
}

// CountDependencyWaitsForTarget returns the total number of wait rows naming
// one target. It backs test instrumentation and operator diagnostics; the wake
// hot path itself uses HasDependencyWaiterForTarget.
func CountDependencyWaitsForTarget(ctx context.Context, tx UnitOfWork, target domain.ResourceID) (int, error) {
	cursor := uint64(0)
	total := 0
	for {
		waits, next, err := tx.DependencyWaits().PageDependencyWaitersByTarget(ctx, target, cursor, 256)
		if err != nil {
			return 0, err
		}
		total += len(waits)
		if next == 0 {
			return total, nil
		}
		cursor = next
	}
}
