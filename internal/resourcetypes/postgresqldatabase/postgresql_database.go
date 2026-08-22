// SPDX-License-Identifier: Apache-2.0

// Package postgresqldatabase provides the first example ResourceType
// definition. It does not provision or connect to PostgreSQL.
package postgresqldatabase

import (
	"fmt"
	"sync"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/resourcetypes"
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

// specSchemaDocument is the authoritative structural contract for
// PostgreSQLDatabase/v1 developer intent. It contains only fields that exist
// in this repository: version, storageGB, and highAvailability. Structural
// rules live here and nowhere else; unknown properties are rejected.
//
// JSON Schema format keywords would be annotation-only in Liftr; none are
// used. Defaults are documentation-only and Liftr applies none, so every
// property is required.
const specSchemaDocument = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "urn:liftr:resource-type:PostgreSQLDatabase:v1:spec",
  "title": "PostgreSQLDatabase/v1 ResourceSpec",
  "description": "Developer intent for a managed PostgreSQL database. Capabilities are contract capabilities of this resource type, not guarantees of current backend availability.",
  "type": "object",
  "additionalProperties": false,
  "required": ["version", "storageGB", "highAvailability"],
  "properties": {
    "version": {
      "type": "string",
      "minLength": 1,
      "description": "PostgreSQL major version the developer requests."
    },
    "storageGB": {
      "type": "integer",
      "minimum": 1,
      "description": "Requested storage capacity in gigabytes."
    },
    "highAvailability": {
      "type": "boolean",
      "description": "Whether the database must run with high availability."
    }
  }
}`

// SpecSchemaDocument returns the registered schema bytes verbatim.
func SpecSchemaDocument() []byte { return []byte(specSchemaDocument) }

var (
	contractOnce sync.Once
	contract     resourcetypes.Contract
	contractErr  error
)

// Contract returns the singleton PostgreSQLDatabase/v1 developer contract.
func Contract() (resourcetypes.Contract, error) {
	contractOnce.Do(func() {
		typeValue, err := NewResourceType()
		if err != nil {
			contractErr = err
			return
		}
		contract, contractErr = resourcetypes.NewContract(resourcetypes.ContractInput{
			Type:        typeValue,
			DisplayName: "PostgreSQL Database",
			SpecSchema:  SpecSchemaDocument(),
		})
	})
	return contract, contractErr
}

// NewSpec creates example PostgreSQL developer intent without implementation
// details. Validation is delegated to the contract so rules are defined only
// by its schema.
func NewSpec(version string, storageGB int64, highAvailability bool) (domain.ResourceSpec, error) {
	spec, err := domain.NewResourceSpec(map[string]any{
		"version":          version,
		"storageGB":        storageGB,
		"highAvailability": highAvailability,
	})
	if err != nil {
		return domain.ResourceSpec{}, err
	}
	contract, err := Contract()
	if err != nil {
		return domain.ResourceSpec{}, fmt.Errorf("PostgreSQLDatabase contract is invalid: %w", err)
	}
	if err := contract.ValidateSpec(spec); err != nil {
		return domain.ResourceSpec{}, err
	}
	return spec, nil
}
