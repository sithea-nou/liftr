// SPDX-License-Identifier: Apache-2.0

package application

import (
	"context"
	"errors"
	"fmt"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
)

// MaxSpecViolations caps the number of structured violations returned in a
// RESOURCE_SPEC_INVALID problem so error responses stay small and bounded.
const MaxSpecViolations = 10

// ErrInvalidResourceSpec reports that a ResourceSpec does not satisfy the
// developer-facing contract of its ResourceType. It aliases the neutral
// sentinel so producer and consumer sentinels remain interchangeable.
var ErrInvalidResourceSpec = resourcecontract.ErrInvalidSpec

// SpecViolation is one sanitized, client-safe spec-contract violation. It
// aliases the neutral violation vocabulary so concrete ResourceTypes never
// need to import this package to author violations.
type SpecViolation = resourcecontract.Violation

// ResourceContract is the developer-facing contract interface consumed by the
// application. It aliases the neutral shared vocabulary owned by the
// resourcecontract package; concrete ResourceType implementations satisfy it
// structurally and never import this package.
type ResourceContract = resourcecontract.Contract

// InvalidSpecError carries the structured violations that caused admission to
// reject a ResourceSpec. It never contains implementation internals.
type InvalidSpecError struct {
	TypeRef    domain.ResourceTypeRef
	Violations []SpecViolation
	Truncated  bool
}

// SortSpecViolations orders violations deterministically by path, then
// keyword, then message. Every violation producer applies this order so
// repeated validations of identical input always agree.
func SortSpecViolations(violations []SpecViolation) {
	resourcecontract.SortViolations(violations)
}

// NewInvalidSpecError normalizes raw violations into the stable public shape:
// duplicates removed, deterministic order by path, keyword, then message, and
// results capped at MaxSpecViolations with Truncated reporting the overflow.
func NewInvalidSpecError(ref domain.ResourceTypeRef, violations []SpecViolation) *InvalidSpecError {
	unique := make([]SpecViolation, 0, len(violations))
	seen := make(map[SpecViolation]struct{}, len(violations))
	for _, violation := range violations {
		if _, exists := seen[violation]; exists {
			continue
		}
		seen[violation] = struct{}{}
		unique = append(unique, violation)
	}
	SortSpecViolations(unique)
	truncated := len(unique) > MaxSpecViolations
	if truncated {
		unique = unique[:MaxSpecViolations]
	}
	return &InvalidSpecError{TypeRef: ref, Violations: unique, Truncated: truncated}
}

func (e *InvalidSpecError) Error() string {
	return fmt.Sprintf("spec does not satisfy the %s/%s resource type contract (%d violation(s))",
		e.TypeRef.Name, e.TypeRef.Version, len(e.Violations))
}

func (e *InvalidSpecError) Is(target error) bool { return target == ErrInvalidResourceSpec }

// invalidSpecFromContract converts a neutral producer-side rejection into the
// bounded public shape. Response bounding — deduplication happened at the
// producer, capping happens here — is consuming-application policy.
func invalidSpecFromContract(rejection *resourcecontract.ValidationError) *InvalidSpecError {
	capped := NewInvalidSpecError(rejection.TypeRef, rejection.Violations)
	return &InvalidSpecError{TypeRef: capped.TypeRef, Violations: capped.Violations, Truncated: capped.Truncated}
}

// ResourceTypeCatalog answers "what developer contracts exist?". Provisioner
// selection remains a separate concern on ProvisionerSelector. Contracts are
// expressed through the neutral shared interface so concrete registries
// satisfy this port structurally without importing application.
type ResourceTypeCatalog interface {
	Get(context.Context, domain.ResourceTypeRef) (resourcecontract.Contract, error)
	List(context.Context) ([]resourcecontract.Contract, error)
}

// GetResourceType reads one registered developer contract for discovery.
// Catalog lookup failures are reported as unknown resource types; catalog
// availability is never part of the developer contract.
func (s *Service) GetResourceType(ctx context.Context, ref domain.ResourceTypeRef) (ResourceContract, error) {
	contract, err := s.Types.Get(ctx, ref)
	if err != nil || isNilInterface(contract) {
		return nil, fmt.Errorf("%w: %v", ErrResourceTypeNotFound, err)
	}
	return contract, nil
}

// ListResourceTypes reads every registered developer contract in the
// catalog's deterministic order.
func (s *Service) ListResourceTypes(ctx context.Context) ([]ResourceContract, error) {
	return s.Types.List(ctx)
}

// asContractRejection reports whether err is an expected contract rejection
// and returns its bounded public form. Producer rejections arrive through the
// neutral ValidationError channel; legacy in-test producers may still supply
// the application-shaped error directly. Any other error is unexpected.
func asContractRejection(err error) (*InvalidSpecError, bool) {
	var neutral *resourcecontract.ValidationError
	if errors.As(err, &neutral) && neutral != nil {
		return invalidSpecFromContract(neutral), true
	}
	var invalid *InvalidSpecError
	if errors.As(err, &invalid) && invalid != nil {
		return invalid, true
	}
	return nil, false
}

// validateCommandSpec enforces the admission boundary: a spec is validated
// against its ResourceType contract before any lifecycle or persistence work.
// Validation is a pure predicate; it performs no defaulting, normalization,
// or mutation of the submitted intent.
func validateCommandSpec(contract ResourceContract, spec domain.ResourceSpec) error {
	err := contract.ValidateSpec(spec)
	if err == nil {
		return nil
	}
	if invalid, ok := asContractRejection(err); ok {
		return invalid
	}
	return fmt.Errorf("resource type spec validation failed unexpectedly: %w", err)
}

// validateCommandTransition enforces the update-admission boundary: an old→new
// transition is validated against the contract's declared update-transition
// rules after the new spec validates on its own and before any desired-state
// mutation. Rejections use the same structured InvalidSpecError channel as
// spec validation.
func validateCommandTransition(contract ResourceContract, oldSpec, newSpec domain.ResourceSpec) error {
	err := contract.ValidateUpdate(oldSpec, newSpec)
	if err == nil {
		return nil
	}
	if invalid, ok := asContractRejection(err); ok {
		return invalid
	}
	return fmt.Errorf("resource type transition validation failed unexpectedly: %w", err)
}
