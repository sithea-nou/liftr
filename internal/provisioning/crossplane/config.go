// SPDX-License-Identifier: Apache-2.0

package crossplane

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube"
)

// GVK identifies the platform-owned composite resource kind one binding
// targets. It is private adapter configuration and never leaves the adapter.
type GVK struct {
	Group   string
	Version string
	Kind    string
}

// ConditionRule declares one structural readiness condition of the target
// XR schema. Required rules must be present, report True, and carry a fresh
// observedGeneration before any Ready fact may be reported.
type ConditionRule struct {
	Type     string
	Required bool
}

// DefaultConditionRules are the standard Crossplane XR conditions. They are
// used when a binding declares no explicit rule set.
func DefaultConditionRules() []ConditionRule {
	return []ConditionRule{{Type: "Ready", Required: true}, {Type: "Synced", Required: true}}
}

// OutputMapping registers one private, immutable output-mapping
// implementation reading exactly one status path. CompatibleSourceMappingRef
// optionally declares the one exact persisted envelope identity this mapping
// can safely repair during M13 output recovery.
type OutputMapping struct {
	Ref                        string
	StatusPath                 []string
	CompatibleSourceMappingRef string
}

// Input mirrors the neutral execution intent handed to encoders.
type Input struct {
	OperationID      domain.OperationID
	AttemptNumber    uint64
	ResourceID       domain.ResourceID
	ResourceType     domain.ResourceTypeRef
	Capability       domain.Capability
	Spec             domain.ResourceSpec
	TargetGeneration uint64
}

// InputEncoder translates one execution into the validated XR spec document.
// The encoder owns the developer-intent mapping only; the adapter owns the
// Kubernetes envelope and all correlation metadata.
type InputEncoder func(Input) ([]byte, error)

// Binding is one private ResourceType implementation registration. Like the
// Pulumi program registration, create/update/delete are capabilities inside
// this single entry; configuring two bindings for one ResourceTypeRef is
// rejected.
type Binding struct {
	ResourceType            domain.ResourceTypeRef
	Capabilities            []domain.Capability
	Target                  GVK
	Plural                  string
	Namespace               string
	NamingVersion           string
	EncodeInput             InputEncoder
	Readiness               []ConditionRule
	TerminalSyncedReasons   []string
	OutputMappings          []OutputMapping
	CurrentOutputMappingRef string
}

// Config is the adapter's private startup configuration.
type Config struct {
	Identity       string
	KubeconfigPath string
	ContextName    string
	RequestTimeout time.Duration
	Bindings       []Binding
}

var (
	pluralPattern    = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
	namespacePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
)

const maxNamespaceLength = 63

type resolvedBinding struct {
	binding         Binding
	gvr             kube.GVR
	kind            string
	namespace       string
	readiness       []ConditionRule
	terminalReasons map[string]struct{}
	outputs         map[string]OutputMapping
}

func (b *resolvedBinding) supportsCapability(capability domain.Capability) bool {
	for _, supported := range b.binding.Capabilities {
		if supported == capability {
			return true
		}
	}
	return false
}

// OutputMappingIsRegistered reports whether the binding publishes outputs.
func (b *resolvedBinding) OutputMappingIsRegistered() bool {
	return len(b.outputs) > 0
}

func (c Config) resolve() (string, map[domain.ResourceTypeRef]*resolvedBinding, error) {
	if strings.TrimSpace(c.Identity) == "" {
		return "", nil, fmt.Errorf("configuration identity is required")
	}
	if len(c.Bindings) == 0 {
		return "", nil, fmt.Errorf("at least one XR binding is required")
	}
	digest := PlatformDigest(c.Identity)
	bindings := make(map[domain.ResourceTypeRef]*resolvedBinding)
	for _, binding := range c.Bindings {
		resolved, err := resolveBinding(binding)
		if err != nil {
			return "", nil, err
		}
		if _, exists := bindings[binding.ResourceType]; exists {
			return "", nil, fmt.Errorf("duplicate XR binding for resource type %s/%s",
				binding.ResourceType.Name, binding.ResourceType.Version)
		}
		bindings[binding.ResourceType] = resolved
	}
	return digest, bindings, nil
}

func resolveBinding(binding Binding) (*resolvedBinding, error) {
	ref := binding.ResourceType
	if strings.TrimSpace(ref.Name) == "" || strings.TrimSpace(ref.Version) == "" {
		return nil, fmt.Errorf("XR binding resource type reference is required")
	}
	if len(binding.Capabilities) == 0 {
		return nil, fmt.Errorf("XR binding %s/%s capabilities are required", ref.Name, ref.Version)
	}
	seen := make(map[domain.Capability]struct{}, len(binding.Capabilities))
	for _, capability := range binding.Capabilities {
		if capability != domain.CapabilityCreate && capability != domain.CapabilityUpdate && capability != domain.CapabilityDelete {
			return nil, fmt.Errorf("unsupported XR binding capability")
		}
		if _, duplicate := seen[capability]; duplicate {
			return nil, fmt.Errorf("XR binding capability is duplicated")
		}
		seen[capability] = struct{}{}
	}
	if strings.TrimSpace(binding.Target.Group) == "" || strings.TrimSpace(binding.Target.Version) == "" ||
		strings.TrimSpace(binding.Target.Kind) == "" {
		return nil, fmt.Errorf("XR binding %s/%s must declare a complete group/version/kind", ref.Name, ref.Version)
	}
	if !pluralPattern.MatchString(binding.Plural) {
		return nil, fmt.Errorf("XR binding %s/%s plural is invalid", ref.Name, ref.Version)
	}
	if !namespacePattern.MatchString(binding.Namespace) || len(binding.Namespace) > maxNamespaceLength {
		return nil, fmt.Errorf("XR binding %s/%s namespace is not a valid DNS label", ref.Name, ref.Version)
	}
	if binding.NamingVersion != NamingVersionV1 {
		return nil, fmt.Errorf("XR binding %s/%s naming version %q is unsupported", ref.Name, ref.Version, binding.NamingVersion)
	}
	if binding.EncodeInput == nil {
		return nil, fmt.Errorf("XR binding %s/%s input encoder is required", ref.Name, ref.Version)
	}
	rules := binding.Readiness
	if rules == nil {
		rules = DefaultConditionRules()
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("XR binding %s/%s must require at least one readiness condition", ref.Name, ref.Version)
	}
	requiredCount := 0
	conditionNames := make(map[string]struct{}, len(rules))
	for _, rule := range rules {
		if strings.TrimSpace(rule.Type) == "" {
			return nil, fmt.Errorf("XR binding %s/%s readiness condition type is required", ref.Name, ref.Version)
		}
		if _, duplicate := conditionNames[rule.Type]; duplicate {
			return nil, fmt.Errorf("XR binding %s/%s readiness condition type is duplicated", ref.Name, ref.Version)
		}
		conditionNames[rule.Type] = struct{}{}
		if rule.Required {
			requiredCount++
		}
	}
	if requiredCount == 0 {
		return nil, fmt.Errorf("XR binding %s/%s readiness rule set requires at least one required condition", ref.Name, ref.Version)
	}
	terminalReasons := make(map[string]struct{}, len(binding.TerminalSyncedReasons))
	for _, reason := range binding.TerminalSyncedReasons {
		if strings.TrimSpace(reason) == "" {
			return nil, fmt.Errorf("XR binding terminal reconciliation reasons must not be empty")
		}
		terminalReasons[reason] = struct{}{}
	}
	resolved := &resolvedBinding{
		binding:         binding,
		gvr:             kube.GVR{Group: binding.Target.Group, Version: binding.Target.Version, Resource: binding.Plural},
		kind:            binding.Target.Kind,
		namespace:       binding.Namespace,
		readiness:       rules,
		terminalReasons: terminalReasons,
		outputs:         map[string]OutputMapping{},
	}
	if err := validateOutputMappings(resolved); err != nil {
		return nil, err
	}
	return resolved, nil
}

func validateOutputMappings(resolved *resolvedBinding) error {
	binding := resolved.binding
	current := strings.TrimSpace(binding.CurrentOutputMappingRef)
	if len(binding.OutputMappings) == 0 {
		if current != "" {
			return fmt.Errorf("current XR output mapping is not registered")
		}
		return nil
	}
	if current == "" {
		return fmt.Errorf("current XR output mapping identity is required")
	}
	for _, mapping := range binding.OutputMappings {
		if strings.TrimSpace(mapping.Ref) == "" || len(mapping.StatusPath) == 0 {
			return fmt.Errorf("XR output mapping identity and status path are required")
		}
		if _, exists := resolved.outputs[mapping.Ref]; exists {
			return fmt.Errorf("XR output mapping identity is duplicated")
		}
		resolved.outputs[mapping.Ref] = mapping
		if mapping.CompatibleSourceMappingRef == "" {
			continue
		}
		if mapping.CompatibleSourceMappingRef == mapping.Ref {
			return fmt.Errorf("compatible XR output source mapping identity is invalid")
		}
	}
	if _, exists := resolved.outputs[current]; !exists {
		return fmt.Errorf("current XR output mapping is not registered")
	}
	return nil
}

func sortCapabilities(capabilities []provisioning.ProvisionerCapability) {
	sort.Slice(capabilities, func(i, j int) bool {
		left := capabilities[i].ResourceType.Name + "\x00" + capabilities[i].ResourceType.Version + "\x00" + string(capabilities[i].Capability)
		right := capabilities[j].ResourceType.Name + "\x00" + capabilities[j].ResourceType.Version + "\x00" + string(capabilities[j].Capability)
		return left < right
	})
}
