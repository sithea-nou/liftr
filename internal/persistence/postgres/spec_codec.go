// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"

	"github.com/sithea-nou/liftr/internal/domain"
)

const resourceSpecCodecVersion = 1

type specNode struct {
	Kind   string              `json:"kind"`
	Scalar *string             `json:"scalar,omitempty"`
	Object map[string]specNode `json:"object"`
	List   []specNode          `json:"list"`
}

func encodeResourceSpec(spec domain.ResourceSpec) (int, []byte, error) {
	node, err := encodeSpecValue(spec.Values())
	if err != nil {
		return 0, nil, err
	}
	encoded, err := json.Marshal(node)
	if err != nil {
		return 0, nil, fmt.Errorf("encode resource spec: %w", err)
	}
	return resourceSpecCodecVersion, encoded, nil
}

func decodeResourceSpec(version int, encoded []byte) (domain.ResourceSpec, error) {
	if version != resourceSpecCodecVersion {
		return domain.ResourceSpec{}, fmt.Errorf("unsupported resource spec codec version %d", version)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var node specNode
	if err := decoder.Decode(&node); err != nil {
		return domain.ResourceSpec{}, fmt.Errorf("decode resource spec: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return domain.ResourceSpec{}, fmt.Errorf("decode resource spec: trailing data")
	}
	value, err := decodeSpecValue(node)
	if err != nil {
		return domain.ResourceSpec{}, err
	}
	values, ok := value.(map[string]any)
	if !ok {
		return domain.ResourceSpec{}, fmt.Errorf("resource spec root must be an object")
	}
	return domain.NewResourceSpec(values)
}

func encodeSpecValue(value any) (specNode, error) {
	scalar := func(kind, value string) specNode { return specNode{Kind: kind, Scalar: &value} }
	switch value := value.(type) {
	case nil:
		return specNode{Kind: "null"}, nil
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
		object := make(map[string]specNode, len(value))
		for key, item := range value {
			encoded, err := encodeSpecValue(item)
			if err != nil {
				return specNode{}, fmt.Errorf("encode property %q: %w", key, err)
			}
			object[key] = encoded
		}
		return specNode{Kind: "object", Object: object}, nil
	case []any:
		list := make([]specNode, len(value))
		for i, item := range value {
			encoded, err := encodeSpecValue(item)
			if err != nil {
				return specNode{}, fmt.Errorf("encode list item %d: %w", i, err)
			}
			list[i] = encoded
		}
		return specNode{Kind: "list", List: list}, nil
	default:
		return specNode{}, fmt.Errorf("unsupported resource spec value %T", value)
	}
}

func decodeSpecValue(node specNode) (any, error) {
	requireScalar := func() (string, error) {
		if node.Scalar == nil || node.Object != nil || node.List != nil {
			return "", fmt.Errorf("malformed %s resource spec value", node.Kind)
		}
		return *node.Scalar, nil
	}
	requireContainer := func(object bool) error {
		if node.Scalar != nil || (object && node.Object == nil) || (!object && node.List == nil) ||
			(object && node.List != nil) || (!object && node.Object != nil) {
			return fmt.Errorf("malformed %s resource spec value", node.Kind)
		}
		return nil
	}
	switch node.Kind {
	case "null":
		if node.Scalar != nil || node.Object != nil || node.List != nil {
			return nil, fmt.Errorf("malformed null resource spec value")
		}
		return nil, nil
	case "bool":
		value, err := requireScalar()
		if err != nil {
			return nil, err
		}
		return strconv.ParseBool(value)
	case "string":
		return requireScalar()
	case "int":
		value, err := parseSigned(node, strconv.IntSize)
		return int(value), err
	case "int8":
		value, err := parseSigned(node, 8)
		return int8(value), err
	case "int16":
		value, err := parseSigned(node, 16)
		return int16(value), err
	case "int32":
		value, err := parseSigned(node, 32)
		return int32(value), err
	case "int64":
		return parseSigned(node, 64)
	case "uint":
		value, err := parseUnsigned(node, strconv.IntSize)
		return uint(value), err
	case "uint8":
		value, err := parseUnsigned(node, 8)
		return uint8(value), err
	case "uint16":
		value, err := parseUnsigned(node, 16)
		return uint16(value), err
	case "uint32":
		value, err := parseUnsigned(node, 32)
		return uint32(value), err
	case "uint64":
		return parseUnsigned(node, 64)
	case "float32":
		value, err := requireScalar()
		if err != nil {
			return nil, err
		}
		bits, err := decodeBits(value, 4)
		if err != nil {
			return nil, err
		}
		return math.Float32frombits(readUint32(bits)), nil
	case "float64":
		value, err := requireScalar()
		if err != nil {
			return nil, err
		}
		bits, err := decodeBits(value, 8)
		if err != nil {
			return nil, err
		}
		return math.Float64frombits(readUint64(bits)), nil
	case "object":
		if err := requireContainer(true); err != nil {
			return nil, err
		}
		object := make(map[string]any, len(node.Object))
		for key, item := range node.Object {
			if key == "" {
				return nil, fmt.Errorf("resource spec property name cannot be empty")
			}
			value, err := decodeSpecValue(item)
			if err != nil {
				return nil, fmt.Errorf("decode property %q: %w", key, err)
			}
			object[key] = value
		}
		return object, nil
	case "list":
		if err := requireContainer(false); err != nil {
			return nil, err
		}
		list := make([]any, len(node.List))
		for i, item := range node.List {
			value, err := decodeSpecValue(item)
			if err != nil {
				return nil, fmt.Errorf("decode list item %d: %w", i, err)
			}
			list[i] = value
		}
		return list, nil
	default:
		return nil, fmt.Errorf("unknown resource spec value kind %q", node.Kind)
	}
}

func parseSigned(node specNode, bits int) (int64, error) {
	if node.Scalar == nil || node.Object != nil || node.List != nil {
		return 0, fmt.Errorf("malformed %s resource spec value", node.Kind)
	}
	value, err := strconv.ParseInt(*node.Scalar, 10, bits)
	if err != nil {
		return 0, fmt.Errorf("decode %s resource spec value: %w", node.Kind, err)
	}
	return value, nil
}

func parseUnsigned(node specNode, bits int) (uint64, error) {
	if node.Scalar == nil || node.Object != nil || node.List != nil {
		return 0, fmt.Errorf("malformed %s resource spec value", node.Kind)
	}
	value, err := strconv.ParseUint(*node.Scalar, 10, bits)
	if err != nil {
		return 0, fmt.Errorf("decode %s resource spec value: %w", node.Kind, err)
	}
	return value, nil
}

func decodeBits(value string, size int) ([]byte, error) {
	bits, err := hex.DecodeString(value)
	if err != nil || len(bits) != size {
		return nil, fmt.Errorf("malformed floating-point resource spec value")
	}
	return bits, nil
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

func readUint32(value []byte) uint32 {
	var result uint32
	for _, item := range value {
		result = result<<8 | uint32(item)
	}
	return result
}

func readUint64(value []byte) uint64 {
	var result uint64
	for _, item := range value {
		result = result<<8 | uint64(item)
	}
	return result
}
