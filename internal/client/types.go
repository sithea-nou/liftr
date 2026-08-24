// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// The representations below mirror the public v1 contract in
// docs/openapi/v1/openapi.yaml. They deliberately duplicate the server's
// transport types instead of importing them: the client depends on the
// published document, not on Liftr internals. Dynamic documents (spec,
// specSchema) stay raw so numeric literals such as 20 and 20.0 are never
// normalized. A contract-drift test pins these shapes against the OpenAPI
// document.

type ResourceTypeRef struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type OwnerRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type Condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason"`
	Message            string    `json:"message,omitempty"`
	ObservedGeneration uint64    `json:"observedGeneration"`
	LastTransitionAt   time.Time `json:"lastTransitionAt"`
}

type ResourceStatus struct {
	State              string      `json:"state"`
	ObservedGeneration uint64      `json:"observedGeneration"`
	Conditions         []Condition `json:"conditions,omitempty"`
	UpdatedAt          time.Time   `json:"updatedAt"`
}

type LatestOperationRef struct {
	ID               string `json:"id"`
	Capability       string `json:"capability"`
	State            string `json:"state"`
	TargetGeneration uint64 `json:"targetGeneration"`
	Href             string `json:"href"`
}

// ResourceOutputs carries the declared non-secret realized values. Values
// decode with json.Number semantics when typed rendering needs them; Raw
// keeps the exact server bytes for JSON output.
type ResourceOutputs struct {
	ObservedGeneration uint64          `json:"observedGeneration"`
	Values             map[string]any  `json:"values"`
	Raw                json.RawMessage `json:"-"`
}

type Resource struct {
	Raw             json.RawMessage     `json:"-"`
	ID              string              `json:"id"`
	Type            ResourceTypeRef     `json:"type"`
	Owner           OwnerRef            `json:"owner"`
	Generation      uint64              `json:"generation"`
	Spec            json.RawMessage     `json:"spec"`
	Status          ResourceStatus      `json:"status"`
	LatestOperation *LatestOperationRef `json:"latestOperation,omitempty"`
	Outputs         *ResourceOutputs    `json:"outputs,omitempty"`
	CreatedAt       time.Time           `json:"createdAt"`
	UpdatedAt       time.Time           `json:"updatedAt"`
}

type OperationFailure struct {
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

type Operation struct {
	Raw              json.RawMessage   `json:"-"`
	ID               string            `json:"id"`
	ResourceID       string            `json:"resourceId"`
	RetryOf          string            `json:"retryOf,omitempty"`
	Capability       string            `json:"capability"`
	State            string            `json:"state"`
	TargetGeneration uint64            `json:"targetGeneration"`
	RequestedAt      time.Time         `json:"requestedAt"`
	StartedAt        *time.Time        `json:"startedAt,omitempty"`
	CompletedAt      *time.Time        `json:"completedAt,omitempty"`
	Failure          *OperationFailure `json:"failure,omitempty"`
}

type OperationList struct {
	Raw        json.RawMessage `json:"-"`
	Items      []Operation     `json:"items"`
	NextCursor string          `json:"nextCursor,omitempty"`
}

// ResourceSummaryStatus is the inventory observation of a Resource: state
// and freshness only. Conditions belong to Resource.
type ResourceSummaryStatus struct {
	State              string    `json:"state"`
	ObservedGeneration uint64    `json:"observedGeneration"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

// ResourceSummary is one inventory entry. It deliberately carries no spec,
// conditions, outputs, or execution metadata; read those from Resource via
// GetResource.
type ResourceSummary struct {
	Raw             json.RawMessage       `json:"-"`
	ID              string                `json:"id"`
	Type            ResourceTypeRef       `json:"type"`
	Owner           OwnerRef              `json:"owner"`
	Generation      uint64                `json:"generation"`
	Status          ResourceSummaryStatus `json:"status"`
	LatestOperation *LatestOperationRef   `json:"latestOperation,omitempty"`
	CreatedAt       time.Time             `json:"createdAt"`
	UpdatedAt       time.Time             `json:"updatedAt"`
}

// ResourceList is one ownership-scoped inventory page. Raw keeps the exact
// server bytes for verbatim JSON output; NextCursor is empty on the final page.
type ResourceList struct {
	Raw        json.RawMessage   `json:"-"`
	Items      []ResourceSummary `json:"items"`
	NextCursor string            `json:"nextCursor,omitempty"`
}

const (
	StatePending   = "Pending"
	StateRunning   = "Running"
	StateSucceeded = "Succeeded"
	StateFailed    = "Failed"
	StateCanceled  = "Canceled"
)

type ResourceTypeSummary struct {
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	DisplayName  string   `json:"displayName"`
	Description  string   `json:"description"`
	Capabilities []string `json:"capabilities"`
	Href         string   `json:"href"`
}

type ResourceTypeDetail struct {
	Raw            json.RawMessage `json:"-"`
	Name           string          `json:"name"`
	Version        string          `json:"version"`
	DisplayName    string          `json:"displayName"`
	Description    string          `json:"description"`
	Capabilities   []string        `json:"capabilities"`
	Href           string          `json:"href"`
	SpecSchema     json.RawMessage `json:"specSchema"`
	OutputContract json.RawMessage `json:"outputContract,omitempty"`
}

type ResourceTypeList struct {
	Raw   json.RawMessage       `json:"-"`
	Items []ResourceTypeSummary `json:"items"`
}

// MutationResult is an admitted mutation: its Resource or Operation response
// plus the untrusted server-supplied monitor references, which are resolved
// and origin-checked only by MonitorOperationID.
type MutationResult struct {
	Resource        *Resource
	Operation       *Operation
	Status          int
	Replay          bool
	MonitorRef      string
	HasMonitorEntry bool
	LocationRef     string
}

// decodeInto decodes exactly one JSON document into out using json.Number
// semantics for dynamic values, so no number is ever coerced through
// float64.
func decodeInto(raw []byte, out any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return errors.New("response body is empty")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("response is not a valid API representation: %w", err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("response must contain exactly one JSON document")
	}
	return nil
}

func (c *Client) decodeResource(rsp *response) (*Resource, error) {
	resource := &Resource{}
	if err := decodeInto(rsp.raw, resource); err != nil {
		return nil, err
	}
	resource.Raw = rsp.raw
	return resource, nil
}

func (c *Client) decodeOperation(rsp *response) (*Operation, error) {
	operation := &Operation{}
	if err := decodeInto(rsp.raw, operation); err != nil {
		return nil, err
	}
	operation.Raw = rsp.raw
	return operation, nil
}
