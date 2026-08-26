// SPDX-License-Identifier: Apache-2.0

package fake

import (
	"context"
	"fmt"
	"sort"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
)

func cloneReferenceEdges(source map[domain.ResourceID][]application.ReferenceEdge) map[domain.ResourceID][]application.ReferenceEdge {
	cloned := make(map[domain.ResourceID][]application.ReferenceEdge, len(source))
	for id, edges := range source {
		cloned[id] = append([]application.ReferenceEdge(nil), edges...)
	}
	return cloned
}

// LockResources serializes through the store mutex (strictly stronger than the
// durable per-row locks) and returns records in deterministic ID order.
func (s *Store) LockResources(_ context.Context, ids []domain.ResourceID) ([]application.ResourceRecord, error) {
	ordered := append([]domain.ResourceID(nil), ids...)
	sort.Slice(ordered, func(a, b int) bool { return ordered[a] < ordered[b] })
	records := make([]application.ResourceRecord, 0, len(ordered))
	for _, id := range ordered {
		record, ok := s.resources[id]
		if !ok {
			return nil, application.ErrResourceNotFound
		}
		records = append(records, record)
	}
	return records, nil
}

func (s *Store) DesiredReferences(_ context.Context, source domain.ResourceID) ([]application.ReferenceEdge, error) {
	return append([]application.ReferenceEdge(nil), s.desiredReferences[source]...), nil
}

func (s *Store) AppliedReferences(_ context.Context, source domain.ResourceID) ([]application.ReferenceEdge, error) {
	return append([]application.ReferenceEdge(nil), s.appliedReferences[source]...), nil
}

func (s *Store) ReplaceDesiredReferences(_ context.Context, source domain.ResourceID, generation uint64, edges []application.ReferenceEdge) error {
	stored := make([]application.ReferenceEdge, 0, len(edges))
	for _, edge := range edges {
		stored = append(stored, application.ReferenceEdge{Slot: edge.Slot, TargetID: edge.TargetID, Generation: generation})
	}
	if len(stored) == 0 {
		delete(s.desiredReferences, source)
		return nil
	}
	s.desiredReferences[source] = stored
	return nil
}

func (s *Store) AdvanceAppliedReferences(_ context.Context, source domain.ResourceID, generation uint64, edges []application.ReferenceEdge) error {
	stored := make([]application.ReferenceEdge, 0, len(edges))
	for _, edge := range edges {
		stored = append(stored, application.ReferenceEdge{Slot: edge.Slot, TargetID: edge.TargetID, Generation: generation})
	}
	if len(stored) == 0 {
		delete(s.appliedReferences, source)
		return nil
	}
	s.appliedReferences[source] = stored
	return nil
}

func (s *Store) DeleteReferencesForSource(_ context.Context, source domain.ResourceID) error {
	delete(s.desiredReferences, source)
	delete(s.appliedReferences, source)
	return nil
}

// HasInboundProtectiveReference mirrors the fail-closed protective rule:
// any inbound row is protective evidence; rows owned by a Deleted source are
// invariant corruption and refuse the delete.
func (s *Store) HasInboundProtectiveReference(_ context.Context, target domain.ResourceID) (bool, error) {
	protective := 0
	for _, source := range inboundSources(s.desiredReferences, target) {
		record, ok := s.resources[source]
		if !ok || record.Status.State() == domain.ResourceStateDeleted {
			return false, fmt.Errorf("%w: desired reference row belongs to a Deleted or missing source", application.ErrReferenceInvariant)
		}
		protective++
	}
	for _, source := range inboundSources(s.appliedReferences, target) {
		record, ok := s.resources[source]
		if !ok || record.Status.State() == domain.ResourceStateDeleted {
			return false, fmt.Errorf("%w: applied reference row belongs to a Deleted or missing source", application.ErrReferenceInvariant)
		}
		protective++
	}
	return protective > 0, nil
}

func inboundSources(table map[domain.ResourceID][]application.ReferenceEdge, target domain.ResourceID) []domain.ResourceID {
	var sources []domain.ResourceID
	for source, edges := range table {
		for _, edge := range edges {
			if edge.TargetID == target {
				sources = append(sources, source)
				break
			}
		}
	}
	sort.Slice(sources, func(a, b int) bool { return sources[a] < sources[b] })
	return sources
}

func (s *Store) OutgoingReferenceTargets(_ context.Context, source domain.ResourceID) ([]domain.ResourceID, error) {
	set := map[domain.ResourceID]struct{}{}
	for _, edge := range s.desiredReferences[source] {
		set[edge.TargetID] = struct{}{}
	}
	targets := make([]domain.ResourceID, 0, len(set))
	for target := range set {
		targets = append(targets, target)
	}
	sort.Slice(targets, func(a, b int) bool { return targets[a] < targets[b] })
	return targets, nil
}

func waitKey(operationID domain.OperationID, target domain.ResourceID) string {
	return string(operationID) + "\x00" + string(target)
}

func (s *Store) RegisterDependencyWaits(_ context.Context, operationID domain.OperationID, operationVersion uint64, targets map[domain.ResourceID]uint64) error {
	for target, targetVersion := range targets {
		key := waitKey(operationID, target)
		wait, exists := s.dependencyWaits[key]
		if !exists {
			s.nextWaitSequence++
			wait = application.DependencyWait{WaitSequence: s.nextWaitSequence}
		}
		wait.OperationID = operationID
		wait.TargetID = target
		wait.OperationVersion = operationVersion
		wait.RegisteredTargetVersion = targetVersion
		s.dependencyWaits[key] = wait
	}
	return nil
}

func (s *Store) DeleteDependencyWaitsForOperation(_ context.Context, operationID domain.OperationID) error {
	prefix := string(operationID) + "\x00"
	for key := range s.dependencyWaits {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			delete(s.dependencyWaits, key)
		}
	}
	return nil
}

func (s *Store) HasDependencyWaiterForTarget(_ context.Context, target domain.ResourceID) (bool, error) {
	for _, wait := range s.dependencyWaits {
		if wait.TargetID == target {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) HasDependencyWaitsForOperation(_ context.Context, operationID domain.OperationID) (bool, error) {
	prefix := string(operationID) + "\x00"
	for key := range s.dependencyWaits {
		if len(key) > len(prefix) && key[:len(prefix)] == prefix {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) PageDependencyWaitersByTarget(_ context.Context, target domain.ResourceID, afterSequence uint64, limit int) ([]application.DependencyWait, uint64, error) {
	waits := make([]application.DependencyWait, 0, limit)
	for _, wait := range s.dependencyWaits {
		if wait.TargetID != target || wait.WaitSequence <= afterSequence {
			continue
		}
		waits = append(waits, wait)
	}
	sort.Slice(waits, func(a, b int) bool { return waits[a].WaitSequence < waits[b].WaitSequence })
	next := uint64(0)
	if len(waits) > limit {
		waits = waits[:limit]
	}
	if len(waits) > 0 {
		next = waits[len(waits)-1].WaitSequence
		highest := uint64(0)
		for _, wait := range s.dependencyWaits {
			if wait.TargetID == target && wait.WaitSequence > highest {
				highest = wait.WaitSequence
			}
		}
		if highest <= next {
			next = 0
		}
	}
	return waits, next, nil
}
