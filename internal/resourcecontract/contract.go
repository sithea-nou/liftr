// SPDX-License-Identifier: Apache-2.0

package resourcecontract

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/sithea-nou/liftr/internal/domain"
)

// OutputType enumerates the closed set of public output scalar types. M10
// outputs are flat, non-secret scalars only; nested objects, arrays, null,
// and secret material have no representation.
type OutputType string

const (
	OutputTypeString  OutputType = "string"
	OutputTypeInteger OutputType = "integer"
	OutputTypeNumber  OutputType = "number"
	OutputTypeBoolean OutputType = "boolean"
)

func (t OutputType) valid() bool {
	switch t {
	case OutputTypeString, OutputTypeInteger, OutputTypeNumber, OutputTypeBoolean:
		return true
	default:
		return false
	}
}

// reservedOutputNames cannot be declared as output fields because they would
// collide with the public envelope or with Liftr-reserved vocabulary.
var reservedOutputNames = map[string]struct{}{
	"observedGeneration": {},
	"values":             {},
}

// OutputField declares one named developer-consumable output of a ResourceType.
// RequiredWhenReady means a successfully reconciled generation must publish
// this field before Liftr may report reconciliation success; it is lifecycle
// postcondition semantics, not structural optionality.
type OutputField struct {
	Name              string     `json:"name"`
	JSONType          OutputType `json:"jsonType"`
	RequiredWhenReady bool       `json:"requiredWhenReady"`
}

// OutputContract is the immutable, ResourceType-owned description of the
// non-secret values a Resource realizes. A nil *OutputContract on a Contract
// means the type publishes no outputs. Providers never define this contract;
// they are mapped onto it by private implementation bindings.
type OutputContract struct {
	fields []OutputField
}

// NewOutputContract validates and normalizes one output contract. Field names
// must be non-empty, unique, and outside the reserved vocabulary; types must
// be known. The returned contract stores fields in deterministic name order.
func NewOutputContract(fields []OutputField) (OutputContract, error) {
	if len(fields) == 0 {
		return OutputContract{}, fmt.Errorf("output contract must declare at least one field")
	}
	normalized := make([]OutputField, len(fields))
	copy(normalized, fields)
	for _, field := range normalized {
		name := strings.TrimSpace(field.Name)
		if name == "" || name != field.Name {
			return OutputContract{}, fmt.Errorf("output field name %q is empty or not canonical", field.Name)
		}
		if _, exists := reservedOutputNames[name]; exists {
			return OutputContract{}, fmt.Errorf("output field name %q is reserved", name)
		}
		if !field.JSONType.valid() {
			return OutputContract{}, fmt.Errorf("output field %q declares unknown JSON type %q", name, field.JSONType)
		}
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].Name < normalized[j].Name })
	for i := 1; i < len(normalized); i++ {
		if normalized[i].Name == normalized[i-1].Name {
			return OutputContract{}, fmt.Errorf("output field %q is duplicated", normalized[i].Name)
		}
	}
	return OutputContract{fields: append([]OutputField(nil), normalized...)}, nil
}

// Fields returns the declared fields in deterministic name order.
func (c OutputContract) Fields() []OutputField {
	return append([]OutputField(nil), c.fields...)
}

// RequiresOutputs reports whether at least one declared field is a required
// reconciliation postcondition.
func (c OutputContract) RequiresOutputs() bool {
	for _, field := range c.fields {
		if field.RequiredWhenReady {
			return true
		}
	}
	return false
}

// Validate checks provider-supplied candidate values against the declared
// contract: every key must be declared, every value must match its exact
// declared scalar type without coercion, and every required field must be
// present. It is the boundary that keeps arbitrary provider maps from ever
// becoming public output values.
func (c OutputContract) Validate(values map[string]any) error {
	declared := make(map[string]OutputField, len(c.fields))
	for _, field := range c.fields {
		declared[field.Name] = field
		value, present := values[field.Name]
		if !present {
			continue
		}
		if err := validateFieldType(field, value); err != nil {
			return err
		}
	}
	for name := range values {
		if _, ok := declared[name]; !ok {
			return fmt.Errorf("output field %q is not declared by the resource type contract", name)
		}
	}
	for _, field := range c.fields {
		if !field.RequiredWhenReady {
			continue
		}
		if _, present := values[field.Name]; !present {
			return fmt.Errorf("required output field %q is missing", field.Name)
		}
	}
	return nil
}

func validateFieldType(field OutputField, value any) error {
	switch field.JSONType {
	case OutputTypeString:
		if _, ok := value.(string); !ok {
			return fmt.Errorf("output field %q must be a string", field.Name)
		}
		return nil
	case OutputTypeBoolean:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("output field %q must be a boolean", field.Name)
		}
		return nil
	case OutputTypeInteger:
		if _, ok := domain.IntegralValue(value); !ok {
			return fmt.Errorf("output field %q must carry an integral number", field.Name)
		}
		return nil
	case OutputTypeNumber:
		switch value.(type) {
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
			return nil
		default:
			return fmt.Errorf("output field %q must be a number", field.Name)
		}
	default:
		return fmt.Errorf("output field %q declares unknown JSON type %q", field.Name, field.JSONType)
	}
}

// Contract is the consumer-facing developer-contract behavior shared by the
// application and concrete ResourceType implementations. Concrete packages
// satisfy it structurally; neither side imports the other. It exposes only
// developer-intent concepts: identity, display metadata, contract
// capabilities, the domain lifecycle type, spec validation, update-transition
// validation, the discovery schema document, and the optional output
// contract. Provisioner selection, platform state, and implementation
// bindings have no representation here.
type Contract interface {
	Ref() domain.ResourceTypeRef
	DisplayName() string
	Description() string
	Capabilities() []domain.Capability
	Domain() domain.ResourceType
	ValidateSpec(domain.ResourceSpec) error
	// ValidateUpdate reports whether transitioning a Resource from oldSpec to
	// newSpec is legal under the developer contract. A schema-valid spec can
	// still be an illegal transition; contracts own that distinction. The
	// application enforces it synchronously during update admission, before
	// any durable effect.
	ValidateUpdate(oldSpec, newSpec domain.ResourceSpec) error
	SpecSchema() json.RawMessage
	// OutputContract returns the declared non-secret output contract, or nil
	// when the ResourceType publishes no outputs.
	OutputContract() *OutputContract
	// ReferenceContract returns the declared provider-neutral reference
	// contract, or nil when the type participates in no relationships as a
	// source. Relationship semantics belong to Liftr; this contract only
	// declares the slots, exact target types, and cardinality bounds.
	ReferenceContract() *ReferenceContract
}

// The catalog port ("what developer contracts exist?") stays consumer-owned
// by the application package. Its Get and List methods return this neutral
// Contract type, so concrete registries satisfy the application port
// structurally without importing application.
