// SPDX-License-Identifier: Apache-2.0

// Package xrbinding owns the private translation from provider-neutral
// PostgreSQLDatabase execution intent into the platform-owned composite
// resource consumed by Crossplane compositions. It belongs to the
// implementation side of the platform: nothing here appears in ResourceSpec,
// ResourceType discovery, the HTTP API, or the domain.
package xrbinding

import (
	"encoding/json"
	"fmt"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane"
) // EnvelopeVersion identifies the private output envelope schema shared with
// the XR composition contract. Every registered mapping asserts it before
// interpreting status material.
const EnvelopeVersion = 1

// DefaultNamespace is the private control-plane namespace used by the
// reference binding in deterministic tests and example composition.
const DefaultNamespace = "liftr-xr"

// OutputMappingRefV1 is the current immutable output-mapping identity of the
// v2 binding. The single registered status path holds one strictly validated
// envelope published by the platform-owned composition; arbitrary XR status
// is never read.
const (
	OutputMappingRefV1      = "xrb-postgres-v2-v1"
	OutputStatusEnvelopeRef = "xrb-postgres-v2-envelope-source"
	outputStatusPath0       = "liftr"
	outputStatusPath1       = "outputs"
)

// PostgresXRSpec is the platform-owned spec document the binding writes into
// every XR. Field names are implementation vocabulary; developer intent maps
// onto them exactly once, here.
type PostgresXRSpec struct {
	EngineVersion    string `json:"engineVersion"`
	StorageGB        int64  `json:"storageGB"`
	HighAvailability bool   `json:"highAvailability"`
}

// EncodePostgresSpec translates neutral execution intent into the XR spec
// document. Mapping rules mirror the Pulumi binding:
//
//	version          -> engine major version (string passthrough)
//	storageGB        -> integral capacity; int64 and float64(20.0) map identically
//	highAvailability -> availability flag passthrough
//
// Non-numeric, fractional, non-finite, or out-of-range storage values fail
// here instead of reaching infrastructure. The stored ResourceSpec is never
// mutated.
func EncodePostgresSpec(values map[string]any) (PostgresXRSpec, error) {
	spec := PostgresXRSpec{}
	versionValue, ok := values["version"]
	if !ok {
		return spec, fmt.Errorf("spec property %q is required", "version")
	}
	version, ok := versionValue.(string)
	if !ok || version == "" {
		return spec, fmt.Errorf("spec property %q must be a non-empty string", "version")
	}
	storageRaw, ok := values["storageGB"]
	if !ok {
		return spec, fmt.Errorf("spec property %q is required", "storageGB")
	}
	storageGB, ok := domain.IntegralValue(storageRaw)
	if !ok {
		return spec, fmt.Errorf("spec property %q must carry an integral capacity value", "storageGB")
	}
	availabilityRaw, ok := values["highAvailability"]
	if !ok {
		return spec, fmt.Errorf("spec property %q is required", "highAvailability")
	}
	highAvailability, ok := availabilityRaw.(bool)
	if !ok {
		return spec, fmt.Errorf("spec property %q must be a boolean", "highAvailability")
	}
	spec.EngineVersion = version
	spec.StorageGB = storageGB
	spec.HighAvailability = highAvailability
	return spec, nil
}

// Binding composes the private crossplane.Binding for PostgreSQLDatabase/v2.
func Binding() crossplane.Binding {
	ref := domain.ResourceTypeRef{Name: "PostgreSQLDatabase", Version: "v2"}
	return crossplane.Binding{
		ResourceType:  ref,
		Capabilities:  []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete},
		Target:        crossplane.GVK{Group: "platform.liftr.io", Version: "v1alpha1", Kind: "XPostgreSQLDatabase"},
		Plural:        "xpostgresqldatabases",
		Namespace:     DefaultNamespace,
		NamingVersion: crossplane.NamingVersionV1,
		EncodeInput: func(input crossplane.Input) ([]byte, error) {
			spec, err := EncodePostgresSpec(input.Spec.Values())
			if err != nil {
				return nil, err
			}
			return json.Marshal(spec)
		},
		Readiness: crossplane.DefaultConditionRules(),
		OutputMappings: []crossplane.OutputMapping{{
			Ref:                        OutputMappingRefV1,
			StatusPath:                 []string{outputStatusPath0, outputStatusPath1},
			CompatibleSourceMappingRef: "",
		}},
		CurrentOutputMappingRef: OutputMappingRefV1,
	}
}
