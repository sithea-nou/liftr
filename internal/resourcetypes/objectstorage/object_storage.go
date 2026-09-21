// SPDX-License-Identifier: Apache-2.0

// Package objectstorage defines the provider-neutral contract used by the
// opt-in Azure Storage acceptance scenarios. It is intentionally not added to
// the production server catalog yet; the acceptance scenarios qualify two
// implementations without expanding Liftr's advertised production surface.
package objectstorage

import (
	"fmt"
	"sync"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
	"github.com/sithea-nou/liftr/internal/resourcetypes"
)

const (
	Name    = "ObjectStorage"
	Version = "v1"
)

func TypeRef() domain.ResourceTypeRef {
	return domain.ResourceTypeRef{Name: Name, Version: Version}
}

func NewResourceType() (domain.ResourceType, error) {
	return domain.NewResourceType(
		TypeRef(),
		"A managed object-storage endpoint requested through a provisioner-neutral contract.",
		[]domain.Capability{
			domain.CapabilityCreate,
			domain.CapabilityUpdate,
			domain.CapabilityDelete,
			domain.CapabilityObserve,
		},
	)
}

const specSchemaDocument = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "urn:liftr:resource-type:ObjectStorage:v1:spec",
  "title": "ObjectStorage/v1 ResourceSpec",
  "description": "Developer intent for a managed object-storage endpoint. Capabilities are contract capabilities, not guarantees of current backend availability.",
  "type": "object",
  "additionalProperties": false,
  "required": ["accessFrequency", "permitAnonymousAccess"],
  "properties": {
    "accessFrequency": {
      "type": "string",
      "enum": ["frequent", "infrequent"],
      "description": "Expected access frequency used to select the service storage tier."
    },
    "permitAnonymousAccess": {
      "type": "boolean",
      "description": "Whether the service may expose objects for anonymous access when separately configured."
    }
  }
}`

func SpecSchemaDocument() []byte { return []byte(specSchemaDocument) }

var (
	contractOnce sync.Once
	contract     resourcetypes.Contract
	contractErr  error
)

func Contract() (resourcetypes.Contract, error) {
	contractOnce.Do(func() {
		typeValue, err := NewResourceType()
		if err != nil {
			contractErr = err
			return
		}
		contract, contractErr = resourcetypes.NewContract(resourcetypes.ContractInput{
			Type:        typeValue,
			DisplayName: "Object Storage",
			SpecSchema:  SpecSchemaDocument(),
			Outputs: []resourcecontract.OutputField{{
				Name: "endpoint", JSONType: resourcecontract.OutputTypeString, RequiredWhenReady: true,
			}},
		})
	})
	return contract, contractErr
}

// NewSpec creates provider-neutral object-storage intent and validates it
// against the same contract used by the acceptance implementations.
func NewSpec(accessFrequency string, permitAnonymousAccess bool) (domain.ResourceSpec, error) {
	spec, err := domain.NewResourceSpec(map[string]any{
		"accessFrequency":       accessFrequency,
		"permitAnonymousAccess": permitAnonymousAccess,
	})
	if err != nil {
		return domain.ResourceSpec{}, err
	}
	contract, err := Contract()
	if err != nil {
		return domain.ResourceSpec{}, fmt.Errorf("ObjectStorage contract is invalid: %w", err)
	}
	if err := contract.ValidateSpec(spec); err != nil {
		return domain.ResourceSpec{}, err
	}
	return spec, nil
}
