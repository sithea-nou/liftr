// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Server-supplied references (Link, Location, anything inside a Problem or a
// body) are untrusted input. Monitor resolution parses them, resolves them
// against the configured Liftr origin, and refuses anything that would leave
// that origin — scheme, host, and effective port must all match — or that
// does not identify exactly one v1 Operation. There is no fallback to
// Resource.latestOperation: on an idempotency replay it may belong to a newer
// request, so a missing or malformed authoritative reference is a protocol
// failure.

var operationPathPattern = regexp.MustCompile(`^/v1/operations/([^/]+)$`)

var errNoMonitor = errors.New("the admission carries no Operation monitor reference")

func (c *Client) resolveMonitorRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", errors.New("monitor reference is empty")
	}
	parsed, err := url.Parse(ref)
	if err != nil {
		return "", fmt.Errorf("server-supplied monitor reference is malformed")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return "", errors.New("server-supplied monitor reference is malformed")
	}
	resolved := c.origin.ResolveReference(parsed)
	if !sameOrigin(c.origin, resolved) {
		return "", errors.New("server-supplied monitor reference leaves the configured Liftr origin; refusing to follow it")
	}
	match := operationPathPattern.FindStringSubmatch(resolved.EscapedPath())
	if match == nil {
		return "", errors.New("server-supplied monitor reference does not identify a v1 Operation")
	}
	operationID, err := url.PathUnescape(match[1])
	if err != nil {
		return "", errors.New("server-supplied monitor reference is malformed")
	}
	if operationID == "" {
		return "", errors.New("server-supplied monitor reference is malformed")
	}
	return operationID, nil
}

// MonitorOperationID returns the identifier of the Operation admitted by this
// exact mutation. Preference is the Link rel="monitor" entry; Location is
// consulted only when no such entry exists at all. When a monitor entry is
// present but invalid there is deliberately no fallback.
func (c *Client) MonitorOperationID(m *MutationResult) (string, error) {
	if m.HasMonitorEntry {
		return c.resolveMonitorRef(m.MonitorRef)
	}
	if strings.TrimSpace(m.LocationRef) != "" {
		operationID, err := c.resolveMonitorRef(m.LocationRef)
		if err != nil {
			return "", fmt.Errorf("admission carries no usable Operation monitor reference: %w", err)
		}
		return operationID, nil
	}
	return "", errNoMonitor
}

// parseMonitorLink extracts the raw reference of the first
// `Link: <...>; rel="monitor"` entry in the header value. It reports whether
// any rel="monitor" entry was found at all, so callers can distinguish "no
// authoritative link" from "authoritative link present but invalid".
func parseMonitorLink(header string) (string, bool) {
	for _, candidate := range strings.Split(header, ",") {
		segments := strings.Split(candidate, ";")
		target := strings.TrimSpace(segments[0])
		if len(target) < 2 || !strings.HasPrefix(target, "<") || !strings.HasSuffix(target, ">") {
			continue
		}
		rel := ""
		for _, parameter := range segments[1:] {
			key, value, ok := strings.Cut(parameter, "=")
			if ok && strings.EqualFold(strings.TrimSpace(key), "rel") {
				rel = strings.Trim(strings.TrimSpace(value), `"`)
			}
		}
		if strings.EqualFold(rel, "monitor") {
			return target[1 : len(target)-1], true
		}
	}
	return "", false
}

// ListResourceTypes reads the deterministic discovery summaries.
func (c *Client) ListResourceTypes(ctx context.Context) (*ResourceTypeList, error) {
	rsp, err := c.do(ctx, http.MethodGet, "/v1/resource-types", nil, nil, false)
	if err != nil {
		return nil, err
	}
	if apiErr := toError(rsp); apiErr != nil {
		return nil, apiErr
	}
	list := &ResourceTypeList{}
	if err := decodeInto(rsp.raw, list); err != nil {
		return nil, err
	}
	list.Raw = rsp.raw
	return list, nil
}

// GetResourceType reads one contract with its verbatim spec schema.
func (c *Client) GetResourceType(ctx context.Context, name, version string) (*ResourceTypeDetail, error) {
	rsp, err := c.do(ctx, http.MethodGet,
		"/v1/resource-types/"+url.PathEscape(name)+"/"+url.PathEscape(version), nil, nil, false)
	if err != nil {
		return nil, err
	}
	if apiErr := toError(rsp); apiErr != nil {
		return nil, apiErr
	}
	detail := &ResourceTypeDetail{}
	if err := decodeInto(rsp.raw, detail); err != nil {
		return nil, err
	}
	detail.Raw = rsp.raw
	return detail, nil
}

// GetResource reads one retained Resource.
func (c *Client) GetResource(ctx context.Context, id string) (*Resource, error) {
	rsp, err := c.do(ctx, http.MethodGet, "/v1/resources/"+url.PathEscape(id), nil, nil, false)
	if err != nil {
		return nil, err
	}
	if apiErr := toError(rsp); apiErr != nil {
		return nil, apiErr
	}
	return c.decodeResource(rsp)
}

// GetOperation reads one lifecycle Operation.
func (c *Client) GetOperation(ctx context.Context, id string) (*Operation, error) {
	rsp, err := c.do(ctx, http.MethodGet, "/v1/operations/"+url.PathEscape(id), nil, nil, false)
	if err != nil {
		return nil, err
	}
	if apiErr := toError(rsp); apiErr != nil {
		return nil, apiErr
	}
	return c.decodeOperation(rsp)
}

// CreateResource admits an asynchronous create. The envelope bytes are sent
// byte-identically on every attempt under the same idempotency key.
func (c *Client) CreateResource(ctx context.Context, body []byte, idempotencyKey string) (*MutationResult, error) {
	rsp, err := c.do(ctx, http.MethodPost, "/v1/resources", body,
		map[string]string{"Idempotency-Key": idempotencyKey}, true)
	if err != nil {
		return nil, err
	}
	return c.decodeMutation(rsp)
}

// UpdateResource admits a full spec replacement guarded by a concrete
// generation precondition.
func (c *Client) UpdateResource(ctx context.Context, id string, body []byte, idempotencyKey string, generation uint64) (*MutationResult, error) {
	headers := map[string]string{
		"Idempotency-Key":     idempotencyKey,
		"If-Liftr-Generation": strconv.FormatUint(generation, 10),
	}
	rsp, err := c.do(ctx, http.MethodPut, "/v1/resources/"+url.PathEscape(id), body, headers, true)
	if err != nil {
		return nil, err
	}
	return c.decodeMutation(rsp)
}

// DeleteResource admits an asynchronous delete guarded by a concrete
// generation precondition.
func (c *Client) DeleteResource(ctx context.Context, id string, idempotencyKey string, generation uint64) (*MutationResult, error) {
	headers := map[string]string{
		"Idempotency-Key":     idempotencyKey,
		"If-Liftr-Generation": strconv.FormatUint(generation, 10),
	}
	rsp, err := c.do(ctx, http.MethodDelete, "/v1/resources/"+url.PathEscape(id), nil, headers, true)
	if err != nil {
		return nil, err
	}
	return c.decodeMutation(rsp)
}

func (c *Client) decodeMutation(rsp *response) (*MutationResult, error) {
	if apiErr := toError(rsp); apiErr != nil {
		return nil, apiErr
	}
	resource, err := c.decodeResource(rsp)
	if err != nil {
		return nil, err
	}
	monitorRef, hasMonitorEntry := parseMonitorLink(rsp.header.Get("Link"))
	return &MutationResult{
		Resource:        resource,
		Status:          rsp.status,
		Replay:          rsp.header.Get("Idempotency-Replayed") == "true",
		MonitorRef:      monitorRef,
		HasMonitorEntry: hasMonitorEntry,
		LocationRef:     rsp.header.Get("Location"),
	}, nil
}
