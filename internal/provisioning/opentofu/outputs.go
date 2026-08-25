// SPDX-License-Identifier: Apache-2.0

package opentofu

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/sithea-nou/liftr/internal/domain"
)

const (
	maxRootOutputsBytes    = 4 << 20
	maxOutputEnvelopeBytes = 64 << 10
)

func decodeOutputs(raw []byte, mapping OutputMapping, resourceID domain.ResourceID, generation uint64) (map[string]any, error) {
	if len(raw) == 0 || len(raw) > maxRootOutputsBytes {
		return nil, fmt.Errorf("declared output envelope is invalid")
	}
	if rejectDuplicateJSONKeys(raw) != nil {
		return nil, fmt.Errorf("declared output envelope is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	var all map[string]struct {
		Sensitive *bool           `json:"sensitive"`
		Type      json.RawMessage `json:"type"`
		Value     json.RawMessage `json:"value"`
	}
	if decoder.Decode(&all) != nil || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) {
		return nil, fmt.Errorf("declared output envelope is invalid")
	}
	selected, ok := all[mapping.EnvelopeName]
	all = nil
	if !ok || selected.Sensitive == nil || *selected.Sensitive || len(selected.Type) == 0 || len(selected.Type) > 4096 || len(selected.Value) == 0 || len(selected.Value) > maxOutputEnvelopeBytes {
		return nil, fmt.Errorf("declared output envelope is invalid")
	}
	valueDecoder := json.NewDecoder(bytes.NewReader(selected.Value))
	valueDecoder.UseNumber()
	valueDecoder.DisallowUnknownFields()
	var envelope struct {
		Version          json.Number    `json:"version"`
		Mapping          string         `json:"mapping"`
		ResourceID       string         `json:"resourceId"`
		TargetGeneration json.Number    `json:"targetGeneration"`
		Values           map[string]any `json:"values"`
	}
	if valueDecoder.Decode(&envelope) != nil || !errors.Is(valueDecoder.Decode(&struct{}{}), io.EOF) || envelope.Version.String() != "1" || envelope.Mapping != mapping.Ref || envelope.ResourceID != string(resourceID) || envelope.TargetGeneration.String() != strconv.FormatUint(generation, 10) {
		return nil, fmt.Errorf("declared output envelope is invalid")
	}
	if len(envelope.Values) != len(mapping.Fields) {
		return nil, fmt.Errorf("declared output envelope is invalid")
	}
	result := make(map[string]any, len(mapping.Fields))
	for target, source := range mapping.Fields {
		value, ok := envelope.Values[source]
		if !ok {
			return nil, fmt.Errorf("declared output envelope is invalid")
		}
		scalar, ok := outputScalar(value)
		if !ok {
			return nil, fmt.Errorf("declared output envelope is invalid")
		}
		result[target] = scalar
	}
	return result, nil
}

func outputScalar(value any) (any, bool) {
	switch value := value.(type) {
	case string:
		if len(value) > 4096 || strings.Contains(value, "[secret]") {
			return nil, false
		}
		return value, true
	case bool:
		return value, true
	case json.Number:
		if !strings.ContainsAny(value.String(), ".eE") {
			integer, err := value.Int64()
			if err != nil || value.String() != strconv.FormatInt(integer, 10) {
				return nil, false
			}
			return integer, true
		}
		floating, err := value.Float64()
		return floating, err == nil
	default:
		return nil, false
	}
}

func rejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("JSON document has trailing content")
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 128 {
		return fmt.Errorf("JSON document is too deeply nested")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok || seen[key] {
				return fmt.Errorf("JSON object key is invalid or duplicated")
			}
			seen[key] = true
			if err := consumeJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	expected := json.Delim('}')
	if delimiter == '[' {
		expected = ']'
	}
	if closing != expected {
		return fmt.Errorf("mismatched JSON delimiter")
	}
	return nil
}
