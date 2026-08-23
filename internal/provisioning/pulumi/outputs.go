// SPDX-License-Identifier: Apache-2.0

package pulumi

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

// The private output envelope is the only channel through which realized
// values cross from a Pulumi stack into Liftr. It is deliberately narrow:
//
//   - exactly one allowlisted export name is read, selected retrieval only;
//   - the command never passes --show-secrets, so Pulumi-marked secrets are
//     returned in redacted form and rejected;
//   - the envelope is strictly bounded and parsed without duplicate-key or
//     unknown-field tolerance;
//   - identity fields (mapping reference, resource ID, target generation)
//     must match the executing operation exactly;
//   - values are flat non-secret scalars only.
//
// Redacted-secret marker detection is defense-in-depth below these primary
// boundaries; the spelling of the marker is never a security invariant.
const (
	outputEnvelopeVersion = 1
	maxOutputBytes        = 64 << 10
	maxOutputDepth        = 4
	maxOutputKeys         = 64
	maxOutputStringLength = 4096

	// secretRedactionMarker is Pulumi's textual redaction of secret outputs when
	// plaintext retrieval is not requested. Detection is defense-in-depth only;
	// the primary boundaries are selected retrieval and the strict envelope.
	secretRedactionMarker = "[secret]"
)

var (
	errOutputEnvelopeInvalid = errors.New("declared outputs violated the private output contract")
	errOutputUnavailable     = errors.New("declared outputs could not be extracted")
)

// decodeSelectedOutputEnvelope strictly parses one selected stack export into
// candidate output values. Every violation is deterministic and therefore
// permanent: callers classify them as invalid output evidence, never as
// transient extraction failures.
func decodeSelectedOutputEnvelope(raw []byte, mappingRef string, resourceID domain.ResourceID, targetGeneration uint64) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: selected output is empty", errOutputEnvelopeInvalid)
	}
	if len(raw) > maxOutputBytes {
		return nil, fmt.Errorf("%w: selected output exceeds the size bound", errOutputEnvelopeInvalid)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	root, err := strictDecodeValue(decoder, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errOutputEnvelopeInvalid, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing content after the output document", errOutputEnvelopeInvalid)
	}
	object, ok := root.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: output document is not an object", errOutputEnvelopeInvalid)
	}
	allowedFields := map[string]struct{}{
		"version": {}, "mapping": {}, "resourceId": {}, "targetGeneration": {}, "values": {},
	}
	for field := range object {
		if _, known := allowedFields[field]; !known {
			return nil, fmt.Errorf("%w: unknown private-envelope field %q", errOutputEnvelopeInvalid, field)
		}
	}
	version, ok, err := envelopeInteger(object, "version")
	if err != nil {
		return nil, err
	}
	if !ok || version != outputEnvelopeVersion {
		return nil, fmt.Errorf("%w: unsupported output envelope version", errOutputEnvelopeInvalid)
	}
	mapping, ok := object["mapping"].(string)
	if !ok || mapping != mappingRef {
		return nil, fmt.Errorf("%w: output mapping identity does not match the persisted execution mapping", errOutputEnvelopeInvalid)
	}
	envelopeResourceID, ok := object["resourceId"].(string)
	if !ok || envelopeResourceID != string(resourceID) {
		return nil, fmt.Errorf("%w: output envelope does not belong to this resource", errOutputEnvelopeInvalid)
	}
	envelopeGeneration, ok, err := envelopeInteger(object, "targetGeneration")
	if err != nil {
		return nil, err
	}
	if !ok || envelopeGeneration != targetGeneration {
		return nil, fmt.Errorf("%w: output envelope does not belong to this target generation", errOutputEnvelopeInvalid)
	}
	rawValues, ok := object["values"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: output values are not an object", errOutputEnvelopeInvalid)
	}
	values := make(map[string]any, len(rawValues))
	for key, value := range rawValues {
		scalar, ok := strictScalar(value)
		if !ok {
			return nil, fmt.Errorf("%w: output field %q is not a flat scalar", errOutputEnvelopeInvalid, key)
		}
		values[key] = scalar
	}
	return values, nil
}

func envelopeInteger(object map[string]any, field string) (uint64, bool, error) {
	number, ok := object[field].(json.Number)
	if !ok {
		return 0, false, nil
	}
	parsed, err := number.Int64()
	if err != nil || parsed < 0 {
		return 0, false, fmt.Errorf("%w: output field %q is not an unsigned integer", errOutputEnvelopeInvalid, field)
	}
	return uint64(parsed), true, nil
}

// strictScalar converts a decoded JSON leaf into Liftr's canonical scalar
// representations and rejects anything nested, textual redaction included.
func strictScalar(value any) (any, bool) {
	switch typed := value.(type) {
	case string:
		if len(typed) > maxOutputStringLength || strings.Contains(typed, secretRedactionMarker) {
			return nil, false
		}
		return typed, true
	case bool:
		return typed, true
	case json.Number:
		if integral, err := typed.Int64(); err == nil && typed.String() == strconv.FormatInt(integral, 10) {
			return integral, true
		}
		asFloat, err := typed.Float64()
		if err != nil {
			return nil, false
		}
		return asFloat, true
	default:
		return nil, false
	}
}

// strictDecodeValue parses JSON while rejecting duplicate object keys,
// enforcing depth bounds, and carrying every string through redaction checks.
func strictDecodeValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > maxOutputDepth {
		return nil, fmt.Errorf("document nesting exceeds the depth bound")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	return strictDecodeToken(token, decoder, depth)
}

// strictDecodeToken interprets one already-consumed token. Composite openers
// continue parsing from the decoder; leaves are validated directly.
func strictDecodeToken(token json.Token, decoder *json.Decoder, depth int) (any, error) {
	if depth > maxOutputDepth {
		return nil, fmt.Errorf("document nesting exceeds the depth bound")
	}
	switch boundary := token.(type) {
	case json.Delim:
		switch boundary {
		case '{':
			return strictDecodeObject(decoder, depth)
		case '[':
			return strictDecodeArray(decoder, depth)
		default:
			return nil, fmt.Errorf("unexpected delimiter %q", boundary)
		}
	default:
		return strictDecodeLeaf(token)
	}
}

func strictDecodeLeaf(token json.Token) (any, error) {
	switch typed := token.(type) {
	case string:
		if len(typed) > maxOutputStringLength {
			return nil, fmt.Errorf("string exceeds the length bound")
		}
		if strings.Contains(typed, secretRedactionMarker) {
			return nil, fmt.Errorf("redacted secret material cannot appear in declared outputs")
		}
		return typed, nil
	case json.Number:
		return typed, nil
	case bool:
		return typed, nil
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported JSON token")
	}
}

func strictDecodeObject(decoder *json.Decoder, depth int) (any, error) {
	object := make(map[string]any)
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if boundary, ok := token.(json.Delim); ok && boundary == '}' {
			return object, nil
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("object key is not a string")
		}
		if _, duplicate := object[key]; duplicate {
			return nil, fmt.Errorf("duplicate object key")
		}
		value, err := strictDecodeValue(decoder, depth+1)
		if err != nil {
			return nil, err
		}
		object[key] = value
		if len(object) > maxOutputKeys {
			return nil, fmt.Errorf("object key count exceeds the bound")
		}
	}
}

func strictDecodeArray(decoder *json.Decoder, depth int) (any, error) {
	var items []any
	for {
		// Peek without consuming: More reports whether another value follows.
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if boundary, ok := token.(json.Delim); ok && boundary == ']' {
			return items, nil
		}
		item, err := strictDecodeToken(token, decoder, depth+1)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		if len(items) > maxOutputKeys {
			return nil, fmt.Errorf("array length exceeds the bound")
		}
	}
}
