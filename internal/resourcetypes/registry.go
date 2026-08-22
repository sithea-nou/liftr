// SPDX-License-Identifier: Apache-2.0

package resourcetypes

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
)

// ErrUnknownResourceType reports that no contract is registered under a
// ResourceTypeRef.
var ErrUnknownResourceType = errors.New("unknown resource type")

// Registry is the deterministic in-memory ResourceType catalog. It answers
// "what developer contracts exist?" and never exposes provisioner selection,
// availability, or any other platform state. Get and List match the
// application port structurally.
type Registry struct {
	mu        sync.RWMutex
	contracts map[domain.ResourceTypeRef]Contract
}

var _ application.ResourceTypeCatalog = (*Registry)(nil)

// NewRegistry registers the given contracts, rejecting duplicates.
func NewRegistry(contracts ...Contract) (*Registry, error) {
	registry := &Registry{contracts: make(map[domain.ResourceTypeRef]Contract)}
	for _, contract := range contracts {
		if err := registry.Register(contract); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

// Register adds one contract. A ResourceTypeRef identifies exactly one
// immutable contract, so re-registering a ref — even with identical content —
// is rejected.
func (r *Registry) Register(contract Contract) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ref := contract.Ref()
	if _, exists := r.contracts[ref]; exists {
		return fmt.Errorf("resource type %s/%s is already registered", ref.Name, ref.Version)
	}
	r.contracts[ref] = contract
	return nil
}

func (r *Registry) Get(_ context.Context, ref domain.ResourceTypeRef) (application.ResourceContract, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	contract, ok := r.contracts[ref]
	if !ok {
		return nil, fmt.Errorf("%w: %s/%s", ErrUnknownResourceType, ref.Name, ref.Version)
	}
	return contract, nil
}

// List returns every registered contract ordered deterministically by name
// ascending, then version ascending (byte-wise). The order is stable across
// calls so every catalog implementation can agree on it.
func (r *Registry) List(_ context.Context) ([]application.ResourceContract, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	refs := make([]domain.ResourceTypeRef, 0, len(r.contracts))
	for ref := range r.contracts {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Name != refs[j].Name {
			return refs[i].Name < refs[j].Name
		}
		return refs[i].Version < refs[j].Version
	})
	contracts := make([]application.ResourceContract, 0, len(refs))
	for _, ref := range refs {
		contracts = append(contracts, r.contracts[ref])
	}
	return contracts, nil
}
