// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
)

// ListResourceOperations reads one server-defined page of retained lifecycle
// Operations for a Resource.
func (c *Client) ListResourceOperations(ctx context.Context, resourceID string, limit int, cursor string) (*OperationList, error) {
	query := url.Values{}
	if limit != 0 {
		query.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	path := "/v1/resources/" + url.PathEscape(resourceID) + "/operations"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	rsp, err := c.do(ctx, http.MethodGet, path, nil, nil, false)
	if err != nil {
		return nil, err
	}
	if apiErr := toError(rsp); apiErr != nil {
		return nil, apiErr
	}

	var envelope struct {
		Items      []json.RawMessage `json:"items"`
		NextCursor string            `json:"nextCursor,omitempty"`
	}
	if err := decodeInto(rsp.raw, &envelope); err != nil {
		return nil, err
	}
	list := &OperationList{
		Raw:        rsp.raw,
		Items:      make([]Operation, 0, len(envelope.Items)),
		NextCursor: envelope.NextCursor,
	}
	for _, raw := range envelope.Items {
		operation := Operation{}
		if err := decodeInto(raw, &operation); err != nil {
			return nil, err
		}
		operation.Raw = raw
		list.Items = append(list.Items, operation)
	}
	return list, nil
}

// RetryOperation admits a new lifecycle Operation that retries the identified
// source Operation under a concrete Resource generation precondition.
func (c *Client) RetryOperation(ctx context.Context, operationID, idempotencyKey string, generation uint64) (*MutationResult, error) {
	headers := map[string]string{
		"Idempotency-Key":     idempotencyKey,
		"If-Liftr-Generation": strconv.FormatUint(generation, 10),
	}
	rsp, err := c.do(ctx, http.MethodPost, "/v1/operations/"+url.PathEscape(operationID)+"/retry", nil, headers, true)
	if err != nil {
		return nil, err
	}
	if apiErr := toError(rsp); apiErr != nil {
		return nil, apiErr
	}
	operation, err := c.decodeOperation(rsp)
	if err != nil {
		return nil, err
	}
	monitorRef, hasMonitorEntry := parseMonitorLink(rsp.header.Get("Link"))
	return &MutationResult{
		Operation:       operation,
		Status:          rsp.status,
		Replay:          rsp.header.Get("Idempotency-Replayed") == "true",
		MonitorRef:      monitorRef,
		HasMonitorEntry: hasMonitorEntry,
		LocationRef:     rsp.header.Get("Location"),
	}, nil
}
