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
			Transitions: ValidateTransition,
		})
	})
	return contract, contractErr
}

// ValidateTransition declares the update-transition semantics of the
// PostgreSQLDatabase/v1 developer contract. These rules are contract
// semantics that every implementation claiming to support this ResourceType
// must satisfy; they are not derived from any particular backend:
//
//   - version is immutable after creation. Changing a database's engine
//     major version is a migration workflow, which PostgreSQLDatabase/v1
//     does not offer; developers create a new resource instead.
//   - storageGB may only stay equal or grow. Shrinking allocated storage of
//     an existing database is not part of the v1 contract.
//   - highAvailability may change freely in both directions.
//
// The function receives defensive copies and mutates neither. Violations use
// the reserved transition keyword so admission reports them through the same
// structured channel as other spec-contract violations.
func ValidateTransition(oldValues, newValues map[string]any) []resourcetypes.TransitionViolation {
	var violations []resourcetypes.TransitionViolation
	if oldVersion, oldOK := stringField(oldValues, "version"); oldOK {
		if newVersion, newOK := stringField(newValues, "version"); newOK && newVersion != oldVersion {
			violations = append(violations, resourcetypes.TransitionViolation{Path: "/version", Keyword: resourcetypes.TransitionKeyword,
				Message: "the engine version of an existing PostgreSQLDatabase/v1 resource cannot be changed; request a new resource instead"})
		}
	}
	if oldStorage, oldOK := storageField(oldValues); oldOK {
		if newStorage, newOK := storageField(newValues); newOK && newStorage < oldStorage {
			violations = append(violations, resourcetypes.TransitionViolation{Path: "/storageGB", Keyword: resourcetypes.TransitionKeyword,
				Message: "storageGB cannot decrease for an existing PostgreSQLDatabase/v1 resource"})
		}
	}
	return violations
}

func stringField(values map[string]any, key string) (string, bool) {
	value, ok := values[key]
	if !ok {
		return "", false
	}
	text, ok := value.(string)
	return text, ok
}

// storageField reads storageGB through the shared safe numeric conversion so
// int64 and integral float64 representations compare identically.
func storageField(values map[string]any) (int64, bool) {
	value, ok := values["storageGB"]
	if !ok {
		return 0, false
	}
	return domain.IntegralValue(value)
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
