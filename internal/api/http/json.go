// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// maxBodyBytes bounds every request body. v1 payloads are small JSON
// documents; oversized bodies are rejected before parsing.
const maxBodyBytes = 1 << 20

// requestError carries an already-classified client error through decoding.
type requestError struct {
	code   string
	detail string
}

func (e *requestError) Error() string { return e.detail }

func badRequest(detail string) *requestError {
	return &requestError{code: CodeInvalidArgument, detail: detail}
}

// decodeEnvelope strictly parses one JSON object into env. Unknown fields are
// rejected and trailing data is refused. Numbers stay json.Number inside
// json.RawMessage fields; callers refine those with decodeSpec.
func decodeEnvelope(r *http.Request, env any) *requestError {
	r.Body = http.MaxBytesReader(nil, r.Body, maxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(env); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return badRequest("request body exceeds the size limit")
		}
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			return badRequest("request body is not valid JSON")
		}
		var unmarshal *json.UnmarshalTypeError
		if errors.As(err, &unmarshal) {
			return badRequest(fmt.Sprintf("field %s has the wrong JSON type", jsonField(unmarshal.Field)))
		}
		return badRequest("request body could not be parsed")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return badRequest("request body must contain exactly one JSON document")
	}
	return nil
}

// rawSpec decodes the opaque ResourceSpec payload. Arbitrary keys are allowed
// at every depth because ResourceSpec is ResourceType-defined intent; only
// well-formedness applies. JSON numbers normalize to int64 for integer
// literals and float64 for decimals and exponents, keeping 1 distinct from
// 1.0. Overflowing integers are rejected instead of silently widening.
func rawSpec(raw json.RawMessage) (map[string]any, *requestError) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, badRequest("spec is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return nil, badRequest("spec is not valid JSON")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, badRequest("spec must contain exactly one JSON document")
	}
	normalized, err := normalizeValue(root)
	if err != nil {
		return nil, badRequest(err.Error())
	}
	object, ok := normalized.(map[string]any)
	if !ok {
		return nil, badRequest("spec must be a JSON object")
	}
	return object, nil
}

func normalizeValue(value any) (any, error) {
	switch value := value.(type) {
	case json.Number:
		return normalizeNumber(value)
	case map[string]any:
		normalized := make(map[string]any, len(value))
		for key, item := range value {
			child, err := normalizeValue(item)
			if err != nil {
				return nil, fmt.Errorf("property %q: %w", key, err)
			}
			normalized[key] = child
		}
		return normalized, nil
	case []any:
		normalized := make([]any, len(value))
		for i, item := range value {
			child, err := normalizeValue(item)
			if err != nil {
				return nil, fmt.Errorf("item %d: %w", i, err)
			}
			normalized[i] = child
		}
		return normalized, nil
	default:
		return value, nil
	}
}

// normalizeNumber applies the approved rule: an integer literal becomes int64,
// a decimal or exponent literal becomes float64, and an integer that does not
// fit in int64 is rejected rather than coerced.
func normalizeNumber(number json.Number) (any, error) {
	text := number.String()
	if !strings.ContainsAny(text, ".eE") {
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, errors.New("integer value " + text + " does not fit in 64 bits")
		}
		return value, nil
	}
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil, errors.New("numeric value " + text + " is out of range")
	}
	return value, nil
}

func jsonField(path string) string {
	if path == "" {
		return "body"
	}
	return strings.TrimPrefix(path, ".")
}
