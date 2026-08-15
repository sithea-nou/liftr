// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"math"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
)

func TestNewResourceSpec(t *testing.T) {
	tests := []struct {
		name    string
		values  map[string]any
		wantErr bool
	}{
		{name: "empty", values: nil},
		{name: "portable values", values: map[string]any{"enabled": true, "size": int64(10), "options": []any{"a", map[string]any{"b": 2}}}},
		{name: "empty property", values: map[string]any{"": "value"}, wantErr: true},
		{name: "unsupported value", values: map[string]any{"callback": func() {}}, wantErr: true},
		{name: "non-finite number", values: map[string]any{"ratio": math.Inf(1)}, wantErr: true},
		{name: "unsupported typed slice", values: map[string]any{"items": []string{"a"}}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := domain.NewResourceSpec(tt.values)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewResourceSpec() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestResourceSpecOwnsMutableState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(source map[string]any, returned map[string]any)
		check  func(t *testing.T, values map[string]any)
	}{
		{
			name: "source map",
			mutate: func(source map[string]any, _ map[string]any) {
				source["version"] = "changed"
			},
			check: func(t *testing.T, values map[string]any) {
				if got := values["version"]; got != "16" {
					t.Fatalf("version = %v, want 16", got)
				}
			},
		},
		{
			name: "nested source map",
			mutate: func(source map[string]any, _ map[string]any) {
				source["settings"].(map[string]any)["mode"] = "changed"
			},
			check: func(t *testing.T, values map[string]any) {
				if got := values["settings"].(map[string]any)["mode"]; got != "safe" {
					t.Fatalf("mode = %v, want safe", got)
				}
			},
		},
		{
			name: "returned map",
			mutate: func(_ map[string]any, returned map[string]any) {
				returned["version"] = "changed"
			},
			check: func(t *testing.T, values map[string]any) {
				if got := values["version"]; got != "16" {
					t.Fatalf("version = %v, want 16", got)
				}
			},
		},
		{
			name: "nested returned list",
			mutate: func(_ map[string]any, returned map[string]any) {
				returned["replicas"].([]any)[0] = "changed"
			},
			check: func(t *testing.T, values map[string]any) {
				if got := values["replicas"].([]any)[0]; got != "primary" {
					t.Fatalf("replica = %v, want primary", got)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := map[string]any{
				"version":  "16",
				"settings": map[string]any{"mode": "safe"},
				"replicas": []any{"primary"},
			}
			spec, err := domain.NewResourceSpec(source)
			if err != nil {
				t.Fatalf("NewResourceSpec() error = %v", err)
			}

			tt.mutate(source, spec.Values())
			tt.check(t, spec.Values())
		})
	}
}
