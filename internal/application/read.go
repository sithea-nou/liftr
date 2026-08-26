// SPDX-License-Identifier: Apache-2.0

package application

import (
	"context"
	"fmt"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
)

const MaxResourceOperationPageSize = 100

// ResourceView is the read model for one Resource together with its latest
// Operation and, when published, its realized outputs. Latest is nil when the
// Resource has no Operation yet.
type ResourceView struct {
	Resource ResourceRecord
	Latest   *domain.Operation
	// Outputs is the latest published output snapshot. It is nil when no
	// snapshot has been published or when the Resource is a Deleted tombstone:
	// deleted endpoints are never exposed, while immutable internal history is
	// retained for audit and recovery.
	Outputs *domain.ResourceOutputs
	// References is the current canonical DESIRED reference set (M21).
	// Applied references are internal protective evidence and are never
	// exposed publicly; observedGeneration already communicates whether the
	// desired generation has converged.
	References []ReferenceEdge
}

// GetResource reads the current stored state of one Resource for an
// authenticated principal. The stored owner is authorized before any
// representation is produced; a denial is externally indistinguishable from
// Resource absence (ADR-0012). It performs no lifecycle or provisioning work.
func (s *Service) GetResource(ctx context.Context, principal identity.Principal, id domain.ResourceID) (ResourceRecord, error) {
	var record ResourceRecord
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		var err error
		record, err = tx.Resources().GetResource(ctx, id)
		if err != nil {
			return err
		}
		return s.authorize(ctx, principal, identity.ActionResourceRead, resourceTargetOf(record))
	})
	if err != nil {
		return ResourceRecord{}, err
	}
	return record, nil
}

// GetOperation reads the current stored state of one Operation for an
// authenticated principal. Operations have no independent access policy: the
// owning Resource's stored owner is authorized with resource:read (ADR-0012).
func (s *Service) GetOperation(ctx context.Context, principal identity.Principal, id domain.OperationID) (OperationRecord, error) {
	var record OperationRecord
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		preflight, err := tx.Operations().LookupOperation(ctx, id)
		if err != nil {
			return err
		}
		resource, err := tx.Resources().GetResource(ctx, preflight.Operation.ResourceID())
		if err != nil {
			return err
		}
		stored, err := tx.Operations().GetOperation(ctx, id)
		if err != nil {
			return err
		}
		if stored.Operation.ResourceID() != resource.Resource.ID() {
			return ErrConcurrencyConflict
		}
		if err := s.authorize(ctx, principal, identity.ActionResourceRead, resourceTargetOf(resource)); err != nil {
			return err
		}
		record = stored
		return nil
	})
	if err != nil {
		return OperationRecord{}, err
	}
	return record, nil
}

// ListResourceOperations returns one insertion-ordered page of Operation
// history after authorizing resource:read against the Resource's stored owner.
// beforeSequence is private pagination state supplied by a trusted transport;
// it is never part of an Operation representation.
func (s *Service) ListResourceOperations(ctx context.Context, principal identity.Principal, id domain.ResourceID, beforeSequence uint64, limit int) (OperationPage, error) {
	var page OperationPage
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		resource, err := tx.Resources().GetResource(ctx, id)
		if err != nil {
			return err
		}
		if err := s.authorize(ctx, principal, identity.ActionResourceRead, resourceTargetOf(resource)); err != nil {
			return err
		}
		if limit < 1 || limit > MaxResourceOperationPageSize {
			return fmt.Errorf("%w: operation page limit must be between 1 and %d", ErrInvalidApplicationCall, MaxResourceOperationPageSize)
		}
		page, err = tx.Operations().PageForResource(ctx, id, beforeSequence, limit)
		return err
	})
	if err != nil {
		return OperationPage{}, err
	}
	if page.Records == nil {
		page.Records = []OperationRecord{}
	}
	return page, nil
}

// GetResourceOperation reads a consistent view of one Resource, its latest
// Operation as defined by the deterministic LatestForResource ordering, and
// its publicly visible outputs — after authorizing the principal against the
// stored owner. Non-secret outputs follow resource:read; the future
// secret:resolve action stays independently authorized (ADR-0011/0012).
func (s *Service) GetResourceOperation(ctx context.Context, principal identity.Principal, id domain.ResourceID) (ResourceView, error) {
	var view ResourceView
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		record, err := tx.Resources().GetResource(ctx, id)
		if err != nil {
			return err
		}
		if err := s.authorize(ctx, principal, identity.ActionResourceRead, resourceTargetOf(record)); err != nil {
			return err
		}
		latest, found, err := tx.Operations().LatestForResource(ctx, id)
		if err != nil {
			return err
		}
		view = ResourceView{Resource: record}
		if found {
			operation := latest.Operation
			view.Latest = &operation
		}
		if record.Status.State() != domain.ResourceStateDeleted {
			outputRecord, found, err := tx.Outputs().LatestResourceOutputs(ctx, id)
			if err != nil {
				return err
			}
			if found {
				outputs := outputRecord.Values
				view.Outputs = &outputs
			}
		}
		references, err := tx.References().DesiredReferences(ctx, id)
		if err != nil {
			return err
		}
		view.References = references
		return nil
	})
	if err != nil {
		return ResourceView{}, err
	}
	return view, nil
}
