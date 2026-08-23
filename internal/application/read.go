// SPDX-License-Identifier: Apache-2.0

package application

import (
	"context"

	"github.com/sithea-nou/liftr/internal/domain"
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

// GetResource reads the current stored state of one Resource. It performs no
// lifecycle or provisioning work.
func (s *Service) GetResource(ctx context.Context, id domain.ResourceID) (ResourceRecord, error) {
	var record ResourceRecord
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		var err error
		record, err = tx.Resources().GetResource(ctx, id)
		return err
	})
	if err != nil {
		return ResourceRecord{}, err
	}
	return record, nil
}

// GetOperation reads the current stored state of one Operation.
func (s *Service) GetOperation(ctx context.Context, id domain.OperationID) (OperationRecord, error) {
	var record OperationRecord
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		var err error
		record, err = tx.Operations().GetOperation(ctx, id)
		return err
	})
	if err != nil {
		return OperationRecord{}, err
	}
	return record, nil
}

// GetResourceOperation reads a consistent view of one Resource, its latest
// Operation as defined by the deterministic LatestForResource ordering, and
// its publicly visible outputs.
func (s *Service) GetResourceOperation(ctx context.Context, id domain.ResourceID) (ResourceView, error) {
	var view ResourceView
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		record, err := tx.Resources().GetResource(ctx, id)
		if err != nil {
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
