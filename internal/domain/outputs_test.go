// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"math"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
)

var publishAt = time.Date(2026, 8, 23, 9, 0, 0, 0, time.UTC)

func TestNewResourceOutputsAcceptsClosedScalars(t *testing.T) {
	outputs, err := domain.NewResourceOutputs(4, map[string]any{
		"hostname": "orders-db.postgres.example",
		"port":     int64(5432),
		"weight":   float64(1.5),
		"enabled":  true,
	}, publishAt)
	if err != nil {
		t.Fatal(err)
	}
	if outputs.ObservedGeneration() != 4 {
		t.Fatalf("ObservedGeneration = %d", outputs.ObservedGeneration())
	}
	if !outputs.PublishedAt().Equal(publishAt) {
		t.Fatalf("PublishedAt = %v", outputs.PublishedAt())
	}
	values := outputs.Values()
	if values["port"] != int64(5432) || values["hostname"] != "orders-db.postgres.example" {
		t.Fatalf("values = %#v", values)
	}
}

func TestNewResourceOutputsNormalizesIntegerWidths(t *testing.T) {
	outputs, err := domain.NewResourceOutputs(1, map[string]any{
		"a": int32(7),
		"b": uint8(8),
		"c": float32(9),
		"d": int64(math.MaxInt64),
	}, publishAt)
	if err != nil {
		t.Fatal(err)
	}
	values := outputs.Values()
	if values["a"] != int64(7) || values["b"] != int64(8) || values["c"] != float64(9) || values["d"] != int64(math.MaxInt64) {
		t.Fatalf("width normalization failed: %#v", values)
	}
}

func TestNewResourceOutputsRejectsInvalidConstruction(t *testing.T) {
	cases := []struct {
		name       string
		generation uint64
		values     map[string]any
		at         time.Time
	}{
		{"zero generation", 0, map[string]any{"x": "y"}, publishAt},
		{"zero time", 1, map[string]any{"x": "y"}, time.Time{}},
		{"empty key", 1, map[string]any{"": "y"}, publishAt},
		{"padded key", 1, map[string]any{" x ": "y"}, publishAt},
		{"nested object", 1, map[string]any{"conn": map[string]any{"host": "h"}}, publishAt},
		{"array value", 1, map[string]any{"list": []any{1}}, publishAt},
		{"nil value", 1, map[string]any{"x": nil}, publishAt},
		{"NaN", 1, map[string]any{"x": math.NaN()}, publishAt},
		{"infinity", 1, map[string]any{"x": math.Inf(1)}, publishAt},
		{"uint overflow", 1, map[string]any{"x": uint64(math.MaxUint64)}, publishAt},
		{"struct", 1, map[string]any{"x": struct{}{}}, publishAt},
	}
	for _, tc := range cases {
		if _, err := domain.NewResourceOutputs(tc.generation, tc.values, tc.at); err == nil {
			t.Errorf("%s: construction accepted", tc.name)
		}
	}
}

func TestResourceOutputsValuesAreDefensivelyCopied(t *testing.T) {
	source := map[string]any{"hostname": "db.example", "port": int64(5432)}
	outputs, err := domain.NewResourceOutputs(1, source, publishAt)
	if err != nil {
		t.Fatal(err)
	}
	first := outputs.Values()
	first["hostname"] = "mutated"
	first["extra"] = true
	if outputs.Values()["hostname"] != "db.example" {
		t.Fatal("Values() exposed internal state")
	}
	if _, leaked := outputs.Values()["extra"]; leaked {
		t.Fatal("caller mutation leaked into the domain value")
	}
	source["hostname"] = "mutated-source"
	if outputs.Values()["hostname"] != "db.example" {
		t.Fatal("construction did not copy the source map")
	}
}

// TestResourceOutputsCarriesNoProvenance pins Correction K: the semantic
// domain value exposes only generation, values, and publication time. Worker,
// persistence, mapping, and operation provenance live in application records.
func TestResourceOutputsCarriesNoProvenance(t *testing.T) {
	outputs, err := domain.NewResourceOutputs(1, map[string]any{"port": int64(5432)}, publishAt)
	if err != nil {
		t.Fatal(err)
	}
	type provenance interface {
		OperationID() any
		MappingRef() string
		Digest() string
	}
	var _ = outputs
	if _, ok := any(outputs).(provenance); ok {
		t.Fatal("domain ResourceOutputs exposes provenance accessors")
	}
}
