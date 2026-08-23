// SPDX-License-Identifier: Apache-2.0

package application

import (
	"context"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
)

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
		stored, err := tx.Operations().GetOperation(ctx, id)
		if err != nil {
			return err
		}
		resource, err := tx.Resources().GetResource(ctx, stored.Operation.ResourceID())
		if err != nil {
			return err
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
		return nil
	})
	if err != nil {
		return ResourceView{}, err
	}
	return view, nil
}
