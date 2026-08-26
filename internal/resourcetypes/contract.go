// SPDX-License-Identifier: Apache-2.0

package resourcetypes

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
)

// SemanticValidator applies cross-field or domain rules the JSON Schema
// cannot express. It receives a defensive copy of the spec values and must
// not mutate them.
type SemanticValidator func(values map[string]any) []resourcecontract.Violation

// TransitionViolation reports one illegal old→new spec transition. Path is an
// RFC 6901 JSON Pointer into the submitted (new) spec, Keyword names the
// violated contract rule, and Message is a curated client-safe sentence.
// It is deliberately defined in this package so concrete ResourceType
// implementations can author transition rules without importing any
// orchestration package.
type TransitionViolation struct {
	Path    string
	Keyword string
	Message string
}

// TransitionKeyword is the reserved violation keyword for update-transition
// rules. Admission surfaces these violations through the same structured
// RESOURCE_SPEC_INVALID channel as structural and semantic violations.
const TransitionKeyword = "transition"

// TransitionValidator applies old→new legality rules that belong to the
// developer contract rather than to any implementation. It receives defensive
// copies of the previous and submitted spec values and must mutate neither.
type TransitionValidator func(oldValues, newValues map[string]any) []TransitionViolation

// ContractInput assembles one immutable ResourceType contract.
type ContractInput struct {
	Type        domain.ResourceType
	DisplayName string
	SpecSchema  []byte
	Semantic    SemanticValidator
	Transitions TransitionValidator
	// Outputs declares the developer-consumable non-secret output fields of
	// this ResourceType version. Nil or empty means the type publishes no
	// outputs; a declared output contract is part of the immutable contract.
	Outputs []resourcecontract.OutputField
	// References declares the provider-neutral reference slots of this
	// ResourceType version. Nil or empty means the type participates in no
	// relationships as a source; a declared reference contract is part of the
	// immutable contract and can never be added to a released version.
	References []resourcecontract.ReferenceSlot
}

// Contract is a developer-facing ResourceType: identity, display metadata,
// contract capabilities, the ResourceSpec schema, spec validation, and the
// optional output contract. It satisfies the consumer-side contract port
// structurally through resourcecontract.Contract without importing the
// application.
//
// A contract never carries provisioner references, stacks, workspaces,
// repositories, accounts, credentials, availability, or UI metadata.
type Contract struct {
	resourceType      domain.ResourceType
	displayName       string
	schema            SpecSchema
	semantic          SemanticValidator
	transitions       TransitionValidator
	outputContract    *resourcecontract.OutputContract
	referenceContract *resourcecontract.ReferenceContract
}

var _ resourcecontract.Contract = Contract{}

// NewContract compiles and binds one contract. Registration fails unless the
// schema document compiles under the blocked loader (self-contained), pins
// draft 2020-12, declares an object root, and carries the $id derived from
// the contract identity. A declared output contract must itself be valid;
// it becomes part of this immutable contract instance.
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
	var outputContract *resourcecontract.OutputContract
	if len(input.Outputs) > 0 {
		outputs, err := resourcecontract.NewOutputContract(input.Outputs)
		if err != nil {
			return Contract{}, fmt.Errorf("resource type %s/%s output contract is invalid: %w", input.Type.Ref().Name, input.Type.Ref().Version, err)
		}
		outputContract = &outputs
	} else if input.Outputs != nil {
		return Contract{}, fmt.Errorf("output field declarations cannot be empty")
	}
	var referenceContract *resourcecontract.ReferenceContract
	if len(input.References) > 0 {
		references, err := resourcecontract.NewReferenceContract(input.References)
		if err != nil {
			return Contract{}, fmt.Errorf("resource type %s/%s reference contract is invalid: %w", input.Type.Ref().Name, input.Type.Ref().Version, err)
		}
		referenceContract = &references
	} else if input.References != nil {
		return Contract{}, fmt.Errorf("reference slot declarations cannot be empty")
	}
	return Contract{
		resourceType:      input.Type,
		displayName:       displayName,
		schema:            schema,
		semantic:          input.Semantic,
		transitions:       input.Transitions,
		outputContract:    outputContract,
		referenceContract: referenceContract,
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

// OutputContract returns the declared non-secret output contract, or nil when
// the ResourceType publishes no outputs.
func (c Contract) OutputContract() *resourcecontract.OutputContract { return c.outputContract }

// ReferenceContract returns the declared provider-neutral reference contract,
// or nil when the ResourceType participates in no relationships as a source.
func (c Contract) ReferenceContract() *resourcecontract.ReferenceContract { return c.referenceContract }

// SchemaDigest reports the SHA-256 of the registered schema bytes. It is not
// part of the application contract; it supports registration integrity work
// and tests.
func (c Contract) SchemaDigest() string { return c.schema.Digest() }

// ValidateSpec evaluates submitted intent against the contract. It is a pure
// predicate: structural rules come from the compiled JSON Schema, semantic
// rules from the optional validator hook, and nothing is mutated or
// defaulted. Failures are returned as *resourcecontract.ValidationError with
// sanitized violations in deterministic order; the consuming application owns
// response bounding.
func (c Contract) ValidateSpec(spec domain.ResourceSpec) error {
	values := spec.Values()
	violations := c.schema.violationsFor(values)
	if c.semantic != nil {
		violations = append(violations, c.semantic(values)...)
	}
	if len(violations) == 0 {
		return nil
	}
	return resourcecontract.NewValidationError(c.Ref(), violations)
}

// ValidateUpdate evaluates an old→new spec transition against the contract's
// declared update-transition rules. A schema-valid spec can still be an
// illegal transition; the contract — not any implementation — owns that
// distinction. Like ValidateSpec it is a pure predicate over defensive copies,
// and failures are returned as *resourcecontract.ValidationError with violations
// carrying the reserved transition keyword. Contracts without transition rules
// accept every schema-valid transition.
func (c Contract) ValidateUpdate(oldSpec, newSpec domain.ResourceSpec) error {
	if c.transitions == nil {
		return nil
	}
	raw := c.transitions(oldSpec.Values(), newSpec.Values())
	if len(raw) == 0 {
		return nil
	}
	violations := make([]resourcecontract.Violation, 0, len(raw))
	for _, violation := range raw {
		violations = append(violations, resourcecontract.Violation{Path: violation.Path, Keyword: violation.Keyword, Message: violation.Message})
	}
	return resourcecontract.NewValidationError(c.Ref(), violations)
}

// ValidateOutputValues checks provider-supplied candidate output values
// against this contract's declared output contract. Contracts without outputs
// reject every candidate key: undeclared provider output never reaches
// persistence or clients. Values are validated exactly — unknown names,
// wrong scalar types, and missing required fields are rejected without
// coercion. The input map is not mutated.
func (c Contract) ValidateOutputValues(values map[string]any) error {
	if c.outputContract == nil {
		if len(values) == 0 {
			return nil
		}
		return fmt.Errorf("resource type %s/%s declares no output contract", c.Ref().Name, c.Ref().Version)
	}
	return c.outputContract.Validate(values)
}
