// SPDX-License-Identifier: Apache-2.0

// Package kube contains the private Kubernetes REST boundary used by the
// Crossplane provisioner adapter. Nothing in this package escapes the
// adapter subtree: public Liftr contracts never see Kubernetes objects,
// group/kind/version tuples, namespaces, UIDs, or resource versions.
package kube

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// GVR identifies one REST resource family. Group may be empty for core
// resources; Crossplane XRs always carry a group.
type GVR struct {
	Group    string
	Version  string
	Resource string
}

// Path renders the namespaced resource collection path for this GVR.
func (g GVR) Path(namespace string) string {
	prefix := "/apis/" + g.Group + "/" + g.Version
	if g.Group == "" {
		prefix = "/api/" + g.Version
	}
	if namespace == "" {
		return prefix + "/" + g.Resource
	}
	return prefix + "/namespaces/" + namespace + "/" + g.Resource
}

// Object is a minimal unstructured Kubernetes object. It is deliberately
// opaque: callers use the typed accessors below and never ship Raw across
// the adapter boundary.
type Object struct {
	raw map[string]any
}

// NewObject wraps a decoded object document.
func NewObject(raw map[string]any) *Object {
	if raw == nil {
		raw = map[string]any{}
	}
	return &Object{raw: raw}
}

// Raw exposes the underlying document for marshalling.
func (o *Object) Raw() map[string]any { return o.raw }

// Clone returns a deep copy so callers can mutate safely.
func (o *Object) Clone() *Object { return NewObject(deepCopyMap(o.raw)) }

func (o *Object) metadata() map[string]any { return childMap(o.raw, "metadata") }

// Metadata returns the live metadata subdocument.
func (o *Object) Metadata() map[string]any { return o.metadata() }

// Name returns metadata.name.
func (o *Object) Name() string { value, _ := o.metadata()["name"].(string); return value }

// Namespace returns metadata.namespace.
func (o *Object) Namespace() string { value, _ := o.metadata()["namespace"].(string); return value }

// UID returns metadata.uid.
func (o *Object) UID() string { value, _ := o.metadata()["uid"].(string); return value }

// ResourceVersion returns metadata.resourceVersion.
func (o *Object) ResourceVersion() string {
	value, _ := o.metadata()["resourceVersion"].(string)
	return value
}

// Generation returns metadata.generation, or zero when unset/non-numeric.
func (o *Object) Generation() uint64 {
	return uint64Value(o.metadata()["generation"])
}

// Terminating reports whether metadata.deletionTimestamp is set.
func (o *Object) Terminating() bool {
	_, ok := o.metadata()["deletionTimestamp"]
	return ok
}

// Labels returns metadata.labels.
func (o *Object) Labels() map[string]any { return childMap(o.metadata(), "labels") }

// Annotations returns metadata.annotations.
func (o *Object) Annotations() map[string]any { return childMap(o.metadata(), "annotations") }

// LabelString reads one metadata label.
func (o *Object) LabelString(key string) (string, bool) {
	value, ok := o.Labels()[key].(string)
	return value, ok
}

// AnnotationString reads one metadata annotation.
func (o *Object) AnnotationString(key string) (string, bool) {
	value, ok := o.Annotations()[key].(string)
	return value, ok
}

// Status returns the status subdocument.
func (o *Object) Status() map[string]any { return childMap(o.raw, "status") }

// Spec returns the spec subdocument.
func (o *Object) Spec() map[string]any { return childMap(o.raw, "spec") }

// Conditions returns status.conditions entries that are objects.
func (o *Object) Conditions() []map[string]any {
	entries, _ := o.Status()["conditions"].([]any)
	result := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if typed, ok := entry.(map[string]any); ok {
			result = append(result, typed)
		}
	}
	return result
}

// ValueAtPath walks a dotted-free path of map keys and returns the value.
func (o *Object) ValueAtPath(path []string) (any, bool) {
	current := any(o.raw)
	for _, segment := range path {
		typed, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = typed[segment]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func childMap(parent map[string]any, key string) map[string]any {
	if parent == nil {
		return map[string]any{}
	}
	typed, _ := parent[key].(map[string]any)
	if typed == nil {
		return map[string]any{}
	}
	return typed
}

func uint64Value(value any) uint64 {
	switch typed := value.(type) {
	case float64:
		if typed < 0 {
			return 0
		}
		return uint64(typed)
	case int64:
		if typed < 0 {
			return 0
		}
		return uint64(typed)
	case int:
		if typed < 0 {
			return 0
		}
		return uint64(typed)
	case uint64:
		return typed
	default:
		return 0
	}
}

func deepCopyMap(source map[string]any) map[string]any {
	target := make(map[string]any, len(source))
	for key, value := range source {
		target[key] = deepCopyValue(value)
	}
	return target
}

func deepCopyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return deepCopyMap(typed)
	case []any:
		copied := make([]any, len(typed))
		for index, entry := range typed {
			copied[index] = deepCopyValue(entry)
		}
		return copied
	default:
		return typed
	}
}

// Client is the narrow REST surface the adapter needs. Implementations must
// be safe for concurrent use.
type Client interface {
	Get(ctx context.Context, gvr GVR, namespace, name string) (*Object, error)
	Create(ctx context.Context, gvr GVR, namespace string, object *Object) (*Object, error)
	// Update performs an atomic full-object update. When
	// requiredResourceVersion is non-empty, the API server must reject the
	// write with a conflict unless the live object still carries exactly
	// that resource version. This converts every mutation into an optimistic
	// compare-and-swap against the object version Liftr just verified, so a
	// replacement object under the same name can never be overwritten.
	Update(ctx context.Context, gvr GVR, namespace, name string, object *Object, requiredResourceVersion string) (*Object, error)
	Delete(ctx context.Context, gvr GVR, namespace, name string, uidPrecondition string) error
	// ServedResource reports, through structured API discovery only, whether
	// the group/version/resource is currently served by the API server.
	// Definitive answers are ServedConfirmed or ServedRefuted; every failure
	// mode — transport loss, throttling, authorization, malformed payloads —
	// collapses to ServedUnknown so callers can fail closed instead of
	// mistaking an unserved kind for a missing object.
	ServedResource(ctx context.Context, gvr GVR) ServedVerdict
}

// ServedVerdict classifies one discovery answer.
type ServedVerdict int

const (
	// ServedConfirmed means the API resource list definitively includes the
	// target resource.
	ServedConfirmed ServedVerdict = iota
	// ServedRefuted means the API server definitively does not serve the
	// target resource: either the group/version endpoint is absent or the
	// structured resource list omits it.
	ServedRefuted
	// ServedUnknown means discovery could not produce a definitive answer.
	// Callers must treat managed-object absence as unproven.
	ServedUnknown
)

// APIError is a structured Kubernetes API failure. Message carries the raw
// server message for adapter-side classification only; it never reaches
// Liftr contracts or persistence.
type APIError struct {
	Code    int
	Reason  string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("kubernetes api error %d %s: %s", e.Code, e.Reason, e.Message)
}

// IsAPIError reports whether err carries structured API error data.
func IsAPIError(err error) bool {
	var api *APIError
	return errors.As(err, &api)
}

func apiErrorOf(err error) *APIError {
	var api *APIError
	if errors.As(err, &api) {
		return api
	}
	return nil
}

// IsNotFound reports a definitive 404 from the API server.
func IsNotFound(err error) bool {
	api := apiErrorOf(err)
	return api != nil && (api.Code == http.StatusNotFound || strings.EqualFold(api.Reason, "NotFound"))
}

// IsAlreadyExists reports a create collision.
func IsAlreadyExists(err error) bool {
	api := apiErrorOf(err)
	return api != nil && (strings.EqualFold(api.Reason, "AlreadyExists"))
}

// IsConflict reports precondition or optimistic-concurrency failures.
func IsConflict(err error) bool {
	api := apiErrorOf(err)
	return api != nil && (api.Code == http.StatusConflict || strings.EqualFold(api.Reason, "Conflict"))
}

// IsForbidden reports authorization failures.
func IsForbidden(err error) bool {
	api := apiErrorOf(err)
	return api != nil && (api.Code == http.StatusForbidden || strings.EqualFold(api.Reason, "Forbidden"))
}

// IsInvalid reports schema/admission rejections.
func IsInvalid(err error) bool {
	api := apiErrorOf(err)
	return api != nil && (api.Code == http.StatusUnprocessableEntity || strings.EqualFold(api.Reason, "Invalid"))
}

// IsUnavailable reports transient control-plane failures: transport loss,
// timeouts, server faults, and throttling. These are never conclusive.
func IsUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if api := apiErrorOf(err); api != nil {
		return api.Code == http.StatusTooManyRequests || api.Code >= http.StatusInternalServerError ||
			strings.EqualFold(api.Reason, "Timeout") || strings.EqualFold(api.Reason, "ServerTimeout")
	}
	return true
}
