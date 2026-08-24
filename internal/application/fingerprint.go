// SPDX-License-Identifier: Apache-2.0

package application

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	"github.com/sithea-nou/liftr/internal/domain"
)

// fingerprintNode is a canonical, type-distinguishing, order-independent
// encoding of a resource spec. It mirrors the persisted spec codec so a
// fingerprint is stable regardless of map ordering or the numeric Go type the
// caller used to express a value.
type fingerprintNode struct {
	Kind   string                     `json:"kind"`
	Scalar *string                    `json:"scalar,omitempty"`
	Object map[string]fingerprintNode `json:"object,omitempty"`
	List   []fingerprintNode          `json:"list,omitempty"`
}

// canonicalSpec renders a resource spec into a deterministic byte string. The
// JSON encoding of fingerprintNode sorts object keys, and encodeFingerprintValue
// keeps numeric width and floating-point bits distinct.
func canonicalSpec(spec domain.ResourceSpec) (string, error) {
	node, err := encodeFingerprintValue(spec.Values())
	if err != nil {
		return "", fmt.Errorf("fingerprint resource spec: %w", err)
	}
	encoded, err := json.Marshal(node)
	if err != nil {
		return "", fmt.Errorf("fingerprint resource spec: %w", err)
	}
	return string(encoded), nil
}

func createCommandFingerprint(cmd CreateResourceCommand) (string, error) {
	spec, err := canonicalSpec(cmd.Spec)
	if err != nil {
		return "", err
	}
	return fingerprintHash("create", string(cmd.ID), cmd.Type.Name, cmd.Type.Version, cmd.Owner.Kind, cmd.Owner.ID, spec), nil
}

func updateCommandFingerprint(cmd UpdateResourceCommand) (string, error) {
	spec, err := canonicalSpec(cmd.Spec)
	if err != nil {
		return "", err
	}
	return fingerprintHash("update", string(cmd.ID), strconv.FormatUint(cmd.ExpectedGeneration, 10), spec), nil
}

func deleteCommandFingerprint(cmd DeleteResourceCommand) string {
	return fingerprintHash("delete", string(cmd.ID), strconv.FormatUint(cmd.ExpectedGeneration, 10))
}

// retryCommandFingerprint is versioned independently from the lifecycle shape.
// Only submitted command identity participates in the v1 retry fingerprint.
func retryCommandFingerprint(cmd RetryOperationCommand) string {
	return fingerprintHash("retry-operation-v1", string(cmd.OperationID), strconv.FormatUint(cmd.ExpectedGeneration, 10))
}

// fingerprintHash digests request identity parts with fixed-width hex length
// prefixes so the encoding is injective: no delimiter can collide with data,
// and parts that contain NUL bytes or other parts as substrings stay distinct.
func fingerprintHash(parts ...string) string {
	hasher := sha256.New()
	for _, part := range parts {
		hasher.Write([]byte(fmt.Sprintf("%08x", len(part))))
		hasher.Write([]byte(part))
	}
	digest := hasher.Sum(nil)
	return hex.EncodeToString(digest)
}

func encodeFingerprintValue(value any) (fingerprintNode, error) {
	scalar := func(kind, value string) fingerprintNode { return fingerprintNode{Kind: kind, Scalar: &value} }
	switch value := value.(type) {
	case nil:
		return fingerprintNode{Kind: "null"}, nil
	case bool:
		return scalar("bool", strconv.FormatBool(value)), nil
	case string:
		return scalar("string", value), nil
	case int:
		return scalar("int", strconv.FormatInt(int64(value), 10)), nil
	case int8:
		return scalar("int8", strconv.FormatInt(int64(value), 10)), nil
	case int16:
		return scalar("int16", strconv.FormatInt(int64(value), 10)), nil
	case int32:
		return scalar("int32", strconv.FormatInt(int64(value), 10)), nil
	case int64:
		return scalar("int64", strconv.FormatInt(value, 10)), nil
	case uint:
		return scalar("uint", strconv.FormatUint(uint64(value), 10)), nil
	case uint8:
		return scalar("uint8", strconv.FormatUint(uint64(value), 10)), nil
	case uint16:
		return scalar("uint16", strconv.FormatUint(uint64(value), 10)), nil
	case uint32:
		return scalar("uint32", strconv.FormatUint(uint64(value), 10)), nil
	case uint64:
		return scalar("uint64", strconv.FormatUint(value, 10)), nil
	case float32:
		bits := make([]byte, 4)
		putUint32(bits, math.Float32bits(value))
		return scalar("float32", hex.EncodeToString(bits)), nil
	case float64:
		bits := make([]byte, 8)
		putUint64(bits, math.Float64bits(value))
		return scalar("float64", hex.EncodeToString(bits)), nil
	case map[string]any:
		object := make(map[string]fingerprintNode, len(value))
		for key, item := range value {
			encoded, err := encodeFingerprintValue(item)
			if err != nil {
				return fingerprintNode{}, fmt.Errorf("encode property %q: %w", key, err)
			}
			object[key] = encoded
		}
		return fingerprintNode{Kind: "object", Object: object}, nil
	case []any:
		list := make([]fingerprintNode, len(value))
		for i, item := range value {
			encoded, err := encodeFingerprintValue(item)
			if err != nil {
				return fingerprintNode{}, fmt.Errorf("encode list item %d: %w", i, err)
			}
			list[i] = encoded
		}
		return fingerprintNode{Kind: "list", List: list}, nil
	default:
		return fingerprintNode{}, fmt.Errorf("unsupported resource spec value %T", value)
	}
}

func putUint32(target []byte, value uint32) {
	for i := len(target) - 1; i >= 0; i-- {
		target[i] = byte(value)
		value >>= 8
	}
}

func putUint64(target []byte, value uint64) {
	for i := len(target) - 1; i >= 0; i-- {
		target[i] = byte(value)
		value >>= 8
	}
}
