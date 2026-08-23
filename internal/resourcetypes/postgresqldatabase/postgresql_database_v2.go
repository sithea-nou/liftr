// SPDX-License-Identifier: Apache-2.0

package postgresqldatabase

import (
	"fmt"
	"sync"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
	"github.com/sithea-nou/liftr/internal/resourcetypes"
)

// VersionV2 identifies the PostgreSQLDatabase/v2 developer contract.
//
// ADR-0011: a released ResourceTypeRef is immutable, and output declarations
// are part of the developer-facing contract. PostgreSQLDatabase/v1 was
// released with a spec-only contract and no outputs, so it stays exactly as
// registered. M10 introduces v2 — same developer input fields and transition
// rules — that additionally declares the required non-secret realized values:
//
//	hostname  string   required — server endpoint developers connect to
//	port      integer  required — PostgreSQL wire-protocol port (5432)
//
// There is no v1→v2 migration: existing Resources keep their identity, their
// provisioner binding, and their spec-only contract. Clients select v2
// explicitly; no "latest" alias exists.
const VersionV2 = "v2"

// V2TypeRef returns the immutable reference of the v2 contract.
func V2TypeRef() domain.ResourceTypeRef {
	return domain.ResourceTypeRef{Name: Name, Version: VersionV2}
}

func newResourceTypeV2() (domain.ResourceType, error) {
	return domain.NewResourceType(
		V2TypeRef(),
		"A managed PostgreSQL database requested through a provisioner-neutral contract.",
		[]domain.Capability{
			domain.CapabilityCreate,
			domain.CapabilityUpdate,
			domain.CapabilityDelete,
			domain.CapabilityObserve,
		},
	)
}

// specSchemaDocumentV2 is the authoritative structural contract for
// PostgreSQLDatabase/v2 developer intent. It declares exactly the v1 input
// fields — version, storageGB, highAvailability — under the v2 schema
// identity. Structural rules match v1 field for field; unknown properties are
// rejected.
//
// JSON Schema format keywords would be annotation-only in Liftr; none are
// used. Defaults are documentation-only and Liftr applies none, so every
// property is required.
const specSchemaDocumentV2 = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "urn:liftr:resource-type:PostgreSQLDatabase:v2:spec",
  "title": "PostgreSQLDatabase/v2 ResourceSpec",
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

// SpecSchemaDocumentV2 returns the v2 schema bytes verbatim.
func SpecSchemaDocumentV2() []byte { return []byte(specSchemaDocumentV2) }

// OutputFieldsV2 declares the complete v2 output contract. Both fields are
// required reconciliation postconditions: a create or update whose managed
// infrastructure succeeded without them is an incomplete implementation of
// this contract, not a success. The declared names are provider-neutral;
// hostname is an opaque endpoint string, and port is derived by the private
// implementation mapping rather than trusted from any provider payload.
func OutputFieldsV2() []resourcecontract.OutputField {
	return []resourcecontract.OutputField{
		{Name: "hostname", JSONType: resourcecontract.OutputTypeString, RequiredWhenReady: true},
		{Name: "port", JSONType: resourcecontract.OutputTypeInteger, RequiredWhenReady: true},
	}
}

var (
	contractV2Once sync.Once
	contractV2     resourcetypes.Contract
	contractV2Err  error
)

// ContractV2 returns the singleton PostgreSQLDatabase/v2 developer contract.
// It never mutates or replaces the v1 singleton.
func ContractV2() (resourcetypes.Contract, error) {
	contractV2Once.Do(func() {
		typeValue, err := newResourceTypeV2()
		if err != nil {
			contractV2Err = err
			return
		}
		contractV2, contractV2Err = resourcetypes.NewContract(resourcetypes.ContractInput{
			Type:        typeValue,
			DisplayName: "PostgreSQL Database",
			SpecSchema:  SpecSchemaDocumentV2(),
			Transitions: ValidateTransitionV2,
			Outputs:     OutputFieldsV2(),
		})
	})
	return contractV2, contractV2Err
}

// ValidateTransitionV2 declares the v2 update-transition semantics. The rules
// are identical to v1 — version immutable, storage grow-only, availability
// free — because the input contract did not change between versions; only the
// curated messages name the correct contract version.
func ValidateTransitionV2(oldValues, newValues map[string]any) []resourcetypes.TransitionViolation {
	return transitionViolations(oldValues, newValues, VersionV2)
}

// transitionViolations builds the shared transition rules parameterized by
// the contract version named in curated messages.
func transitionViolations(oldValues, newValues map[string]any, version string) []resourcetypes.TransitionViolation {
	var violations []resourcetypes.TransitionViolation
	if oldVersion, oldOK := stringField(oldValues, "version"); oldOK {
		if newVersion, newOK := stringField(newValues, "version"); newOK && newVersion != oldVersion {
			violations = append(violations, resourcetypes.TransitionViolation{Path: "/version", Keyword: resourcetypes.TransitionKeyword,
				Message: fmt.Sprintf("the engine version of an existing PostgreSQLDatabase/%s resource cannot be changed; request a new resource instead", version)})
		}
	}
	if oldStorage, oldOK := storageField(oldValues); oldOK {
		if newStorage, newOK := storageField(newValues); newOK && newStorage < oldStorage {
			violations = append(violations, resourcetypes.TransitionViolation{Path: "/storageGB", Keyword: resourcetypes.TransitionKeyword,
				Message: fmt.Sprintf("storageGB cannot decrease for an existing PostgreSQLDatabase/%s resource", version)})
		}
	}
	return violations
}

// NewSpecV2 creates example v2 developer intent without implementation
// details. Validation is delegated to the v2 contract so rules are defined
// only by its schema and transition rules.
func NewSpecV2(version string, storageGB int64, highAvailability bool) (domain.ResourceSpec, error) {
	spec, err := domain.NewResourceSpec(map[string]any{
		"version":          version,
		"storageGB":        storageGB,
		"highAvailability": highAvailability,
	})
	if err != nil {
		return domain.ResourceSpec{}, err
	}
	contract, err := ContractV2()
	if err != nil {
		return domain.ResourceSpec{}, fmt.Errorf("PostgreSQLDatabase/v2 contract is invalid: %w", err)
	}
	if err := contract.ValidateSpec(spec); err != nil {
		return domain.ResourceSpec{}, err
	}
	return spec, nil
}
