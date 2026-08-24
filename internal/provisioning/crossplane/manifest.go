// SPDX-License-Identifier: Apache-2.0

package crossplane

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube"
)

// The encoded XR spec is the only developer-intent payload crossing into
// Kubernetes. It is strictly bounded and must not carry Kubernetes envelope
// concepts; the adapter owns apiVersion, kind, metadata, and all correlation
// metadata.
const (
	maxSpecBytes = 64 << 10
)

var forbiddenSpecKeys = map[string]struct{}{
	"apiVersion": {},
	"kind":       {},
	"metadata":   {},
	"status":     {},
}

func decodeEncodedSpec(encoded []byte) (map[string]any, error) {
	if len(encoded) == 0 {
		return nil, fmt.Errorf("encoded spec is empty")
	}
	if len(encoded) > maxSpecBytes {
		return nil, fmt.Errorf("encoded spec exceeds the size bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("encoded spec is not valid JSON")
	}
	if decoder.More() {
		return nil, fmt.Errorf("encoded spec carries trailing content")
	}
	object, ok := document.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("encoded spec must be a JSON object")
	}
	for key := range forbiddenSpecKeys {
		if _, exists := object[key]; exists {
			return nil, fmt.Errorf("encoded spec must not declare reserved field %q", key)
		}
	}
	return normalizeJSON(object).(map[string]any), nil
}

// normalizeJSON converts json.Number values to float64 so documents round-trip
// through encoding/json consistently.
func normalizeJSON(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, entry := range typed {
			typed[key] = normalizeJSON(entry)
		}
		return typed
	case []any:
		for index, entry := range typed {
			typed[index] = normalizeJSON(entry)
		}
		return typed
	case json.Number:
		float, err := typed.Float64()
		if err != nil {
			return typed.String()
		}
		return float
	default:
		return value
	}
}

// buildManifest assembles the full desired XR document for one execution.
// The returned object is a fresh document safe to hand to the client.
func buildManifest(binding *resolvedBinding, identity identityMetadata, name string, input Input) (*kube.Object, error) {
	encoded, err := binding.binding.EncodeInput(input)
	if err != nil {
		return nil, fmt.Errorf("encode XR spec: %w", err)
	}
	spec, err := decodeEncodedSpec(encoded)
	if err != nil {
		return nil, err
	}
	apiVersion := binding.gvr.Version
	if binding.gvr.Group != "" {
		apiVersion = binding.gvr.Group + "/" + binding.gvr.Version
	}
	document := map[string]any{
		"apiVersion": apiVersion,
		"kind":       binding.kind,
		"metadata": map[string]any{
			"name":      name,
			"namespace": binding.namespace,
		},
		"spec": spec,
	}
	identity.stamp(document)
	stampOperationCorrelation(document, input.OperationID, input.TargetGeneration)
	return kube.NewObject(document), nil
}

// conditionalUpdate turns a manifest into the full desired object for one
// atomic conditional update. The resourceVersion precondition makes the PUT
// a compare-and-swap against exactly the object version Liftr verified; UID
// is never asserted through the write payload, so replacement objects can
// only ever surface as precondition conflicts that the caller re-evaluates.
func conditionalUpdate(manifest *kube.Object, resourceVersion string) *kube.Object {
	cleaned := make(map[string]any, len(manifest.Raw()))
	for key, value := range manifest.Raw() {
		cleaned[key] = value
	}
	metadata := make(map[string]any, len(manifest.Metadata())+1)
	for key, value := range manifest.Metadata() {
		metadata[key] = value
	}
	delete(metadata, "uid")
	if resourceVersion != "" {
		metadata["resourceVersion"] = resourceVersion
	}
	cleaned["metadata"] = metadata
	return kube.NewObject(cleaned)
}
