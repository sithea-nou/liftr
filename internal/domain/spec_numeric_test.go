// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"math"
	"testing"
)

func TestIntegralValueAcceptsIntegralNumbers(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  int64
	}{
		{name: "int", value: 20, want: 20},
		{name: "int8", value: int8(20), want: 20},
		{name: "int16", value: int16(20), want: 20},
		{name: "int32", value: int32(20), want: 20},
		{name: "int64", value: int64(20), want: 20},
		{name: "uint", value: uint(20), want: 20},
		{name: "uint8", value: uint8(20), want: 20},
		{name: "uint16", value: uint16(20), want: 20},
		{name: "uint32", value: uint32(20), want: 20},
		{name: "uint64", value: uint64(20), want: 20},
		{name: "float32 integral literal", value: float32(20), want: 20},
		{name: "float64 integral literal", value: float64(20), want: 20},
		{name: "float64 negative integral", value: float64(-3), want: -3},
		{name: "max int64", value: int64(math.MaxInt64), want: math.MaxInt64},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := IntegralValue(test.value)
			if !ok || got != test.want {
				t.Fatalf("IntegralValue(%v) = (%d, %v), want (%d, true)", test.value, got, ok, test.want)
			}
		})
	}
}

func TestIntegralValueRejectsNonIntegralAndUnsafeValues(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{name: "fractional float64", value: 20.5},
		{name: "fractional float32", value: float32(0.1)},
		{name: "float64 NaN", value: math.NaN()},
		{name: "float64 positive infinity", value: math.Inf(1)},
		{name: "float64 negative infinity", value: math.Inf(-1)},
		{name: "float64 beyond exact integer range", value: math.Ldexp(1, 54)},
		{name: "uint64 beyond int64 range", value: uint64(math.MaxUint64)},
		{name: "string", value: "20"},
		{name: "bool", value: true},
		{name: "nil", value: nil},
		{name: "map", value: map[string]any{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got, ok := IntegralValue(test.value); ok {
				t.Fatalf("IntegralValue(%v) = (%d, true), want rejection", test.value, got)
			}
		})
	}
}

// TestIntegralValueMapsIntegerAndDecimalRepresentationsIdentically pins the M9
// numeric contract: schema-valid integer values represented as int64 and as an
// integral float64 must consume identically downstream.
func TestIntegralValueMapsIntegerAndDecimalRepresentationsIdentically(t *testing.T) {
	fromInt, intOK := IntegralValue(int64(20))
	fromFloat, floatOK := IntegralValue(float64(20.0))
	if !intOK || !floatOK || fromInt != fromFloat {
		t.Fatalf("representations diverged: int64=(%d,%v) float64=(%d,%v)", fromInt, intOK, fromFloat, floatOK)
	}
}
