// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"encoding/json"
	"net/http"
	"strings"
)

const (
	codeUnauthenticated             = "UNAUTHENTICATED"
	codeOperatorForbidden           = "OPERATOR_FORBIDDEN"
	codeInvalidArgument             = "INVALID_ARGUMENT"
	codePreconditionRequired        = "PRECONDITION_REQUIRED"
	codeDiagnosticStale             = "DIAGNOSTIC_STALE"
	codeActionNotApplicable         = "ACTION_NOT_APPLICABLE"
	codeRecoveryAlreadyActive       = "RECOVERY_ALREADY_ACTIVE"
	codeRecoveryUnsafe              = "RECOVERY_UNSAFE"
	codeOperatorIdempotencyConflict = "OPERATOR_IDEMPOTENCY_CONFLICT"
	codeResourceNotFound            = "RESOURCE_NOT_FOUND"
	codeOperationNotFound           = "OPERATION_NOT_FOUND"
	codeWorkNotFound                = "WORK_NOT_FOUND"
	codePersistenceUnavailable      = "PERSISTENCE_UNAVAILABLE"
	codeAdminDraining               = "ADMIN_DRAINING"
	codeInternal                    = "INTERNAL"
)

var titles = map[string]string{
	codeUnauthenticated: "Unauthenticated", codeOperatorForbidden: "Operator forbidden",
	codeInvalidArgument: "Invalid request", codePreconditionRequired: "Precondition required",
	codeDiagnosticStale: "Diagnostic stale", codeActionNotApplicable: "Action not applicable",
	codeRecoveryAlreadyActive: "Recovery already active", codeRecoveryUnsafe: "Recovery unsafe",
	codeOperatorIdempotencyConflict: "Operator idempotency conflict",
	codeResourceNotFound:            "Resource not found", codeOperationNotFound: "Operation not found",
	codeWorkNotFound: "Work not found", codePersistenceUnavailable: "Persistence unavailable",
	codeAdminDraining: "Admin plane draining", codeInternal: "Internal error",
}

type problem struct {
	Type           string `json:"type"`
	Title          string `json:"title"`
	Status         int    `json:"status"`
	Detail         string `json:"detail,omitempty"`
	Instance       string `json:"instance,omitempty"`
	Code           string `json:"code"`
	RequestID      string `json:"requestId"`
	ExistingWorkID string `json:"existingWorkId,omitempty"`
}

func statusFor(code string) int {
	switch code {
	case codeUnauthenticated:
		return http.StatusUnauthorized
	case codeOperatorForbidden:
		return http.StatusForbidden
	case codeInvalidArgument:
		return http.StatusBadRequest
	case codePreconditionRequired:
		return http.StatusPreconditionRequired
	case codeDiagnosticStale:
		return http.StatusPreconditionFailed
	case codeActionNotApplicable, codeRecoveryAlreadyActive, codeRecoveryUnsafe, codeOperatorIdempotencyConflict:
		return http.StatusConflict
	case codeResourceNotFound, codeOperationNotFound, codeWorkNotFound:
		return http.StatusNotFound
	case codePersistenceUnavailable, codeAdminDraining:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

func writeProblem(w http.ResponseWriter, r *http.Request, code, detail string, extensions *problem) {
	body := problem{
		Type:  "https://liftr.dev/problems/" + strings.ToLower(strings.ReplaceAll(code, "_", "-")),
		Title: titles[code], Status: statusFor(code), Detail: detail,
		Instance: r.URL.Path, Code: code, RequestID: requestID(r.Context()),
	}
	if extensions != nil {
		body.ExistingWorkID = extensions.ExistingWorkID
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(body.Status)
	_ = json.NewEncoder(w).Encode(body)
}
