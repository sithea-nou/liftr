// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// ResourceOutputs is the realized, developer-consumable result of one
// successful reconciliation generation. It is deliberately separate from
// ResourceSpec (desired intent) and ResourceStatus (normalized lifecycle
// observation): outputs are neither desired state nor conditions.
//
// The value carries only semantic output content — the producing generation,
// the values themselves, and when Liftr published them. Worker, persistence,
// and mapping provenance live in application records, never here. Values are
// flat non-secret scalars only; nested objects, arrays, null, and secret
// material have no representation.
type ResourceOutputs struct {
	observedGeneration uint64
	values             map[string]any
	publishedAt        time.Time
}

func NewResourceOutputs(observedGeneration uint64, values map[string]any, publishedAt time.Time) (ResourceOutputs, error) {
	if observedGeneration == 0 {
		return ResourceOutputs{}, fmt.Errorf("resource outputs observed generation must be greater than zero")
	}
	if publishedAt.IsZero() {
		return ResourceOutputs{}, fmt.Errorf("resource outputs publication time is required")
	}
	cloned := make(map[string]any, len(values))
	for key, value := range values {
		if strings.TrimSpace(key) == "" || key != strings.TrimSpace(key) {
			return ResourceOutputs{}, fmt.Errorf("resource outputs property name %q is empty or not canonical", key)
		}
		scalar, err := canonicalOutputValue(value)
		if err != nil {
			return ResourceOutputs{}, fmt.Errorf("resource outputs property %q: %w", key, err)
		}
		cloned[key] = scalar
	}
	return ResourceOutputs{observedGeneration: observedGeneration, values: cloned, publishedAt: publishedAt.UTC()}, nil
}

// canonicalOutputValue restricts output values to the closed scalar set and
// normalizes integer widths so identical logical values share one
// representation. Fractional, non-finite, and non-scalar values are rejected
// rather than coerced.
func canonicalOutputValue(value any) (any, error) {
	switch value := value.(type) {
	case string:
		return value, nil
	case bool:
		return value, nil
	case int:
		return int64(value), nil
	case int8:
		return int64(value), nil
	case int16:
		return int64(value), nil
	case int32:
		return int64(value), nil
	case int64:
		return value, nil
	case uint:
		if uint64(value) > math.MaxInt64 {
			return nil, fmt.Errorf("integer value is out of the supported range")
		}
		return int64(value), nil
	case uint8:
		return int64(value), nil
	case uint16:
		return int64(value), nil
	case uint32:
		return int64(value), nil
	case uint64:
		if value > math.MaxInt64 {
			return nil, fmt.Errorf("integer value is out of the supported range")
		}
		return int64(value), nil
	case float32:
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("non-finite number is not supported")
		}
		return float64(value), nil
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("non-finite number is not supported")
		}
		return value, nil
	default:
		return nil, fmt.Errorf("unsupported output value type %T; outputs carry flat scalars only", value)
	}
}

// ObservedGeneration identifies the desired generation whose successful
// reconciliation produced these values. It is an independent dimension from
// ResourceStatus.ObservedGeneration and never claims to be a live health
// observation.
func (o ResourceOutputs) ObservedGeneration() uint64 { return o.observedGeneration }

// Values returns a defensive copy of the realized scalar values.
func (o ResourceOutputs) Values() map[string]any {
	cloned := make(map[string]any, len(o.values))
	for key, value := range o.values {
		cloned[key] = value
	}
	return cloned
}

// PublishedAt reports when Liftr durably published this snapshot.
func (o ResourceOutputs) PublishedAt() time.Time { return o.publishedAt }
