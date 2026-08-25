// SPDX-License-Identifier: Apache-2.0

// Package policy implements Liftr's immutable M18 admission-policy overlay.
// It is deliberately data-only: no repositories, identity, clocks, remote
// calls, expressions, or provisioner concepts exist here.
package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
)

const (
	APIVersion              = "liftr.dev/admission-policy/v1"
	MaxFileBytes            = 1 << 20
	MaxRules                = 1024
	MaxSelectorBytes        = 256
	MaxQuotaLimit    uint64 = 1_000_000_000
)

var ruleIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

type ruleKind string

const (
	kindCapabilityDeny ruleKind = "capability-deny"
	kindResourceQuota  ruleKind = "resource-count-quota"
)

type compiledRule struct {
	ID           string
	Kind         ruleKind
	Owner        *domain.OwnerRef
	ResourceType *domain.ResourceTypeRef
	Capabilities []application.AdmissionMutation
	Limit        uint64
}

type Policy struct {
	revision application.PolicyRevision
	rules    []compiledRule
}

var _ application.AdmissionPolicy = (*Policy)(nil)

func LoadFile(ctx context.Context, path string, catalog application.ResourceTypeCatalog) (*Policy, error) {
	if catalog == nil {
		return nil, errors.New("policy ResourceType catalog is required")
	}
	contracts, err := catalog.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list ResourceTypes for policy validation: %w", err)
	}
	registered := make([]domain.ResourceTypeRef, 0, len(contracts))
	for _, contract := range contracts {
		registered = append(registered, contract.Ref())
	}
	if strings.TrimSpace(path) == "" {
		return Parse([]byte(`{"apiVersion":"`+APIVersion+`","rules":[]}`), registered)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read policy file: %w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, MaxFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read policy file: %w", err)
	}
	if len(raw) > MaxFileBytes {
		return nil, fmt.Errorf("policy file exceeds %d bytes", MaxFileBytes)
	}
	result, err := Parse(raw, registered)
	if err != nil {
		return nil, fmt.Errorf("parse policy file %s: %w", path, err)
	}
	return result, nil
}

// Parse strictly validates, normalizes, and compiles one M18 policy document.
// It is exported for composition and contract tests; policy remains immutable.
func Parse(raw []byte, registered []domain.ResourceTypeRef) (*Policy, error) {
	if len(raw) == 0 {
		return nil, errors.New("policy document is empty")
	}
	if len(raw) > MaxFileBytes {
		return nil, fmt.Errorf("policy document exceeds %d bytes", MaxFileBytes)
	}
	if err := validateUniqueJSON(raw); err != nil {
		return nil, err
	}
	root, err := decodeObject(raw, "apiVersion", "rules")
	if err != nil {
		return nil, err
	}
	version, err := requiredString(root, "apiVersion")
	if err != nil {
		return nil, err
	}
	if version != APIVersion {
		return nil, fmt.Errorf("apiVersion must be %q", APIVersion)
	}
	rulesRaw, ok := root["rules"]
	if !ok {
		return nil, errors.New("rules is required")
	}
	var rawRules []json.RawMessage
	if err := json.Unmarshal(rulesRaw, &rawRules); err != nil {
		return nil, errors.New("rules must be an array")
	}
	if rawRules == nil {
		return nil, errors.New("rules must be an array")
	}
	if len(rawRules) > MaxRules {
		return nil, fmt.Errorf("policy declares more than %d rules", MaxRules)
	}
	registeredSet := make(map[domain.ResourceTypeRef]struct{}, len(registered))
	for _, ref := range registered {
		registeredSet[ref] = struct{}{}
	}
	rules := make([]compiledRule, 0, len(rawRules))
	ids := make(map[string]struct{}, len(rawRules))
	semantics := make(map[string]string)
	for index, rawRule := range rawRules {
		rule, err := parseRule(rawRule, registeredSet)
		if err != nil {
			return nil, fmt.Errorf("rule %d: %w", index, err)
		}
		if _, exists := ids[rule.ID]; exists {
			return nil, fmt.Errorf("rule ID %q is duplicated", rule.ID)
		}
		ids[rule.ID] = struct{}{}
		for _, key := range semanticKeys(rule) {
			if previous, exists := semantics[key]; exists {
				return nil, fmt.Errorf("rules %q and %q duplicate the same semantic restriction", previous, rule.ID)
			}
			semantics[key] = rule.ID
		}
		rules = append(rules, rule)
	}
	sort.Slice(rules, func(i, j int) bool {
		if rules[i].Kind != rules[j].Kind {
			return rules[i].Kind < rules[j].Kind
		}
		return rules[i].ID < rules[j].ID
	})
	canonical, err := canonicalPolicy(rules)
	if err != nil {
		return nil, err
	}
	return &Policy{revision: application.NewPolicyRevision(canonical), rules: rules}, nil
}

func parseRule(raw json.RawMessage, registered map[domain.ResourceTypeRef]struct{}) (compiledRule, error) {
	object, err := decodeObject(raw, "id", "kind", "owner", "resourceType", "capabilities", "limit")
	if err != nil {
		return compiledRule{}, err
	}
	id, err := requiredString(object, "id")
	if err != nil {
		return compiledRule{}, err
	}
	if !ruleIDPattern.MatchString(id) {
		return compiledRule{}, fmt.Errorf("rule ID %q must match %s", id, ruleIDPattern)
	}
	kindText, err := requiredString(object, "kind")
	if err != nil {
		return compiledRule{}, err
	}
	rule := compiledRule{ID: id, Kind: ruleKind(kindText)}
	if rawOwner, present := object["owner"]; present {
		owner, err := parseOwner(rawOwner)
		if err != nil {
			return compiledRule{}, err
		}
		rule.Owner = &owner
	}
	if rawType, present := object["resourceType"]; present {
		ref, err := parseResourceType(rawType)
		if err != nil {
			return compiledRule{}, err
		}
		if _, found := registered[ref]; !found {
			return compiledRule{}, fmt.Errorf("ResourceType %s/%s is not registered", ref.Name, ref.Version)
		}
		rule.ResourceType = &ref
	}
	switch rule.Kind {
	case kindCapabilityDeny:
		if rule.ResourceType == nil {
			return compiledRule{}, errors.New("capability-deny requires an exact resourceType")
		}
		if _, present := object["limit"]; present {
			return compiledRule{}, errors.New("capability-deny does not accept limit")
		}
		rawCaps, present := object["capabilities"]
		if !present {
			return compiledRule{}, errors.New("capabilities is required")
		}
		var caps []string
		if err := json.Unmarshal(rawCaps, &caps); err != nil || len(caps) == 0 {
			return compiledRule{}, errors.New("capabilities must be a non-empty array")
		}
		seen := map[application.AdmissionMutation]struct{}{}
		for _, capText := range caps {
			capability := application.AdmissionMutation(capText)
			if capability != application.AdmissionCreate && capability != application.AdmissionUpdate {
				return compiledRule{}, fmt.Errorf("unsupported denied capability %q", capText)
			}
			if _, duplicate := seen[capability]; duplicate {
				return compiledRule{}, fmt.Errorf("capability %q is duplicated", capText)
			}
			seen[capability] = struct{}{}
			rule.Capabilities = append(rule.Capabilities, capability)
		}
		sort.Slice(rule.Capabilities, func(i, j int) bool { return rule.Capabilities[i] < rule.Capabilities[j] })
	case kindResourceQuota:
		if _, present := object["capabilities"]; present {
			return compiledRule{}, errors.New("resource-count-quota does not accept capabilities")
		}
		rawLimit, present := object["limit"]
		if !present {
			return compiledRule{}, errors.New("limit is required")
		}
		limit, err := parseLimit(rawLimit)
		if err != nil {
			return compiledRule{}, err
		}
		rule.Limit = limit
	default:
		return compiledRule{}, fmt.Errorf("unsupported rule kind %q", kindText)
	}
	return rule, nil
}

func parseOwner(raw json.RawMessage) (domain.OwnerRef, error) {
	object, err := decodeObject(raw, "kind", "id")
	if err != nil {
		return domain.OwnerRef{}, fmt.Errorf("owner: %w", err)
	}
	kind, err := canonicalSelectorString(object, "kind")
	if err != nil {
		return domain.OwnerRef{}, fmt.Errorf("owner: %w", err)
	}
	id, err := canonicalSelectorString(object, "id")
	if err != nil {
		return domain.OwnerRef{}, fmt.Errorf("owner: %w", err)
	}
	return domain.OwnerRef{Kind: kind, ID: id}, nil
}

func parseResourceType(raw json.RawMessage) (domain.ResourceTypeRef, error) {
	object, err := decodeObject(raw, "name", "version")
	if err != nil {
		return domain.ResourceTypeRef{}, fmt.Errorf("resourceType: %w", err)
	}
	name, err := canonicalSelectorString(object, "name")
	if err != nil {
		return domain.ResourceTypeRef{}, fmt.Errorf("resourceType: %w", err)
	}
	version, err := canonicalSelectorString(object, "version")
	if err != nil {
		return domain.ResourceTypeRef{}, fmt.Errorf("resourceType: %w", err)
	}
	return domain.ResourceTypeRef{Name: name, Version: version}, nil
}

func parseLimit(raw json.RawMessage) (uint64, error) {
	text := string(bytes.TrimSpace(raw))
	if text == "" || strings.ContainsAny(text, ".eE+-") {
		return 0, errors.New("limit must be a positive bounded integer")
	}
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil || value == 0 || value > MaxQuotaLimit {
		return 0, fmt.Errorf("limit must be between 1 and %d", MaxQuotaLimit)
	}
	return value, nil
}

func semanticKeys(rule compiledRule) []string {
	selector := selectorKey(rule.Owner, rule.ResourceType)
	if rule.Kind == kindResourceQuota {
		return []string{string(rule.Kind) + "\x00" + selector}
	}
	keys := make([]string, 0, len(rule.Capabilities))
	for _, capability := range rule.Capabilities {
		keys = append(keys, string(rule.Kind)+"\x00"+selector+"\x00"+string(capability))
	}
	return keys
}

func selectorKey(owner *domain.OwnerRef, ref *domain.ResourceTypeRef) string {
	fields := []string{"owner"}
	if owner != nil {
		fields = append(fields, owner.Kind, owner.ID)
	}
	fields = append(fields, "type")
	if ref != nil {
		fields = append(fields, ref.Name, ref.Version)
	}
	var result strings.Builder
	for _, field := range fields {
		fmt.Fprintf(&result, "%d:%s", len(field), field)
	}
	return result.String()
}

type canonicalDocument struct {
	APIVersion string          `json:"apiVersion"`
	Rules      []canonicalRule `json:"rules"`
}

type canonicalRule struct {
	ID           string                          `json:"id"`
	Kind         ruleKind                        `json:"kind"`
	Owner        *domain.OwnerRef                `json:"owner,omitempty"`
	ResourceType *domain.ResourceTypeRef         `json:"resourceType,omitempty"`
	Capabilities []application.AdmissionMutation `json:"capabilities,omitempty"`
	Limit        uint64                          `json:"limit,omitempty"`
}

func canonicalPolicy(rules []compiledRule) ([]byte, error) {
	document := canonicalDocument{APIVersion: APIVersion, Rules: make([]canonicalRule, 0, len(rules))}
	for _, rule := range rules {
		document.Rules = append(document.Rules, canonicalRule{
			ID: rule.ID, Kind: rule.Kind, Owner: rule.Owner, ResourceType: rule.ResourceType,
			Capabilities: rule.Capabilities, Limit: rule.Limit,
		})
	}
	return json.Marshal(document)
}

func (p *Policy) Revision() application.PolicyRevision { return p.revision }

func (p *Policy) Plan(intent application.AdmissionIntent) (application.AdmissionPlan, error) {
	if p == nil || p.revision == "" || (intent.Mutation != application.AdmissionCreate && intent.Mutation != application.AdmissionUpdate) ||
		intent.Owner.Kind == "" || intent.Owner.ID == "" || intent.ResourceType.Name == "" || intent.ResourceType.Version == "" {
		return application.AdmissionPlan{}, fmt.Errorf("%w: invalid admission intent or policy", application.ErrPolicyEvaluation)
	}
	plan := application.AdmissionPlan{Intent: intent, Revision: p.revision}
	best := map[application.QuotaDimension]application.ResourceCountConstraint{}
	for _, rule := range p.rules {
		if !matches(rule, intent) {
			continue
		}
		switch rule.Kind {
		case kindCapabilityDeny:
			for _, capability := range rule.Capabilities {
				if capability == intent.Mutation && plan.CapabilityDenial == nil {
					plan.CapabilityDenial = &application.PolicyDenial{Kind: application.PolicyDenialCapabilityDisabled, RuleID: rule.ID}
				}
			}
		case kindResourceQuota:
			if intent.Mutation != application.AdmissionCreate {
				continue
			}
			dimension := application.QuotaOwnerResources
			if rule.ResourceType != nil {
				dimension = application.QuotaOwnerTypeResources
			}
			candidate := application.ResourceCountConstraint{RuleID: rule.ID, Dimension: dimension, Limit: rule.Limit}
			current, present := best[dimension]
			if !present || candidate.Limit < current.Limit || candidate.Limit == current.Limit && candidate.RuleID < current.RuleID {
				best[dimension] = candidate
			}
		}
	}
	for _, dimension := range []application.QuotaDimension{application.QuotaOwnerResources, application.QuotaOwnerTypeResources} {
		if constraint, present := best[dimension]; present {
			plan.CountConstraints = append(plan.CountConstraints, constraint)
		}
	}
	return plan, nil
}

func matches(rule compiledRule, intent application.AdmissionIntent) bool {
	if rule.Owner != nil && *rule.Owner != intent.Owner {
		return false
	}
	if rule.ResourceType != nil && *rule.ResourceType != intent.ResourceType {
		return false
	}
	return true
}

func (p *Policy) Decide(plan application.AdmissionPlan, facts application.ResourceCountFacts) (application.AdmissionDecision, error) {
	if p == nil || plan.Revision != p.revision || plan.Intent.Mutation != application.AdmissionCreate && plan.Intent.Mutation != application.AdmissionUpdate {
		return application.AdmissionDecision{}, fmt.Errorf("%w: admission plan does not belong to this policy", application.ErrPolicyEvaluation)
	}
	if plan.CapabilityDenial != nil {
		denial := *plan.CapabilityDenial
		return application.AdmissionDecision{Outcome: application.AdmissionDenied, Revision: p.revision, Denial: &denial}, nil
	}
	if len(plan.CountConstraints) == 0 {
		return application.AdmissionDecision{Outcome: application.AdmissionAllowed, Revision: p.revision}, nil
	}
	if !facts.Available || facts.Owner != plan.Intent.Owner || facts.ResourceType != plan.Intent.ResourceType {
		return application.AdmissionDecision{}, fmt.Errorf("%w: resource count facts are missing or dimensionally inconsistent", application.ErrPolicyEvaluation)
	}
	seen := map[application.QuotaDimension]struct{}{}
	for _, constraint := range plan.CountConstraints {
		if constraint.Limit == 0 || constraint.RuleID == "" {
			return application.AdmissionDecision{}, fmt.Errorf("%w: malformed count constraint", application.ErrPolicyEvaluation)
		}
		if _, duplicate := seen[constraint.Dimension]; duplicate {
			return application.AdmissionDecision{}, fmt.Errorf("%w: duplicate count dimension", application.ErrPolicyEvaluation)
		}
		seen[constraint.Dimension] = struct{}{}
		var current uint64
		switch constraint.Dimension {
		case application.QuotaOwnerResources:
			current = facts.OwnerNonDeleted
		case application.QuotaOwnerTypeResources:
			current = facts.TypeNonDeleted
		default:
			return application.AdmissionDecision{}, fmt.Errorf("%w: unsupported quota dimension", application.ErrPolicyEvaluation)
		}
		if current >= constraint.Limit {
			denial := application.PolicyDenial{
				Kind: application.PolicyDenialQuotaExceeded, RuleID: constraint.RuleID,
				Measure: "resource_count", Current: current, Requested: 1, Limit: constraint.Limit,
			}
			return application.AdmissionDecision{Outcome: application.AdmissionDenied, Revision: p.revision, Denial: &denial}, nil
		}
	}
	return application.AdmissionDecision{Outcome: application.AdmissionAllowed, Revision: p.revision}, nil
}

func decodeObject(raw []byte, allowed ...string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, errors.New("value must be a JSON object")
	}
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allowedSet[name] = struct{}{}
	}
	for name := range object {
		if _, ok := allowedSet[name]; !ok {
			return nil, fmt.Errorf("unknown field %q", name)
		}
	}
	return object, nil
}

func requiredString(object map[string]json.RawMessage, name string) (string, error) {
	raw, present := object[name]
	if !present {
		return "", fmt.Errorf("%s is required", name)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || value == "" {
		return "", fmt.Errorf("%s must be a non-empty string", name)
	}
	return value, nil
}

func canonicalSelectorString(object map[string]json.RawMessage, name string) (string, error) {
	value, err := requiredString(object, name)
	if err != nil {
		return "", err
	}
	if value != strings.TrimSpace(value) || len(value) > MaxSelectorBytes {
		return "", fmt.Errorf("%s must be canonical and at most %d bytes", name, MaxSelectorBytes)
	}
	return value, nil
}

func validateUniqueJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := scanJSONValue(decoder, 0); err != nil {
		return fmt.Errorf("invalid policy JSON: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("invalid policy JSON: trailing JSON value")
		}
		return fmt.Errorf("invalid policy JSON: %w", err)
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("nesting exceeds 32 levels")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object member name is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate object member %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("unterminated object")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("unterminated array")
		}
	default:
		return fmt.Errorf("unexpected delimiter %q", delimiter)
	}
	return nil
}
