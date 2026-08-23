// SPDX-License-Identifier: Apache-2.0

package client

import (
	"fmt"
	"net/http"
)

// Approved v1 problem codes the CLI branches on. Every other code is
// rendered generically; the CLI never invents per-Problem semantics.
const (
	CodeUnauthenticated    = "UNAUTHENTICATED"
	CodeOperationNotFound  = "OPERATION_NOT_FOUND"
	CodeGenerationConflict = "GENERATION_CONFLICT"
)

type SpecViolation struct {
	Path    string `json:"path"`
	Keyword string `json:"keyword"`
	Message string `json:"message"`
}

// Problem is RFC 9457 Problem Details with the Liftr extensions published in
// the OpenAPI contract.
type Problem struct {
	Type              string          `json:"type"`
	Title             string          `json:"title"`
	Status            int             `json:"status"`
	Detail            string          `json:"detail,omitempty"`
	Instance          string          `json:"instance,omitempty"`
	Code              string          `json:"code"`
	RequestID         string          `json:"requestId"`
	CurrentGeneration *uint64         `json:"currentGeneration,omitempty"`
	Violations        []SpecViolation `json:"violations,omitempty"`
	Truncated         bool            `json:"truncated,omitempty"`
}

// APIError reports a non-2xx API response. It carries the decoded Problem
// when the server answered with application/problem+json and otherwise an
// opaque fallback carrying only the status and the authoritative
// X-Request-ID header. It never exposes headers or credentials.
type APIError struct {
	Status    int
	Problem   Problem
	RequestID string
	parsed    bool
}

func (e *APIError) Error() string {
	title := e.Problem.Title
	if title == "" {
		title = http.StatusText(e.Status)
	}
	if e.Problem.Code != "" {
		return fmt.Sprintf("API error %d: %s (%s)", e.Status, title, e.Problem.Code)
	}
	return fmt.Sprintf("API error %d: %s", e.Status, title)
}

// IsAuthentication reports a credential failure (401 UNAUTHENTICATED).
func (e *APIError) IsAuthentication() bool {
	return e.Status == http.StatusUnauthorized || e.Problem.Code == CodeUnauthenticated
}

// HasCode reports whether the server answered with the given approved code.
func (e *APIError) HasCode(code string) bool {
	return e.Problem.Code == code
}

func decodeProblem(raw []byte) (Problem, error) {
	problem := Problem{}
	if err := decodeInto(raw, &problem); err != nil {
		return Problem{}, err
	}
	return problem, nil
}

// toError converts one client response into either nil (2xx) or an *APIError,
// decoding application/problem+json bodies through media-type parsing rather
// than brittle exact-string comparison.
func toError(rsp *response) error {
	if rsp.status >= 200 && rsp.status < 300 {
		if len(rsp.raw) == 0 {
			return fmt.Errorf("unexpected empty response body with status %d", rsp.status)
		}
		contentType, ok := mediaType(rsp.header.Get("Content-Type"))
		if !ok || contentType != "application/json" {
			return fmt.Errorf("unexpected response content type %q with status %d", contentType, rsp.status)
		}
		return nil
	}

	apiErr := &APIError{
		Status:    rsp.status,
		RequestID: rsp.header.Get("X-Request-ID"),
	}
	contentType, ok := mediaType(rsp.header.Get("Content-Type"))
	if ok && contentType == "application/problem+json" {
		if problem, err := decodeProblem(rsp.raw); err == nil {
			apiErr.Problem = problem
			apiErr.parsed = true
			if apiErr.Problem.Status == 0 {
				apiErr.Problem.Status = rsp.status
			}
			if apiErr.Problem.RequestID == "" {
				apiErr.Problem.RequestID = apiErr.RequestID
			}
			return apiErr
		}
	}
	apiErr.Problem = Problem{
		Title:  "request failed",
		Status: rsp.status,
	}
	return apiErr
}
