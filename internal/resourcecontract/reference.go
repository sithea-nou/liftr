// SPDX-License-Identifier: Apache-2.0

package resourcecontract

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sithea-nou/liftr/internal/domain"
)

// MaxReferenceSlots bounds one contract's declared reference slots.
const MaxReferenceSlots = 16

// MaxSlotItems bounds one slot's MaxItems regardless of contract declaration.
const MaxSlotItems = 16

// reservedReferenceNames cannot be declared as reference slots because they
// would collide with the public envelope or Liftr-reserved vocabulary.
var reservedReferenceNames = map[string]struct{}{
	"id": {}, "type": {}, "owner": {}, "spec": {}, "status": {},
	"generation": {}, "references": {}, "outputs": {}, "latestOperation": {},
	"createdAt": {}, "updatedAt": {},
}

// ReferenceSlot declares one named, bounded, hard-dependency slot of a
// ResourceType. Targets are exact ResourceTypeRefs; M21 admits no wildcards,
// selectors, expressions, provider constraints, or reference modes. All bound
// references are hard dependencies; optionality exists only as MinItems == 0.
type ReferenceSlot struct {
	Name               string                   `json:"name"`
	AllowedTargetTypes []domain.ResourceTypeRef `json:"allowedTargetTypes"`
	MinItems           int                      `json:"minItems"`
	MaxItems           int                      `json:"maxItems"`
}

// ReferenceContract is the immutable, ResourceType-owned description of the
// Liftr Resources a Resource may explicitly depend on. A nil *ReferenceContract
// means the type participates in no relationships as a source. Providers never
// define this contract; relationship semantics belong to Liftr alone.
type ReferenceContract struct {
	slots []ReferenceSlot
}

// NewReferenceContract validates and normalizes one reference contract. Slot
// names must be canonical, unique, and outside the reserved vocabulary;
// targets must be exact non-empty ResourceTypeRefs; cardinality must satisfy
// 0 <= MinItems <= MaxItems <= MaxSlotItems with MaxItems >= 1. The returned
// contract stores slots in deterministic name order with sorted targets.
func NewReferenceContract(slots []ReferenceSlot) (ReferenceContract, error) {
	if len(slots) == 0 {
		return ReferenceContract{}, fmt.Errorf("reference contract must declare at least one slot")
	}
	if len(slots) > MaxReferenceSlots {
		return ReferenceContract{}, fmt.Errorf("reference contract declares %d slots; the maximum is %d", len(slots), MaxReferenceSlots)
	}
	normalized := make([]ReferenceSlot, len(slots))
	copy(normalized, slots)
	for i, slot := range normalized {
		name := strings.TrimSpace(slot.Name)
		if name == "" || name != slot.Name {
			return ReferenceContract{}, fmt.Errorf("reference slot name %q is empty or not canonical", slot.Name)
		}
		if _, exists := reservedReferenceNames[name]; exists {
			return ReferenceContract{}, fmt.Errorf("reference slot name %q is reserved", name)
		}
		if len(slot.AllowedTargetTypes) == 0 {
			return ReferenceContract{}, fmt.Errorf("reference slot %q must declare at least one allowed target type", name)
		}
		targets := make([]domain.ResourceTypeRef, 0, len(slot.AllowedTargetTypes))
		seen := make(map[domain.ResourceTypeRef]struct{}, len(slot.AllowedTargetTypes))
		for _, target := range slot.AllowedTargetTypes {
			if strings.TrimSpace(target.Name) == "" || strings.TrimSpace(target.Version) == "" {
				return ReferenceContract{}, fmt.Errorf("reference slot %q declares an invalid target type: name and version are required", name)
			}
			if _, exists := seen[target]; exists {
				return ReferenceContract{}, fmt.Errorf("reference slot %q declares duplicate target type %s/%s", name, target.Name, target.Version)
			}
			seen[target] = struct{}{}
			targets = append(targets, target)
		}
		sort.Slice(targets, func(a, b int) bool {
			if targets[a].Name != targets[b].Name {
				return targets[a].Name < targets[b].Name
			}
			return targets[a].Version < targets[b].Version
		})
		if slot.MinItems != 0 && slot.MinItems != 1 {
			return ReferenceContract{}, fmt.Errorf("reference slot %q minItems must be 0 or 1", name)
		}
		if slot.MaxItems < 1 || slot.MaxItems > MaxSlotItems {
			return ReferenceContract{}, fmt.Errorf("reference slot %q maxItems must be between 1 and %d", name, MaxSlotItems)
		}
		if slot.MinItems > slot.MaxItems {
			return ReferenceContract{}, fmt.Errorf("reference slot %q minItems exceeds maxItems", name)
		}
		normalized[i] = ReferenceSlot{Name: name, AllowedTargetTypes: targets, MinItems: slot.MinItems, MaxItems: slot.MaxItems}
	}
	sort.Slice(normalized, func(a, b int) bool { return normalized[a].Name < normalized[b].Name })
	for i := 1; i < len(normalized); i++ {
		if normalized[i].Name == normalized[i-1].Name {
			return ReferenceContract{}, fmt.Errorf("reference slot %q is duplicated", normalized[i].Name)
		}
	}
	return ReferenceContract{slots: append([]ReferenceSlot(nil), normalized...)}, nil
}

// Slots returns the declared slots in deterministic name order.
func (c ReferenceContract) Slots() []ReferenceSlot {
	return append([]ReferenceSlot(nil), c.slots...)
}

// Slot returns the named slot declaration.
func (c ReferenceContract) Slot(name string) (ReferenceSlot, bool) {
	for _, slot := range c.slots {
		if slot.Name == name {
			return slot, true
		}
	}
	return ReferenceSlot{}, false
}

// AllowsTarget reports whether the slot accepts the exact target ref.
func (s ReferenceSlot) AllowsTarget(ref domain.ResourceTypeRef) bool {
	for _, allowed := range s.AllowedTargetTypes {
		if allowed == ref {
			return true
		}
	}
	return false
}
