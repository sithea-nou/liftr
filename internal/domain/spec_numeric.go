// SPDX-License-Identifier: Apache-2.0

package domain

import "math"

// maxExactIntegerFloat is 2^53, the largest magnitude at which every integral
// float64 value is exactly representable. Values beyond it may round silently,
// so they are rejected instead of being converted with altered precision.
const maxExactIntegerFloat = 9007199254740992.0

// IntegralValue reports whether a ResourceSpec value carries an integral
// number and returns that value as int64. It implements JSON Schema integer
// semantics over Liftr's preserved numeric representations: any signed or
// unsigned integer width is accepted within int64 range, and any float32 or
// float64 with a zero fractional part is accepted only while its value is
// exactly representable.
//
// The bool result is false for non-numeric values, non-finite floats,
// fractional values, and values outside the exact-representable range. Callers
// must not type-assert concrete numeric types out of a ResourceSpec; they
// should route every numeric consumption through this function so that
// int64(20), int(20), and float64(20.0) map to the same infrastructure value.
func IntegralValue(value any) (int64, bool) {
	switch value := value.(type) {
	case int:
		return int64(value), true
	case int8:
		return int64(value), true
	case int16:
		return int64(value), true
	case int32:
		return int64(value), true
	case int64:
		return value, true
	case uint:
		return fromUint64(uint64(value))
	case uint8:
		return int64(value), true
	case uint16:
		return int64(value), true
	case uint32:
		return int64(value), true
	case uint64:
		return fromUint64(value)
	case float32:
		return fromFloat(float64(value))
	case float64:
		return fromFloat(value)
	default:
		return 0, false
	}
}

func fromUint64(value uint64) (int64, bool) {
	if value > math.MaxInt64 {
		return 0, false
	}
	return int64(value), true
}

func fromFloat(value float64) (int64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	if value != math.Trunc(value) || math.Abs(value) > maxExactIntegerFloat {
		return 0, false
	}
	return int64(value), true
}
