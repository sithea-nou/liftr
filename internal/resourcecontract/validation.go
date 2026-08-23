// SPDX-License-Identifier: Apache-2.0

// Package resourcecontract owns the provider-neutral vocabulary shared by
// ResourceType implementations and the application that consumes them: the
// developer-contract interface, structured spec-validation rejections, and
// output-field descriptors.
//
// The package is deliberately narrow. It imports only the domain and the
// standard library. It never imports application, HTTP, persistence,
// provisioning, JSON Schema libraries, or any provisioner technology, so both
// sides of the boundary can depend on it without creating cycles or leaking
// implementation concerns into concrete ResourceType packages.
package resourcecontract

import (
	"errors"
	"fmt"
	"sort"

	"github.com/sithea-nou/liftr/internal/domain"
)

// ErrInvalidSpec reports that a ResourceSpec does not satisfy the
// developer-facing contract of its ResourceType.
var ErrInvalidSpec = errors.New("resource spec does not satisfy its resource type contract")

// Violation is one sanitized, client-safe contract violation. Path is an
// RFC 6901 JSON Pointer into the submitted spec ("" is the root), Keyword
// names the violated constraint, and Message is a curated sentence.
// Violations never echo raw validator-library output or submitted values.
type Violation struct {
	Path    string `json:"path"`
	Keyword string `json:"keyword"`
	Message string `json:"message"`
}

// ValidationError carries the structured violations that caused a ResourceSpec
// rejection under one ResourceType contract. Producers return it from
// ValidateSpec and ValidateUpdate; the consuming application converts it into
// its own bounded public error shape. It never contains implementation
// internals.
type ValidationError struct {
	TypeRef    domain.ResourceTypeRef
	Violations []Violation
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("spec does not satisfy the %s/%s resource type contract (%d violation(s))",
		e.TypeRef.Name, e.TypeRef.Version, len(e.Violations))
}

func (e *ValidationError) Is(target error) bool { return target == ErrInvalidSpec }

// SortViolations orders violations deterministically by path, then keyword,
// then message. Every violation producer applies this order so repeated
// validations of identical input always agree.
func SortViolations(violations []Violation) {
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

// NewValidationError normalizes raw violations into the stable neutral shape:
// duplicates removed and deterministic order applied by path, keyword, then
// message. It performs no response-size capping; bounding public error
// payloads is consuming-application policy, not producer semantics.
func NewValidationError(ref domain.ResourceTypeRef, violations []Violation) *ValidationError {
	unique := make([]Violation, 0, len(violations))
	seen := make(map[Violation]struct{}, len(violations))
	for _, violation := range violations {
		if _, exists := seen[violation]; exists {
			continue
		}
		seen[violation] = struct{}{}
		unique = append(unique, violation)
	}
	SortViolations(unique)
	return &ValidationError{TypeRef: ref, Violations: unique}
}
