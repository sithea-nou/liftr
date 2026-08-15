// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"fmt"
	"math"
)

// ResourceSpec is opaque, validated developer intent. Its mutable representation is never exposed.
type ResourceSpec struct {
	values map[string]any
}

func NewResourceSpec(values map[string]any) (ResourceSpec, error) {
	if values == nil {
		values = map[string]any{}
	}

	cloned, err := cloneObject(values)
	if err != nil {
		return ResourceSpec{}, err
	}
	return ResourceSpec{values: cloned}, nil
}

// Values returns a deep copy suitable for passing across a boundary.
func (s ResourceSpec) Values() map[string]any {
	cloned, err := cloneObject(s.values)
	if err != nil {
		panic("domain.ResourceSpec contains an invalid value: " + err.Error())
	}
	return cloned
}

func cloneObject(values map[string]any) (map[string]any, error) {
	cloned := make(map[string]any, len(values))
	for key, value := range values {
		if key == "" {
			return nil, fmt.Errorf("resource spec property name cannot be empty")
		}
		copyValue, err := cloneSpecValue(value)
		if err != nil {
			return nil, fmt.Errorf("resource spec property %q: %w", key, err)
		}
		cloned[key] = copyValue
	}
	return cloned, nil
}

func cloneSpecValue(value any) (any, error) {
	switch value := value.(type) {
	case nil, bool, string,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return value, nil
	case float32:
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("non-finite number is not supported")
		}
		return value, nil
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("non-finite number is not supported")
		}
		return value, nil
	case map[string]any:
		return cloneObject(value)
	case []any:
		cloned := make([]any, len(value))
		for i, item := range value {
			copyValue, err := cloneSpecValue(item)
			if err != nil {
				return nil, fmt.Errorf("list item %d: %w", i, err)
			}
			cloned[i] = copyValue
		}
		return cloned, nil
	default:
		return nil, fmt.Errorf("unsupported value type %T", value)
	}
}
