// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Problem codes are the stable, approved v1 error identifiers.
const (
	CodeInvalidArgument         = "INVALID_ARGUMENT"
	CodeUnsupportedResourceType = "UNSUPPORTED_RESOURCE_TYPE"
	CodeResourceNotFound        = "RESOURCE_NOT_FOUND"
	CodeOperationNotFound       = "OPERATION_NOT_FOUND"
	CodeResourceAlreadyExists   = "RESOURCE_ALREADY_EXISTS"
	CodeIdempotencyConflict     = "IDEMPOTENCY_CONFLICT"
	CodeGenerationConflict      = "GENERATION_CONFLICT"
	CodeOperationActive         = "OPERATION_ACTIVE"
	CodeResourceStateConflict   = "RESOURCE_STATE_CONFLICT"
	CodeUnsupportedCapability   = "UNSUPPORTED_CAPABILITY"
	CodePreconditionRequired    = "PRECONDITION_REQUIRED"
	CodeProvisionerUnavailable  = "PROVISIONER_UNAVAILABLE"
	CodePersistenceUnavailable  = "PERSISTENCE_UNAVAILABLE"
	CodeInternal                = "INTERNAL"
)

// problemTypeBase is the namespace for problem type URIs. Each code maps to
// one stable slug so clients can branch on machine-readable types without
// parsing details.
const problemTypeBase = "https://liftr.dev/problems/"

var problemTitles = map[string]string{
	CodeInvalidArgument:         "Invalid request",
	CodeUnsupportedResourceType: "Unsupported resource type",
	CodeResourceNotFound:        "Resource not found",
	CodeOperationNotFound:       "Operation not found",
	CodeResourceAlreadyExists:   "Resource already exists",
	CodeIdempotencyConflict:     "Idempotency key conflict",
	CodeGenerationConflict:      "Generation conflict",
	CodeOperationActive:         "Operation already active",
	CodeResourceStateConflict:   "Resource state conflict",
	CodeUnsupportedCapability:   "Unsupported capability",
	CodePreconditionRequired:    "Precondition required",
	CodeProvisionerUnavailable:  "Provisioner unavailable",
	CodePersistenceUnavailable:  "Persistence unavailable",
	CodeInternal:                "Internal error",
}

// problem is the RFC 9457 representation with Liftr extensions. It never
// embeds raw provider, Go, or persistence errors; detail is a curated,
// client-safe sentence.
type problem struct {
	Type              string  `json:"type"`
	Title             string  `json:"title"`
	Status            int     `json:"status"`
	Detail            string  `json:"detail,omitempty"`
	Instance          string  `json:"instance,omitempty"`
	Code              string  `json:"code"`
	RequestID         string  `json:"requestId"`
	CurrentGeneration *uint64 `json:"currentGeneration,omitempty"`
}

func problemStatus(code string) int {
	switch code {
	case CodeInvalidArgument:
		return http.StatusBadRequest
	case CodeUnsupportedResourceType:
		return http.StatusUnprocessableEntity
	case CodeResourceNotFound, CodeOperationNotFound:
		return http.StatusNotFound
	case CodeResourceAlreadyExists, CodeIdempotencyConflict, CodeGenerationConflict,
		CodeOperationActive, CodeResourceStateConflict, CodeUnsupportedCapability:
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

// writeProblem renders one Problem Details response. requestId comes from the
// authoritative server-generated X-Request-ID.
func writeProblem(w http.ResponseWriter, r *http.Request, code, detail string, currentGeneration *uint64) {
	status := problemStatus(code)
	body := problem{
		Type:              problemTypeBase + problemSlug(code),
		Title:             problemTitles[code],
		Status:            status,
		Detail:            detail,
		Instance:          r.URL.Path,
		Code:              code,
		RequestID:         RequestIDFromContext(r.Context()),
		CurrentGeneration: currentGeneration,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
