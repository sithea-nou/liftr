// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
)

func TestNewResourceType(t *testing.T) {
	tests := []struct {
		name         string
		ref          domain.ResourceTypeRef
		description  string
		capabilities []domain.Capability
		wantErr      bool
	}{
		{name: "lifecycle and operational capabilities", ref: domain.ResourceTypeRef{Name: "Database", Version: "v1"}, description: "A database", capabilities: []domain.Capability{domain.CapabilityCreate, domain.CapabilityObserve}},
		{name: "extensible capability", ref: domain.ResourceTypeRef{Name: "Database", Version: "v1"}, description: "A database", capabilities: []domain.Capability{"backup"}},
		{name: "missing reference", description: "A database", capabilities: []domain.Capability{domain.CapabilityCreate}, wantErr: true},
		{name: "missing description", ref: domain.ResourceTypeRef{Name: "Database", Version: "v1"}, capabilities: []domain.Capability{domain.CapabilityCreate}, wantErr: true},
		{name: "missing capabilities", ref: domain.ResourceTypeRef{Name: "Database", Version: "v1"}, description: "A database", wantErr: true},
		{name: "empty capability", ref: domain.ResourceTypeRef{Name: "Database", Version: "v1"}, description: "A database", capabilities: []domain.Capability{""}, wantErr: true},
		{name: "duplicate capability", ref: domain.ResourceTypeRef{Name: "Database", Version: "v1"}, description: "A database", capabilities: []domain.Capability{domain.CapabilityCreate, domain.CapabilityCreate}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resourceType, err := domain.NewResourceType(tt.ref, tt.description, tt.capabilities)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewResourceType() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !resourceType.Supports(tt.capabilities[0]) {
				t.Fatalf("Supports(%q) = false, want true", tt.capabilities[0])
			}

			returned := resourceType.Capabilities()
			returned[0] = "changed"
			if resourceType.Supports("changed") {
				t.Fatal("Capabilities() exposed mutable state")
			}
		})
	}
}
