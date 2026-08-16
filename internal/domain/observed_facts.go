// SPDX-License-Identifier: Apache-2.0

package domain

import "fmt"

// ResourcePresence describes whether a backend reports a resource target.
type ResourcePresence string

const (
	ResourcePresencePresent  ResourcePresence = "Present"
	ResourcePresenceNotFound ResourcePresence = "NotFound"
	ResourcePresenceUnknown  ResourcePresence = "Unknown"
)

// ResourceReadiness describes whether a present resource is usable.
type ResourceReadiness string

const (
	ResourceReadinessReady    ResourceReadiness = "Ready"
	ResourceReadinessNotReady ResourceReadiness = "NotReady"
	ResourceReadinessUnknown  ResourceReadiness = "Unknown"
)

// ResourceDrift describes whether observed state differs from desired intent.
type ResourceDrift string

const (
	ResourceDriftInSync  ResourceDrift = "InSync"
	ResourceDriftDrifted ResourceDrift = "Drifted"
	ResourceDriftUnknown ResourceDrift = "Unknown"
)

// ObservedFacts are normalized backend facts. They are not lifecycle outcomes
// and do not imply an Operation succeeded or failed.
type ObservedFacts struct {
	Presence  ResourcePresence
	Readiness ResourceReadiness
	Drift     ResourceDrift
}

func (f ObservedFacts) Validate() error {
	if f.Presence != ResourcePresencePresent && f.Presence != ResourcePresenceNotFound && f.Presence != ResourcePresenceUnknown {
		return fmt.Errorf("invalid resource presence %q", f.Presence)
	}
	if f.Readiness != ResourceReadinessReady && f.Readiness != ResourceReadinessNotReady && f.Readiness != ResourceReadinessUnknown {
		return fmt.Errorf("invalid resource readiness %q", f.Readiness)
	}
	if f.Drift != ResourceDriftInSync && f.Drift != ResourceDriftDrifted && f.Drift != ResourceDriftUnknown {
		return fmt.Errorf("invalid resource drift %q", f.Drift)
	}
	if f.Presence != ResourcePresencePresent && f.Readiness != ResourceReadinessUnknown {
		return fmt.Errorf("readiness %q requires a present resource", f.Readiness)
	}
	return nil
}
