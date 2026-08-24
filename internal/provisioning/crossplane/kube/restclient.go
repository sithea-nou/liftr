// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func decodeBase64Field(value string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

type restClient struct {
	base   *url.URL
	client *http.Client
}

// NewClient builds a Client from a resolved RestConfig. timeout bounds every
// request; the client never retries automatically because submission
// idempotency belongs to Liftr's provisioning semantics, not to transport.
func NewClient(config *RestConfig, timeout time.Duration) (Client, error) {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	httpClient, err := config.resolve()
	if err != nil {
		return nil, err
	}
	httpClient.Timeout = timeout
	base, err := url.Parse(strings.TrimSuffix(config.Host, "/"))
	if err != nil {
		return nil, fmt.Errorf("kubernetes api server host is not a valid URL: %w", err)
	}
	return &restClient{base: base, client: httpClient}, nil
}

func (c *restClient) Get(ctx context.Context, gvr GVR, namespace, name string) (*Object, error) {
	endpoint := c.base.String() + gvr.Path(namespace) + "/" + url.PathEscape(name)
	object, _, err := c.do(ctx, http.MethodGet, endpoint, "", nil)
	return object, err
}

func (c *restClient) Create(ctx context.Context, gvr GVR, namespace string, object *Object) (*Object, error) {
	body, err := json.Marshal(object.Raw())
	if err != nil {
		return nil, fmt.Errorf("encode object for create: %w", err)
	}
	endpoint := c.base.String() + gvr.Path(namespace)
	created, _, err := c.do(ctx, http.MethodPost, endpoint, "application/json", body)
	return created, err
}

// Update performs the conditional full-object update. The document must
// carry metadata.resourceVersion equal to requiredResourceVersion, which the
// API server enforces as an optimistic-concurrency precondition: any
// concurrent change — including a wholesale replacement of the object under
// this name — rejects the write with 409 Conflict instead of landing.
func (c *restClient) Update(ctx context.Context, gvr GVR, namespace, name string, object *Object, requiredResourceVersion string) (*Object, error) {
	body := deepCopyForUpdate(object.Raw(), requiredResourceVersion)
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode object for update: %w", err)
	}
	endpoint := c.base.String() + gvr.Path(namespace) + "/" + url.PathEscape(name)
	updated, _, err := c.do(ctx, http.MethodPut, endpoint, "application/json", encoded)
	return updated, err
}

// deepCopyForUpdate clones the desired document and stamps the precondition.
// UID is deliberately omitted: identity is enforced by the resourceVersion
// precondition and re-verified on conflict, never asserted by write payload.
func deepCopyForUpdate(raw map[string]any, requiredResourceVersion string) map[string]any {
	copied := deepCopyMap(raw)
	metadata := childMapOf(copied, "metadata")
	freshMetadata := make(map[string]any, len(metadata)+1)
	for key, value := range metadata {
		freshMetadata[key] = value
	}
	delete(freshMetadata, "uid")
	if requiredResourceVersion != "" {
		freshMetadata["resourceVersion"] = requiredResourceVersion
	} else {
		delete(freshMetadata, "resourceVersion")
	}
	copied["metadata"] = freshMetadata
	return copied
}

func childMapOf(parent map[string]any, key string) map[string]any {
	if parent == nil {
		return map[string]any{}
	}
	typed, _ := parent[key].(map[string]any)
	if typed == nil {
		return map[string]any{}
	}
	return typed
}

func (c *restClient) Delete(ctx context.Context, gvr GVR, namespace, name string, uidPrecondition string) error {
	body := map[string]any{"apiVersion": "v1", "kind": "DeleteOptions"}
	if uidPrecondition != "" {
		body["preconditions"] = map[string]any{"uid": uidPrecondition}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("encode delete options: %w", err)
	}
	endpoint := c.base.String() + gvr.Path(namespace) + "/" + url.PathEscape(name)
	_, status, err := c.do(ctx, http.MethodDelete, endpoint, "application/json", encoded)
	if err != nil {
		return err
	}
	if status == http.StatusNotFound {
		return &APIError{Code: http.StatusNotFound, Reason: "NotFound", Message: "the server reported the target as absent"}
	}
	return nil
}

// ServedResource resolves the served-ness of one API resource through the
// group-version discovery document. The answer is derived exclusively from
// structured payloads and status codes: a 200 whose resource list names the
// resource confirms it; a 404 on the group/version endpoint or an omitted
// entry refutes it; every transport failure, server fault, throttling
// response, authorization denial, and malformed payload is Unknown.
func (c *restClient) ServedResource(ctx context.Context, gvr GVR) ServedVerdict {
	var endpoint string
	if gvr.Group == "" {
		endpoint = c.base.String() + "/api/" + gvr.Version
	} else {
		endpoint = fmt.Sprintf("%s/apis/%s/%s", c.base.String(), url.PathEscape(gvr.Group), url.PathEscape(gvr.Version))
	}
	object, _, err := c.do(ctx, http.MethodGet, endpoint, "", nil)
	if err != nil {
		if IsAPIError(err) && IsNotFound(err) {
			return ServedRefuted
		}
		return ServedUnknown
	}
	if object == nil {
		return ServedUnknown
	}
	resources, ok := object.Raw()["resources"].([]any)
	if !ok {
		return ServedUnknown
	}
	for _, entry := range resources {
		typed, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if name, _ := typed["name"].(string); name == gvr.Resource {
			return ServedConfirmed
		}
	}
	return ServedRefuted
}

func (c *restClient) do(ctx context.Context, method, endpoint, contentType string, body []byte) (*Object, int, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("build kubernetes request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if contentType != "" && len(body) > 0 {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := c.client.Do(request)
	if err != nil {
		// Transport-level failures are returned unwrapped so IsUnavailable
		// classifies them as transient uncertainty.
		return nil, 0, fmt.Errorf("kubernetes request failed: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return nil, response.StatusCode, fmt.Errorf("read kubernetes response: %w", err)
	}
	if len(payload) > maxResponseBytes {
		return nil, response.StatusCode, &APIError{Code: response.StatusCode, Reason: "ResponseTooLarge", Message: "response exceeded the adapter size bound"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		apiError := decodeStatusFailure(response.StatusCode, payload)
		return nil, response.StatusCode, apiError
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		return nil, response.StatusCode, nil
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, response.StatusCode, fmt.Errorf("decode kubernetes response: %w", err)
	}
	return NewObject(document), response.StatusCode, nil
}

const maxResponseBytes = 4 << 20

func decodeStatusFailure(code int, payload []byte) *APIError {
	var status struct {
		Message string `json:"message"`
		Reason  string `json:"reason"`
		Code    int    `json:"code"`
	}
	if len(payload) > 0 && json.Unmarshal(payload, &status) == nil && status.Reason != "" {
		resolved := code
		if status.Code != 0 {
			resolved = status.Code
		}
		return &APIError{Code: resolved, Reason: status.Reason, Message: status.Message}
	}
	return &APIError{Code: code, Reason: http.StatusText(code), Message: strings.TrimSpace(string(payload))}
}
