// SPDX-License-Identifier: Apache-2.0

// Package resourcetypes defines the developer-facing ResourceType contract:
// identity, display metadata, contract capabilities, a self-contained JSON
// Schema (draft 2020-12) document for the accepted ResourceSpec, and spec
// validation. It also hosts the deterministic in-memory registry that serves
// discovery and application admission.
//
// The package is the only place allowed to depend on a JSON Schema
// implementation. The domain stays schema-blind, and the application consumes
// contracts only through its own consumer-owned port.
package resourcetypes

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
)

// SchemaDialect pins the only JSON Schema dialect Liftr accepts for
// ResourceSpec schemas.
const SchemaDialect = "https://json-schema.org/draft/2020-12/schema"

// errResolutionDisabled backs every refused external schema load. Registered
// schemas are self-contained; validation never performs network I/O. Local
// composition through $defs and "#/$defs/..." references remains available.
var errResolutionDisabled = errors.New("schema resolution outside the registered document is disabled")

// blockedLoader refuses every schema load that was not registered with the
// compiler. Embedded metaschemas still resolve, so the pinned dialect
// compiles without network access.
type blockedLoader struct{}

func (blockedLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("%w: %s", errResolutionDisabled, url)
}

// SpecSchema is one compiled, self-contained ResourceSpec schema document.
// Document returns the bytes exactly as registered; Digest is the SHA-256 of
// those bytes and exists for registration integrity checks and tests.
type SpecSchema struct {
	id       string
	document json.RawMessage
	digest   string
	compiled *jsonschema.Schema
}

// CompileSpecSchema validates and compiles one raw JSON Schema 2020-12
// document without any network access.
func CompileSpecSchema(document []byte) (SpecSchema, error) {
	if len(bytes.TrimSpace(document)) == 0 {
		return SpecSchema{}, errors.New("spec schema document is empty")
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(document))
	if err != nil {
		return SpecSchema{}, fmt.Errorf("spec schema is not valid JSON: %w", err)
	}
	root, ok := doc.(map[string]any)
	if !ok {
		return SpecSchema{}, errors.New("spec schema must be a JSON object")
	}
	id, ok := root["$id"].(string)
	if !ok || strings.TrimSpace(id) == "" {
		return SpecSchema{}, errors.New(`spec schema must declare a non-empty "$id"`)
	}
	dialect, _ := root["$schema"].(string)
	if dialect != SchemaDialect {
		return SpecSchema{}, fmt.Errorf("spec schema $schema must be %q", SchemaDialect)
	}
	objectType, _ := root["type"].(string)
	if objectType != "object" {
		return SpecSchema{}, errors.New(`spec schema root must declare "type": "object"`)
	}

	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(blockedLoader{})
	if err := compiler.AddResource(id, doc); err != nil {
		return SpecSchema{}, fmt.Errorf("spec schema resource could not be registered: %w", err)
	}
	compiled, err := compiler.Compile(id)
	if err != nil {
		return SpecSchema{}, fmt.Errorf("spec schema does not compile: %w", err)
	}
	sum := sha256.Sum256(document)
	return SpecSchema{
		id:       id,
		document: append(json.RawMessage(nil), document...),
		digest:   hex.EncodeToString(sum[:]),
		compiled: compiled,
	}, nil
}

// ID returns the schema's declared $id.
func (s SpecSchema) ID() string { return s.id }

// Document returns the registered schema bytes verbatim.
func (s SpecSchema) Document() json.RawMessage {
	return append(json.RawMessage(nil), s.document...)
}

// Digest returns the hex-encoded SHA-256 of the registered schema bytes.
func (s SpecSchema) Digest() string { return s.digest }

// ViolationsFor evaluates spec values against the compiled schema and
// returns sanitized, uncapped violations in deterministic order. Evaluation
// performs no I/O and no mutation.
func (s SpecSchema) ViolationsFor(values map[string]any) []resourcecontract.Violation {
	violations := s.violationsFor(values)
	resourcecontract.SortViolations(violations)
	return violations
}

// violationsFor evaluates the compiled schema against spec values and returns
// sanitized violations without capping. Evaluation performs no I/O.
func (s SpecSchema) violationsFor(values map[string]any) []resourcecontract.Violation {
	err := s.compiled.Validate(values)
	if err == nil {
		return nil
	}
	verr, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []resourcecontract.Violation{{Keyword: "schema", Message: "spec could not be validated"}}
	}
	var out []resourcecontract.Violation
	collectViolations(verr, &out)
	return out
}

// structuralKinds wrap deeper results; their own message carries no client
// value, so only their causes become violations.
func structuralKind(err *jsonschema.ValidationError) bool {
	switch err.ErrorKind.(type) {
	case *kind.Schema, *kind.Group, *kind.Reference,
		*kind.AllOf, *kind.AnyOf, *kind.OneOf, *kind.Not:
		return true
	default:
		return false
	}
}

func collectViolations(err *jsonschema.ValidationError, out *[]resourcecontract.Violation) {
	if structuralKind(err) {
		for _, cause := range err.Causes {
			collectViolations(cause, out)
		}
		return
	}
	for _, violation := range expandViolation(err) {
		*out = append(*out, violation)
	}
	for _, cause := range err.Causes {
		collectViolations(cause, out)
	}
}

// expandViolation renders one library error as zero or more sanitized
// violations. Property names come from the structured error kinds, never from
// formatted library text, and submitted values are never echoed.
func expandViolation(verr *jsonschema.ValidationError) []resourcecontract.Violation {
	parent := jsonPointer(verr.InstanceLocation)
	switch k := verr.ErrorKind.(type) {
	case *kind.Required:
		missing := append([]string(nil), k.Missing...)
		sort.Strings(missing)
		out := make([]resourcecontract.Violation, 0, len(missing))
		for _, name := range missing {
			out = append(out, resourcecontract.Violation{
				Path:    parent,
				Keyword: "required",
				Message: fmt.Sprintf("property %q is required", name),
			})
		}
		return out
	case *kind.AdditionalProperties:
		extra := append([]string(nil), k.Properties...)
		sort.Strings(extra)
		out := make([]resourcecontract.Violation, 0, len(extra))
		for _, name := range extra {
			out = append(out, resourcecontract.Violation{
				Path:    parent + "/" + pointerEscape(name),
				Keyword: "additionalProperties",
				Message: fmt.Sprintf("property %q is not permitted by this resource type", name),
			})
		}
		return out
	default:
		return []resourcecontract.Violation{{
			Path:    parent,
			Keyword: violationKeyword(verr),
			Message: sanitizedMessage(verr),
		}}
	}
}

func violationKeyword(verr *jsonschema.ValidationError) string {
	path := verr.ErrorKind.KeywordPath()
	if len(path) == 0 {
		return "schema"
	}
	return path[0]
}

func sanitizedMessage(verr *jsonschema.ValidationError) string {
	keyword := violationKeyword(verr)
	switch k := verr.ErrorKind.(type) {
	case *kind.Type:
		want := strings.Join(k.Want, " or ")
		if len(verr.InstanceLocation) == 0 {
			return "spec must be an object"
		}
		return fmt.Sprintf("value must be of type %s", want)
	case *kind.Minimum:
		return fmt.Sprintf("value must be greater than or equal to %s", ratString(k.Want))
	case *kind.Maximum:
		return fmt.Sprintf("value must be less than or equal to %s", ratString(k.Want))
	case *kind.ExclusiveMinimum:
		return fmt.Sprintf("value must be strictly greater than %s", ratString(k.Want))
	case *kind.ExclusiveMaximum:
		return fmt.Sprintf("value must be strictly less than %s", ratString(k.Want))
	case *kind.MinLength:
		return fmt.Sprintf("string length must be at least %d", k.Want)
	case *kind.MaxLength:
		return fmt.Sprintf("string length must be at most %d", k.Want)
	case *kind.Pattern:
		return "value does not match the required pattern"
	case *kind.Enum:
		return "value is not one of the allowed values"
	case *kind.Const:
		return "value does not match the required constant"
	case *kind.MinItems:
		return fmt.Sprintf("list length must be at least %d", k.Want)
	case *kind.MaxItems:
		return fmt.Sprintf("list length must be at most %d", k.Want)
	case *kind.UniqueItems:
		return "list items must be unique"
	case *kind.FalseSchema:
		return "value is not permitted here"
	default:
		return fmt.Sprintf("value violates constraint %q", keyword)
	}
}

func ratString(value *big.Rat) string {
	f, _ := value.Float64()
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// jsonPointer renders instance location tokens as an RFC 6901 JSON Pointer;
// the empty string denotes the document root.
func jsonPointer(tokens []string) string {
	var builder strings.Builder
	for _, token := range tokens {
		builder.WriteByte('/')
		builder.WriteString(pointerEscape(token))
	}
	return builder.String()
}

func pointerEscape(token string) string {
	token = strings.ReplaceAll(token, "~", "~0")
	return strings.ReplaceAll(token, "/", "~1")
}

// SchemaID derives the stable URN identifier for a ResourceSpec schema. The
// URN identifies the schema document; Liftr never fetches it.
func SchemaID(ref domain.ResourceTypeRef) string {
	return fmt.Sprintf("urn:liftr:resource-type:%s:%s:spec", ref.Name, ref.Version)
}
