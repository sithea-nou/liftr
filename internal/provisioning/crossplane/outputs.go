// SPDX-License-Identifier: Apache-2.0

package crossplane

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube"
)

// The private output envelope is the only channel through which realized
// values cross from an XR status document into Liftr. It mirrors the Pulumi
// selected-export discipline: exactly one registered status path per mapping,
// identity fields must match the executing operation, and values are flat
// non-secret scalars only. Arbitrary XR status never crosses the boundary.
const (
	outputEnvelopeVersion = 1
	maxEnvelopeDepth      = 4
	maxEnvelopeKeys       = 64
	maxEnvelopeStringLen  = 4096
)

var (
	errOutputEnvelopeInvalid = errors.New("declared outputs violated the private output contract")
	errOutputUnavailable     = errors.New("declared outputs could not be extracted")
)

// extractOutputEvidence reads the registered status path of one observation
// and decodes the envelope. The registered path is relative to the XR status
// document. A missing path is transient unavailability (the composition has
// not patched the status yet); every structural violation of the envelope is
// permanent and deterministic.
func extractOutputEvidence(object *kube.Object, mapping OutputMapping, envelopeRef string, resourceID domain.ResourceID, targetGeneration uint64) *provisioningOutputEvidence {
	status := object.Status()
	var value any = status
	found := true
	for _, segment := range mapping.StatusPath {
		typed, ok := value.(map[string]any)
		if !ok {
			found = false
			break
		}
		value, found = typed[segment]
		if !found {
			break
		}
	}
	if !found {
		return unavailableEvidence(mapping.Ref)
	}
	envelope, ok := value.(map[string]any)
	if !ok {
		return invalidEvidence(mapping.Ref, "output status path does not hold an envelope object")
	}
	values, err := decodeStatusEnvelope(envelope, envelopeRef, resourceID, targetGeneration)
	if err != nil {
		if errors.Is(err, errOutputUnavailable) {
			return unavailableEvidence(mapping.Ref)
		}
		return invalidEvidence(mapping.Ref, "output envelope violated the private output contract")
	}
	return &provisioningOutputEvidence{mappingRef: mapping.Ref, state: evidenceAvailable, values: values}
}

func unavailableEvidence(mappingRef string) *provisioningOutputEvidence {
	return &provisioningOutputEvidence{mappingRef: mappingRef, state: evidenceUnavailable}
}

func invalidEvidence(mappingRef, _ string) *provisioningOutputEvidence {
	// The offending detail stays here; only the curated state and mapping
	// identity cross to the provisioning contract.
	return &provisioningOutputEvidence{mappingRef: mappingRef, state: evidenceInvalid}
}

type evidenceState int

const (
	evidenceUnavailable evidenceState = iota
	evidenceAvailable
	evidenceInvalid
)

type provisioningOutputEvidence struct {
	mappingRef string
	state      evidenceState
	values     map[string]any
}

func decodeStatusEnvelope(envelope map[string]any, mappingRef string, resourceID domain.ResourceID, targetGeneration uint64) (map[string]any, error) {
	if len(envelope) == 0 {
		return nil, fmt.Errorf("%w: envelope is empty", errOutputEnvelopeInvalid)
	}
	if len(envelope) > maxEnvelopeKeys {
		return nil, fmt.Errorf("%w: envelope exceeds the key bound", errOutputEnvelopeInvalid)
	}
	allowed := map[string]struct{}{
		"version": {}, "mapping": {}, "resourceId": {}, "targetGeneration": {}, "values": {},
	}
	for field := range envelope {
		if _, known := allowed[field]; !known {
			return nil, fmt.Errorf("%w: unknown private-envelope field", errOutputEnvelopeInvalid)
		}
	}
	version, ok, err := envelopeInteger(envelope, "version")
	if err != nil || !ok || version != outputEnvelopeVersion {
		return nil, fmt.Errorf("%w: unsupported output envelope version", errOutputEnvelopeInvalid)
	}
	mapping, ok := envelope["mapping"].(string)
	if !ok || mapping != mappingRef {
		return nil, fmt.Errorf("%w: output mapping identity does not match the persisted execution mapping", errOutputEnvelopeInvalid)
	}
	envelopeResourceID, ok := envelope["resourceId"].(string)
	if !ok || envelopeResourceID != string(resourceID) {
		return nil, fmt.Errorf("%w: output envelope does not belong to this resource", errOutputEnvelopeInvalid)
	}
	envelopeGeneration, ok, err := envelopeInteger(envelope, "targetGeneration")
	if err != nil || !ok || envelopeGeneration != targetGeneration {
		return nil, fmt.Errorf("%w: output envelope does not carry this execution's target generation", errOutputEnvelopeInvalid)
	}
	rawValues, ok := envelope["values"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: envelope values are missing", errOutputEnvelopeInvalid)
	}
	if len(rawValues) > maxEnvelopeKeys {
		return nil, fmt.Errorf("%w: envelope values exceed the key bound", errOutputEnvelopeInvalid)
	}
	values := make(map[string]any, len(rawValues))
	for name, raw := range rawValues {
		scalar, ok := flatScalar(raw)
		if !ok {
			return nil, fmt.Errorf("%w: output %q is not a flat scalar", errOutputEnvelopeInvalid, name)
		}
		if text, isText := scalar.(string); isText && len(text) > maxEnvelopeStringLen {
			return nil, fmt.Errorf("%w: output %q exceeds the length bound", errOutputEnvelopeInvalid, name)
		}
		values[name] = scalar
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("%w: envelope carries no values", errOutputEnvelopeInvalid)
	}
	return values, nil
}

func envelopeInteger(envelope map[string]any, field string) (uint64, bool, error) {
	raw, exists := envelope[field]
	if !exists {
		return 0, false, nil
	}
	switch typed := raw.(type) {
	case float64:
		if typed < 0 || typed != float64(uint64(typed)) {
			return 0, false, fmt.Errorf("%w: field %q is not an integral number", errOutputEnvelopeInvalid, field)
		}
		return uint64(typed), true, nil
	case int64:
		return uint64(typed), true, nil
	case int:
		return uint64(typed), true, nil
	default:
		return 0, false, fmt.Errorf("%w: field %q is not numeric", errOutputEnvelopeInvalid, field)
	}
}

func flatScalar(value any) (any, bool) {
	switch typed := value.(type) {
	case string:
		return typed, true
	case bool:
		return typed, true
	case float64:
		if typed != typed || typed > 1.7976931348623157e308 || typed < -1.7976931348623157e308 {
			return nil, false
		}
		return typed, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		float, err := typed.Float64()
		if err != nil {
			return nil, false
		}
		return float, true
	default:
		return nil, false
	}
}
