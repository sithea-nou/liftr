// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
	"github.com/sithea-nou/liftr/internal/lifecycle"
)

const (
	defaultOperationPageSize = 20
	operationCursorPrefix    = "c1_"
	operationCursorKind      = byte(1)
	operationCursorBytes     = 1 + sha256.Size + 8
	maxOperationCursorLength = 64
)

// getOperation returns the public v1 representation of one lifecycle
// Operation to a principal authorized on the owning Resource. Operations have
// no independent ACL: authorization flows through the Resource's stored owner
// (ADR-0012), and denial renders the identical OPERATION_NOT_FOUND problem as
// absence so operation activity is never disclosed. OperationPhase is
// deliberately not part of v1, and no Liftr-Generation header is emitted
// because this response does not represent a Resource.
func (h *handler) getOperation(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w, r) {
		return
	}
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	record, err := h.service.GetOperation(r.Context(), principal, domain.OperationID(r.PathValue("id")))
	if err != nil {
		if errors.Is(err, application.ErrNotAuthorized) {
			hideOperation(w, r)
			return
		}
		h.mapReadError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(newOperationDTO(record.Operation))
}

// listResourceOperations authorizes the addressed Resource before interpreting
// pagination input, then delegates to an application use case that repeats the
// authorization in the page transaction. This ordering prevents malformed
// cursors from becoming a Resource-existence oracle.
func (h *handler) listResourceOperations(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w, r) {
		return
	}
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	resourceID := domain.ResourceID(r.PathValue("id"))
	if err := h.service.CheckResourceAccess(r.Context(), principal, resourceID, identity.ActionResourceRead); err != nil {
		if errors.Is(err, application.ErrResourceNotFound) || errors.Is(err, application.ErrNotAuthorized) {
			hideResource(w, r)
		} else {
			h.mapReadError(w, r, err)
		}
		return
	}
	limit, cursor, rerr := parseOperationPageQuery(r, resourceID)
	if rerr != nil {
		writeProblem(w, r, rerr.code, rerr.detail, nil)
		return
	}
	page, err := h.service.ListResourceOperations(r.Context(), principal, resourceID, cursor, limit)
	if err != nil {
		if errors.Is(err, application.ErrNotAuthorized) {
			hideResource(w, r)
		} else {
			h.mapReadError(w, r, err)
		}
		return
	}
	body := operationListDTO{Items: make([]operationDTO, 0, len(page.Records))}
	for _, record := range page.Records {
		body.Items = append(body.Items, newOperationDTO(record.Operation))
	}
	if page.NextSequence != 0 {
		body.NextCursor = encodeOperationCursor(resourceID, page.NextSequence)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}

func parseOperationPageQuery(r *http.Request, resourceID domain.ResourceID) (int, uint64, *requestError) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, 0, badRequest("query parameters are malformed")
	}
	for name, values := range query {
		if name != "limit" && name != "cursor" {
			return 0, 0, badRequest("only limit and cursor query parameters are supported")
		}
		if len(values) != 1 {
			return 0, 0, badRequest("query parameters may be supplied at most once")
		}
	}
	limit := defaultOperationPageSize
	if values, present := query["limit"]; present {
		value := values[0]
		if value == "" || strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return 0, 0, badRequest("limit must be an integer between 1 and 100")
		}
		parsed, err := strconv.ParseUint(value, 10, 8)
		if err != nil || parsed < 1 || parsed > application.MaxResourceOperationPageSize {
			return 0, 0, badRequest("limit must be an integer between 1 and 100")
		}
		limit = int(parsed)
	}
	var before uint64
	if values, present := query["cursor"]; present {
		before, err = decodeOperationCursor(resourceID, values[0])
		if err != nil {
			return 0, 0, badRequest("cursor is not a valid operation-history cursor for this Resource")
		}
	}
	return limit, before, nil
}

func encodeOperationCursor(resourceID domain.ResourceID, before uint64) string {
	payload := make([]byte, operationCursorBytes)
	payload[0] = operationCursorKind
	digest := sha256.Sum256([]byte(resourceID))
	copy(payload[1:], digest[:])
	binary.BigEndian.PutUint64(payload[1+sha256.Size:], before)
	return operationCursorPrefix + base64.RawURLEncoding.EncodeToString(payload)
}

func decodeOperationCursor(resourceID domain.ResourceID, cursor string) (uint64, error) {
	if len(cursor) > maxOperationCursorLength || !strings.HasPrefix(cursor, operationCursorPrefix) {
		return 0, errors.New("invalid cursor envelope")
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(cursor, operationCursorPrefix))
	if err != nil || len(payload) != operationCursorBytes || payload[0] != operationCursorKind {
		return 0, errors.New("invalid cursor payload")
	}
	digest := sha256.Sum256([]byte(resourceID))
	if subtle.ConstantTimeCompare(payload[1:1+sha256.Size], digest[:]) != 1 {
		return 0, errors.New("cursor belongs to another resource")
	}
	before := binary.BigEndian.Uint64(payload[1+sha256.Size:])
	if before == 0 {
		return 0, errors.New("cursor position must be greater than zero")
	}
	return before, nil
}

// retryOperation performs the existence-hiding retry authorization preflight
// before parsing headers or body. Admission repeats that authorization in the
// transaction before replay, generation, and lifecycle semantics.
func (h *handler) retryOperation(w http.ResponseWriter, r *http.Request) {
	if !h.requireService(w, r) {
		return
	}
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	operationID := domain.OperationID(r.PathValue("id"))
	if _, err := h.service.CheckRetryAccess(r.Context(), principal, operationID); err != nil {
		if errors.Is(err, application.ErrOperationNotFound) {
			hideOperation(w, r)
		} else {
			mapTransportFailure(w, r, err)
		}
		return
	}
	key, rerr := requireIdempotencyKey(r)
	if rerr != nil {
		writeProblem(w, r, rerr.code, rerr.detail, nil)
		return
	}
	generation, rerr := parseGenerationPrecondition(r)
	if rerr != nil {
		writeProblem(w, r, rerr.code, rerr.detail, nil)
		return
	}
	if rerr := requireWhitespaceBody(r); rerr != nil {
		writeProblem(w, r, rerr.code, rerr.detail, nil)
		return
	}
	newOperationID, eventID, minted := mintLifecycleIDs()
	if !minted {
		writeProblem(w, r, CodeInternal, "an unexpected internal error occurred", nil)
		return
	}
	result, err := h.service.AdmitRetryOperation(r.Context(), application.RetryOperationCommand{
		Actor:              principal,
		OperationID:        operationID,
		ExpectedGeneration: generation,
		NewOperationID:     newOperationID,
		EventID:            eventID,
		RequestedAt:        nowUTC(),
		IdempotencyKey:     key,
	})
	if err != nil {
		h.mapRetryError(w, r, principal, operationID, err)
		return
	}
	h.writeRetryResponse(w, result)
}

func requireWhitespaceBody(r *http.Request) *requestError {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return badRequest("request body exceeds the size limit")
		}
		return badRequest("request body could not be read")
	}
	if len(bytes.TrimSpace(body)) != 0 {
		return badRequest("retry requests take no body")
	}
	return nil
}

func (h *handler) mapRetryError(w http.ResponseWriter, r *http.Request, principal identity.Principal, operationID domain.OperationID, err error) {
	switch {
	case errors.Is(err, application.ErrNotAuthorized), errors.Is(err, application.ErrOperationNotFound), errors.Is(err, application.ErrResourceNotFound):
		hideOperation(w, r)
	case errors.Is(err, application.ErrIdempotencyConflict):
		writeProblem(w, r, CodeIdempotencyConflict, "this Idempotency-Key was already used with different request content", nil)
	case errors.Is(err, application.ErrConcurrencyConflict):
		resource, accessErr := h.service.CheckRetryAccess(r.Context(), principal, operationID)
		if errors.Is(accessErr, application.ErrOperationNotFound) {
			hideOperation(w, r)
			return
		}
		var current *uint64
		if accessErr == nil {
			generation := resource.Resource.Generation()
			current = &generation
		}
		writeProblem(w, r, CodeGenerationConflict, "the supplied If-Liftr-Generation does not match the current generation of the Resource", current)
	case errors.Is(err, lifecycle.ErrOperationActive):
		writeProblem(w, r, CodeOperationActive, "the Resource already has an active Operation; monitor it before retrying", nil)
	case errors.Is(err, application.ErrOperationNotRetryable), errors.Is(err, lifecycle.ErrInvalidTransition):
		writeProblem(w, r, CodeOperationNotRetryable, "the source Operation cannot be retried from the Resource's current state", nil)
	case errors.Is(err, application.ErrProvisionerNotFound), errors.Is(err, application.ErrResourceTypeNotFound):
		writeProblem(w, r, CodeProvisionerUnavailable, "no provisioner is available for this retry", nil)
	default:
		mapTransportFailure(w, r, err)
	}
}

func (h *handler) writeRetryResponse(w http.ResponseWriter, result application.Result) {
	operationID := string(result.Operation.ID())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", "/v1/operations/"+operationID)
	w.Header().Set("Link", `</v1/operations/`+operationID+`>; rel="monitor"`)
	if result.Replay {
		w.Header().Set("Idempotency-Replayed", "true")
	}
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(newOperationDTO(result.Operation))
}
