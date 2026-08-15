// SPDX-License-Identifier: Apache-2.0

// Package postgresqldatabase provides an example ResourceType definition.
// It does not provision or connect to PostgreSQL.
package postgresqldatabase

import (
	"fmt"
	"strings"

	"github.com/sithea-nou/liftr/internal/domain"
)

const (
	Name    = "PostgreSQLDatabase"
	Version = "v1"
)

func TypeRef() domain.ResourceTypeRef {
	return domain.ResourceTypeRef{Name: Name, Version: Version}
}

func NewResourceType() (domain.ResourceType, error) {
	return domain.NewResourceType(
		TypeRef(),
		"A managed PostgreSQL database requested through a provisioner-neutral contract.",
		[]domain.Capability{
			domain.CapabilityCreate,
			domain.CapabilityUpdate,
			domain.CapabilityDelete,
			domain.CapabilityObserve,
		},
	)
}

// NewSpec creates example PostgreSQL developer intent without implementation details.
func NewSpec(version string, storageGB int64, highAvailability bool) (domain.ResourceSpec, error) {
	if strings.TrimSpace(version) == "" {
		return domain.ResourceSpec{}, fmt.Errorf("PostgreSQL version is required")
	}
	if storageGB <= 0 {
		return domain.ResourceSpec{}, fmt.Errorf("storageGB must be greater than zero")
	}

	return domain.NewResourceSpec(map[string]any{
		"version":          version,
		"storageGB":        storageGB,
		"highAvailability": highAvailability,
	})
}
