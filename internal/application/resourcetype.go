// SPDX-License-Identifier: Apache-2.0

package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/sithea-nou/liftr/internal/domain"
)

// MaxSpecViolations caps the number of structured violations returned in a
// RESOURCE_SPEC_INVALID problem so error responses stay small and bounded.
const MaxSpecViolations = 10

// ErrInvalidResourceSpec reports that a ResourceSpec does not satisfy the
// developer-facing contract of its ResourceType.
var ErrInvalidResourceSpec = errors.New("resource spec does not satisfy its resource type contract")

// SpecViolation is one sanitized, client-safe spec-contract violation.
// Path is an RFC 6901 JSON Pointer into the submitted spec ("" is the root),
// Keyword names the violated constraint, and Message is a curated sentence.
// Violations never echo raw validator-library output or submitted values.
type SpecViolation struct {
	Path    string `json:"path"`
	Keyword string `json:"keyword"`
	Message string `json:"message"`
}

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
	sort.Slice(violations, func(i, j int) bool {
		left, right := violations[i], violations[j]
		switch {
		case left.Path != right.Path:
			return left.Path < right.Path
		case left.Keyword != right.Keyword:
			return left.Keyword < right.Keyword
		default:
			return left.Message < right.Message
		}
	})
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

// ResourceContract is the consumer-owned developer-facing behavior the
// application needs from a ResourceType. Concrete implementations satisfy it
// structurally; the application never imports them. It exposes only
// developer-intent concepts: identity, display metadata, contract
// capabilities, the domain lifecycle type, spec validation, and the schema
// document used for discovery. Provisioner selection and platform state have
// no representation here.
type ResourceContract interface {
	Ref() domain.ResourceTypeRef
	DisplayName() string
	Description() string
	Capabilities() []domain.Capability
	Domain() domain.ResourceType
	ValidateSpec(domain.ResourceSpec) error
	SpecSchema() json.RawMessage
}

// ResourceTypeCatalog answers "what developer contracts exist?". Provisioner
// selection remains a separate concern on ProvisionerSelector.
type ResourceTypeCatalog interface {
	Get(context.Context, domain.ResourceTypeRef) (ResourceContract, error)
	List(context.Context) ([]ResourceContract, error)
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

// validateCommandSpec enforces the admission boundary: a spec is validated
// against its ResourceType contract before any lifecycle or persistence work.
// Validation is a pure predicate; it performs no defaulting, normalization,
// or mutation of the submitted intent.
func validateCommandSpec(contract ResourceContract, spec domain.ResourceSpec) error {
	err := contract.ValidateSpec(spec)
	if err == nil {
		return nil
	}
	var invalid *InvalidSpecError
	if errors.As(err, &invalid) {
		return invalid
	}
	return fmt.Errorf("resource type spec validation failed unexpectedly: %w", err)
}
