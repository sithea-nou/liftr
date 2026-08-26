// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sithea-nou/liftr/internal/application"
)

// Problem codes are the stable, approved v1 error identifiers.
const (
	CodeUnauthenticated         = "UNAUTHENTICATED"
	CodeForbidden               = "FORBIDDEN"
	CodeInvalidArgument         = "INVALID_ARGUMENT"
	CodeUnsupportedResourceType = "UNSUPPORTED_RESOURCE_TYPE"
	CodeResourceTypeNotFound    = "RESOURCE_TYPE_NOT_FOUND"
	CodeResourceSpecInvalid     = "RESOURCE_SPEC_INVALID"
	CodeResourceNotFound        = "RESOURCE_NOT_FOUND"
	CodeOperationNotFound       = "OPERATION_NOT_FOUND"
	CodeResourceAlreadyExists   = "RESOURCE_ALREADY_EXISTS"
	CodeIdempotencyConflict     = "IDEMPOTENCY_CONFLICT"
	CodeGenerationConflict      = "GENERATION_CONFLICT"
	CodeOperationActive         = "OPERATION_ACTIVE"
	CodeOperationNotRetryable   = "OPERATION_NOT_RETRYABLE"
	CodeResourceStateConflict   = "RESOURCE_STATE_CONFLICT"
	CodeUnsupportedCapability   = "UNSUPPORTED_CAPABILITY"
	CodePreconditionRequired    = "PRECONDITION_REQUIRED"
	CodeProvisionerUnavailable  = "PROVISIONER_UNAVAILABLE"
	CodePolicyDenied            = "POLICY_DENIED"
	CodeQuotaExceeded           = "QUOTA_EXCEEDED"
	CodeReferenceInvalid        = "REFERENCE_INVALID"
	CodeResourceInUse           = "RESOURCE_IN_USE"
	CodeDependencyCycle         = "DEPENDENCY_CYCLE"
	CodePersistenceUnavailable  = "PERSISTENCE_UNAVAILABLE"
	CodeInternal                = "INTERNAL"
)

// problemTypeBase is the namespace for problem type URIs. Each code maps to
// one stable slug so clients can branch on machine-readable types without
// parsing details.
const problemTypeBase = "https://liftr.dev/problems/"

var problemTitles = map[string]string{
	CodeUnauthenticated:         "Unauthenticated",
	CodeForbidden:               "Forbidden",
	CodeInvalidArgument:         "Invalid request",
	CodeUnsupportedResourceType: "Unsupported resource type",
	CodeResourceTypeNotFound:    "Resource type not found",
	CodeResourceSpecInvalid:     "Invalid resource spec",
	CodeResourceNotFound:        "Resource not found",
	CodeOperationNotFound:       "Operation not found",
	CodeResourceAlreadyExists:   "Resource already exists",
	CodeIdempotencyConflict:     "Idempotency key conflict",
	CodeGenerationConflict:      "Generation conflict",
	CodeOperationActive:         "Operation already active",
	CodeOperationNotRetryable:   "Operation not retryable",
	CodeResourceStateConflict:   "Resource state conflict",
	CodeUnsupportedCapability:   "Unsupported capability",
	CodePreconditionRequired:    "Precondition required",
	CodeProvisionerUnavailable:  "Provisioner unavailable",
	CodePolicyDenied:            "Policy denied",
	CodeQuotaExceeded:           "Quota exceeded",
	CodeReferenceInvalid:        "Invalid reference",
	CodeResourceInUse:           "Resource in use",
	CodeDependencyCycle:         "Dependency cycle",
	CodePersistenceUnavailable:  "Persistence unavailable",
	CodeInternal:                "Internal error",
}

// problem is the RFC 9457 representation with Liftr extensions. It never
// embeds raw provider, Go, or persistence errors; detail is a curated,
// client-safe sentence.
type problem struct {
	Type              string                      `json:"type"`
	Title             string                      `json:"title"`
	Status            int                         `json:"status"`
	Detail            string                      `json:"detail,omitempty"`
	Instance          string                      `json:"instance,omitempty"`
	Code              string                      `json:"code"`
	RequestID         string                      `json:"requestId"`
	CurrentGeneration *uint64                     `json:"currentGeneration,omitempty"`
	Violations        []application.SpecViolation `json:"violations,omitempty"`
	Truncated         bool                        `json:"truncated,omitempty"`
}

func problemStatus(code string) int {
	switch code {
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodeForbidden, CodePolicyDenied:
		return http.StatusForbidden
	case CodeInvalidArgument:
		return http.StatusBadRequest
	case CodeUnsupportedResourceType, CodeResourceSpecInvalid, CodeReferenceInvalid:
		return http.StatusUnprocessableEntity
	case CodeResourceNotFound, CodeOperationNotFound, CodeResourceTypeNotFound:
		return http.StatusNotFound
	case CodeResourceAlreadyExists, CodeIdempotencyConflict, CodeGenerationConflict,
		CodeOperationActive, CodeOperationNotRetryable, CodeResourceStateConflict, CodeUnsupportedCapability,
		CodeQuotaExceeded, CodeResourceInUse, CodeDependencyCycle:
		return http.StatusConflict
	case CodePreconditionRequired:
		return http.StatusPreconditionRequired
	case CodeProvisionerUnavailable, CodePersistenceUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func problemSlug(code string) string {
	slug := strings.ToLower(strings.ReplaceAll(code, "_", "-"))
	if _, ok := problemTitles[code]; !ok {
		slug = "internal"
	}
	return slug
}

// buildProblem assembles one Problem Details body from an approved code.
func buildProblem(r *http.Request, code, detail string, currentGeneration *uint64) problem {
	return problem{
		Type:              problemTypeBase + problemSlug(code),
		Title:             problemTitles[code],
		Status:            problemStatus(code),
		Detail:            detail,
		Instance:          r.URL.Path,
		Code:              code,
		RequestID:         RequestIDFromContext(r.Context()),
		CurrentGeneration: currentGeneration,
	}
}

// writeProblem renders one Problem Details response. requestId comes from the
// authoritative server-generated X-Request-ID.
func writeProblem(w http.ResponseWriter, r *http.Request, code, detail string, currentGeneration *uint64) {
	body := buildProblem(r, code, detail, currentGeneration)
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(problemStatus(code))
	_ = json.NewEncoder(w).Encode(body)
}

// writeReferencesProblem renders REFERENCE_INVALID with structured violations.
// Violations are application-sanitized: stable paths, curated messages,
// deterministic order, and the approved cap with a truncated indicator.
func writeReferencesProblem(w http.ResponseWriter, r *http.Request, detail string, invalid *application.InvalidReferenceError) {
	body := buildProblem(r, CodeReferenceInvalid, detail, nil)
	body.Violations = append([]application.SpecViolation(nil), invalid.Violations...)
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(body)
}

// writeSpecProblem renders RESOURCE_SPEC_INVALID with structured violations.
// Violations are sanitized by the application layer: stable JSON Pointer
// paths, curated messages, deterministic order, and the approved cap with a
// truncated indicator. Submitted spec values are never echoed.
func writeSpecProblem(w http.ResponseWriter, r *http.Request, detail string, invalid *application.InvalidSpecError) {
	body := buildProblem(r, CodeResourceSpecInvalid, detail, nil)
	body.Violations = append([]application.SpecViolation(nil), invalid.Violations...)
	body.Truncated = invalid.Truncated
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(body)
}
