// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
)

func TestNewResource(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	spec, err := domain.NewResourceSpec(map[string]any{"size": int64(10)})
	if err != nil {
		t.Fatalf("NewResourceSpec() error = %v", err)
	}

	tests := []struct {
		name    string
		id      domain.ResourceID
		typeRef domain.ResourceTypeRef
		owner   domain.OwnerRef
		at      time.Time
		wantErr bool
	}{
		{name: "valid", id: "resource-1", typeRef: domain.ResourceTypeRef{Name: "Database", Version: "v1"}, owner: domain.OwnerRef{Kind: "team", ID: "payments"}, at: now},
		{name: "missing ID", typeRef: domain.ResourceTypeRef{Name: "Database", Version: "v1"}, owner: domain.OwnerRef{Kind: "team", ID: "payments"}, at: now, wantErr: true},
		{name: "missing type name", id: "resource-1", typeRef: domain.ResourceTypeRef{Version: "v1"}, owner: domain.OwnerRef{Kind: "team", ID: "payments"}, at: now, wantErr: true},
		{name: "missing type version", id: "resource-1", typeRef: domain.ResourceTypeRef{Name: "Database"}, owner: domain.OwnerRef{Kind: "team", ID: "payments"}, at: now, wantErr: true},
		{name: "missing owner kind", id: "resource-1", typeRef: domain.ResourceTypeRef{Name: "Database", Version: "v1"}, owner: domain.OwnerRef{ID: "payments"}, at: now, wantErr: true},
		{name: "missing owner ID", id: "resource-1", typeRef: domain.ResourceTypeRef{Name: "Database", Version: "v1"}, owner: domain.OwnerRef{Kind: "team"}, at: now, wantErr: true},
		{name: "missing creation time", id: "resource-1", typeRef: domain.ResourceTypeRef{Name: "Database", Version: "v1"}, owner: domain.OwnerRef{Kind: "team", ID: "payments"}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resource, err := domain.NewResource(tt.id, tt.typeRef, tt.owner, spec, tt.at)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewResource() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if resource.Generation() != 1 {
				t.Fatalf("Generation() = %d, want 1", resource.Generation())
			}
			if resource.Owner() != tt.owner {
				t.Fatalf("Owner() = %#v, want %#v", resource.Owner(), tt.owner)
			}
		})
	}
}

func TestResourceUpdateSpec(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	spec, _ := domain.NewResourceSpec(map[string]any{"size": int64(10)})
	updatedSpec, _ := domain.NewResourceSpec(map[string]any{"size": int64(20)})

	tests := []struct {
		name           string
		updatedAt      time.Time
		wantErr        bool
		wantGeneration uint64
	}{
		{name: "new revision", updatedAt: now.Add(time.Minute), wantGeneration: 2},
		{name: "same timestamp", updatedAt: now, wantGeneration: 2},
		{name: "missing timestamp", wantErr: true, wantGeneration: 1},
		{name: "earlier timestamp", updatedAt: now.Add(-time.Minute), wantErr: true, wantGeneration: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resource, err := domain.NewResource(
				"resource-1",
				domain.ResourceTypeRef{Name: "Database", Version: "v1"},
				domain.OwnerRef{Kind: "team", ID: "payments"},
				spec,
				now,
			)
			if err != nil {
				t.Fatalf("NewResource() error = %v", err)
			}

			err = resource.UpdateSpec(updatedSpec, tt.updatedAt)
			if (err != nil) != tt.wantErr {
				t.Fatalf("UpdateSpec() error = %v, wantErr %v", err, tt.wantErr)
			}
			if resource.Generation() != tt.wantGeneration {
				t.Fatalf("Generation() = %d, want %d", resource.Generation(), tt.wantGeneration)
			}
		})
	}
}
