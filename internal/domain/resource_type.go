// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"fmt"
	"strings"
)

// Capability names a developer-facing action supported by a ResourceType.
// Lifecycle and operational actions share this extensible type.
type Capability string

const (
	CapabilityCreate  Capability = "create"
	CapabilityUpdate  Capability = "update"
	CapabilityDelete  Capability = "delete"
	CapabilityObserve Capability = "observe"
)

func (c Capability) validate() error {
	name := string(c)
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("capability is required")
	}
	if name != strings.TrimSpace(name) {
		return fmt.Errorf("capability %q cannot contain surrounding whitespace", name)
	}
	return nil
}

// ResourceType describes a versioned kind of Resource and its supported actions.
type ResourceType struct {
	ref          ResourceTypeRef
	description  string
	capabilities []Capability
}

func NewResourceType(ref ResourceTypeRef, description string, capabilities []Capability) (ResourceType, error) {
	if err := ref.validate(); err != nil {
		return ResourceType{}, err
	}
	if strings.TrimSpace(description) == "" {
		return ResourceType{}, fmt.Errorf("resource type description is required")
	}
	if len(capabilities) == 0 {
		return ResourceType{}, fmt.Errorf("resource type must define at least one capability")
	}

	seen := make(map[Capability]struct{}, len(capabilities))
	cloned := make([]Capability, len(capabilities))
	for i, capability := range capabilities {
		if err := capability.validate(); err != nil {
			return ResourceType{}, err
		}
		if _, exists := seen[capability]; exists {
			return ResourceType{}, fmt.Errorf("capability %q is duplicated", capability)
		}
		seen[capability] = struct{}{}
		cloned[i] = capability
	}

	return ResourceType{ref: ref, description: description, capabilities: cloned}, nil
}

func (r ResourceType) Ref() ResourceTypeRef { return r.ref }
func (r ResourceType) Description() string  { return r.description }

func (r ResourceType) Capabilities() []Capability {
	return append([]Capability(nil), r.capabilities...)
}

func (r ResourceType) Supports(capability Capability) bool {
	for _, supported := range r.capabilities {
		if supported == capability {
			return true
		}
	}
	return false
}
