// SPDX-License-Identifier: Apache-2.0

package postgresqldatabase_test

import (
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/resourcetypes/postgresqldatabase"
)

func TestResourceType(t *testing.T) {
	tests := []struct {
		name       string
		capability domain.Capability
	}{
		{name: "create", capability: domain.CapabilityCreate},
		{name: "update", capability: domain.CapabilityUpdate},
		{name: "delete", capability: domain.CapabilityDelete},
		{name: "observe", capability: domain.CapabilityObserve},
	}

	resourceType, err := postgresqldatabase.NewResourceType()
	if err != nil {
		t.Fatalf("NewResourceType() error = %v", err)
	}
	if resourceType.Ref() != postgresqldatabase.TypeRef() {
		t.Fatalf("Ref() = %#v, want %#v", resourceType.Ref(), postgresqldatabase.TypeRef())
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !resourceType.Supports(tt.capability) {
				t.Fatalf("Supports(%q) = false, want true", tt.capability)
			}
		})
	}
}

func TestNewSpec(t *testing.T) {
	tests := []struct {
		name             string
		version          string
		storageGB        int64
		highAvailability bool
		wantErr          bool
	}{
		{name: "valid", version: "16", storageGB: 20, highAvailability: true},
		{name: "missing version", storageGB: 20, wantErr: true},
		{name: "zero storage", version: "16", wantErr: true},
		{name: "negative storage", version: "16", storageGB: -1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, err := postgresqldatabase.NewSpec(tt.version, tt.storageGB, tt.highAvailability)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewSpec() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got := spec.Values()["storageGB"]; got != tt.storageGB {
				t.Fatalf("storageGB = %v, want %d", got, tt.storageGB)
			}
		})
	}
}
