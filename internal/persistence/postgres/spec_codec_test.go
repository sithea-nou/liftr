// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"math"
	"reflect"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
)

func TestResourceSpecCodecRoundTrip(t *testing.T) {
	values := map[string]any{
		"nil": nil, "bool": true, "string": "value",
		"int": int(math.MinInt), "int8": int8(math.MinInt8), "int16": int16(math.MinInt16), "int32": int32(math.MinInt32), "int64": int64(math.MinInt64),
		"uint": uint(math.MaxUint), "uint8": uint8(math.MaxUint8), "uint16": uint16(math.MaxUint16), "uint32": uint32(math.MaxUint32), "uint64": uint64(math.MaxUint64),
		"float32": math.Float32frombits(0x3f000001), "float64": math.Float64frombits(0x3fe0000000000001),
		"nested": map[string]any{"list": []any{int8(1), map[string]any{"x": uint64(2)}, []any{float32(3)}}},
	}
	spec, err := domain.NewResourceSpec(values)
	if err != nil {
		t.Fatal(err)
	}
	version, encoded, err := encodeResourceSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeResourceSpec(version, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Values(), values) {
		t.Fatalf("round trip mismatch\n got: %#v\nwant: %#v", decoded.Values(), values)
	}
}

func TestResourceSpecCodecRejectsMalformedValues(t *testing.T) {
	tests := [][]byte{
		[]byte(`{"kind":"uint8","scalar":"256"}`),
		[]byte(`{"kind":"float64","scalar":"00"}`),
		[]byte(`{"kind":"unknown","scalar":"x"}`),
		[]byte(`{"kind":"object"}`),
		[]byte(`{"kind":"list"}`),
		[]byte(`{"kind":"object","object":{"":{"kind":"null"}}}`),
		[]byte(`{"kind":"null","scalar":"x"}`),
		[]byte(`{"kind":"object","object":{},"extra":true}`),
		[]byte(`{"kind":"object","object":{}} {"kind":"null"}`),
	}
	for _, encoded := range tests {
		if _, err := decodeResourceSpec(resourceSpecCodecVersion, encoded); err == nil {
			t.Fatalf("decodeResourceSpec(%s) succeeded", encoded)
		}
	}
	if _, err := decodeResourceSpec(999, []byte(`{"kind":"object","object":{}}`)); err == nil {
		t.Fatal("unsupported codec version succeeded")
	}
}
