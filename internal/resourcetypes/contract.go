// SPDX-License-Identifier: Apache-2.0

package resourcetypes

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
)

// SemanticValidator applies cross-field or domain rules the JSON Schema
// cannot express. It receives a defensive copy of the spec values and must
// not mutate them.
type SemanticValidator func(values map[string]any) []application.SpecViolation

// ContractInput assembles one immutable ResourceType contract.
type ContractInput struct {
	Type        domain.ResourceType
	DisplayName string
	SpecSchema  []byte
	Semantic    SemanticValidator
}

// Contract is a developer-facing ResourceType: identity, display metadata,
// contract capabilities, the ResourceSpec schema, and spec validation. It
// satisfies application.ResourceContract structurally without importing it.
//
// A contract never carries provisioner references, stacks, workspaces,
// repositories, accounts, credentials, availability, or UI metadata.
type Contract struct {
	resourceType domain.ResourceType
	displayName  string
	schema       SpecSchema
	semantic     SemanticValidator
}

var _ application.ResourceContract = Contract{}

// NewContract compiles and binds one contract. Registration fails unless the
// schema document compiles under the blocked loader (self-contained), pins
// draft 2020-12, declares an object root, and carries the $id derived from
// the contract identity.
func NewContract(input ContractInput) (Contract, error) {
	schema, err := CompileSpecSchema(input.SpecSchema)
	if err != nil {
		return Contract{}, err
	}
	expectedID := SchemaID(input.Type.Ref())
	if schema.ID() != expectedID {
		return Contract{}, fmt.Errorf("spec schema $id %q does not match the required schema identity %q", schema.ID(), expectedID)
	}
	displayName := input.DisplayName
	if strings.TrimSpace(displayName) == "" {
		displayName = input.Type.Ref().Name
	}
	return Contract{
		resourceType: input.Type,
		displayName:  displayName,
		schema:       schema,
		semantic:     input.Semantic,
	}, nil
}

func (c Contract) Ref() domain.ResourceTypeRef { return c.resourceType.Ref() }

func (c Contract) DisplayName() string { return c.displayName }

func (c Contract) Description() string { return c.resourceType.Description() }

func (c Contract) Capabilities() []domain.Capability {
	capabilities := c.resourceType.Capabilities()
	sort.Slice(capabilities, func(i, j int) bool { return capabilities[i] < capabilities[j] })
	return capabilities
}

// Domain exposes the lifecycle-facing domain type.
func (c Contract) Domain() domain.ResourceType { return c.resourceType }

// SpecSchema returns the registered JSON Schema document verbatim for
// discovery.
func (c Contract) SpecSchema() json.RawMessage { return c.schema.Document() }

// SchemaDigest reports the SHA-256 of the registered schema bytes. It is not
// part of the application contract; it supports registration integrity work
// and tests.
func (c Contract) SchemaDigest() string { return c.schema.Digest() }

// ValidateSpec evaluates submitted intent against the contract. It is a pure
// predicate: structural rules come from the compiled JSON Schema, semantic
// rules from the optional validator hook, and nothing is mutated or
// defaulted. Failures are returned as *application.InvalidSpecError with
// sanitized violations in deterministic order.
func (c Contract) ValidateSpec(spec domain.ResourceSpec) error {
	values := spec.Values()
	violations := c.schema.violationsFor(values)
	if c.semantic != nil {
		violations = append(violations, c.semantic(values)...)
	}
	if len(violations) == 0 {
		return nil
	}
	return application.NewInvalidSpecError(c.Ref(), violations)
}
