// SPDX-License-Identifier: Apache-2.0

// Package bindings owns the private translation from provider-neutral
// execution intent to the versioned input envelopes consumed by Liftr's
// registered Pulumi programs. It belongs to the implementation side of the
// platform: nothing here appears in ResourceSpec, ResourceType discovery, the
// HTTP API, or the domain. The package deliberately avoids importing any
// provisioner SDK so encoders stay cheap to test in isolation.
package bindings

import (
	"encoding/json"
	"fmt"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning/pulumi"
)

// EnvelopeVersion identifies the private program-input envelope schema. Every
// registered program asserts this value before interpreting its input.
const EnvelopeVersion = 1

// PostgresPlatform carries the private platform implementation configuration
// for one Azure Flexible Server deployment. Values are supplied by platform
// composition at startup and are never derived from developer intent. The
// enclosing resource group is not configured here: each database owns a
// private group derived from its infrastructure name so delete removes
// exactly that database's resources.
type PostgresPlatform struct {
	Location string `json:"location"`
	SkuName  string `json:"skuName"`
	SkuTier  string `json:"skuTier"`
	// HighAvailabilityMode selects how highAvailability=true maps onto the
	// backend (for example same-zone redundancy); Disabled is used when the
	// developer requested no high availability.
	HighAvailabilityMode string `json:"highAvailabilityMode"`
	AdministratorLogin   string `json:"administratorLogin"`
}

// Validate rejects incomplete platform configuration before any execution.
func (p PostgresPlatform) Validate() error {
	for name, value := range map[string]string{
		"location":             p.Location,
		"skuName":              p.SkuName,
		"skuTier":              p.SkuTier,
		"highAvailabilityMode": p.HighAvailabilityMode,
		"administratorLogin":   p.AdministratorLogin,
	} {
		if value == "" {
			return fmt.Errorf("platform configuration field %q is required", name)
		}
	}
	return nil
}

// postgreSQLEnvelope is the private input document consumed by the
// PostgreSQLDatabase/v1 programs. storageGB is always encoded as a canonical
// integral number so int64 and integral float64 representations of the same
// desired value produce byte-identical infrastructure arguments.
type postgreSQLEnvelope struct {
	InputVersion        int              `json:"inputVersion"`
	Capability          string           `json:"capability"`
	ResourceID          string           `json:"resourceId"`
	ResourceTypeName    string           `json:"resourceTypeName"`
	ResourceTypeVersion string           `json:"resourceTypeVersion"`
	TargetGeneration    uint64           `json:"targetGeneration"`
	InfraName           string           `json:"infraName"`
	Platform            PostgresPlatform `json:"platform"`
	Spec                struct {
		Version          string `json:"version"`
		StorageGB        int64  `json:"storageGB"`
		HighAvailability bool   `json:"highAvailability"`
	} `json:"spec"`
}

// PostgresRequest carries the provider-neutral execution intent of one
// PostgreSQLDatabase submission together with the resolved infrastructure
// name and private platform configuration.
type PostgresRequest struct {
	Capability       domain.Capability
	ResourceID       domain.ResourceID
	ResourceType     domain.ResourceTypeRef
	TargetGeneration uint64
	SpecValues       map[string]any
	InfraName        string
	Platform         PostgresPlatform
}

// EncodePostgresRequest translates one execution request into the private
// program input envelope. Mapping rules:
//
//	version          -> engine major version (string passthrough)
//	storageGB        -> integral capacity; int64 and float64(20.0) map to 20
//	highAvailability -> availability mode selected by platform policy
//
// Non-numeric, fractional, non-finite, or out-of-range storage values fail
// here instead of reaching infrastructure. The stored ResourceSpec is never
// mutated; SpecValues must be a defensive copy.
func EncodePostgresRequest(request PostgresRequest) ([]byte, error) {
	envelope := postgreSQLEnvelope{
		InputVersion:        EnvelopeVersion,
		Capability:          string(request.Capability),
		ResourceID:          string(request.ResourceID),
		ResourceTypeName:    request.ResourceType.Name,
		ResourceTypeVersion: request.ResourceType.Version,
		TargetGeneration:    request.TargetGeneration,
		InfraName:           request.InfraName,
		Platform:            request.Platform,
	}
	switch request.Capability {
	case domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete:
	default:
		return nil, fmt.Errorf("unsupported capability %q", request.Capability)
	}
	values := request.SpecValues
	versionValue, ok := values["version"]
	if !ok {
		return nil, fmt.Errorf("spec property %q is required", "version")
	}
	version, ok := versionValue.(string)
	if !ok || version == "" {
		return nil, fmt.Errorf("spec property %q must be a non-empty string", "version")
	}
	storageRaw, ok := values["storageGB"]
	if !ok {
		return nil, fmt.Errorf("spec property %q is required", "storageGB")
	}
	storageGB, ok := domain.IntegralValue(storageRaw)
	if !ok {
		return nil, fmt.Errorf("spec property %q must carry an integral capacity value", "storageGB")
	}
	availabilityRaw, ok := values["highAvailability"]
	if !ok {
		return nil, fmt.Errorf("spec property %q is required", "highAvailability")
	}
	highAvailability, ok := availabilityRaw.(bool)
	if !ok {
		return nil, fmt.Errorf("spec property %q must be a boolean", "highAvailability")
	}
	envelope.Spec.Version = version
	envelope.Spec.StorageGB = storageGB
	envelope.Spec.HighAvailability = highAvailability
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode program input: %w", err)
	}
	return encoded, nil
}

// PostgresEncoder returns the InputEncoder that binds the
// PostgreSQLDatabase/v1 registration to the envelope above. identity and
// namespace scope the derived infrastructure name to this installation;
// platform carries the private implementation configuration.
func PostgresEncoder(identity, namespace string, platform PostgresPlatform) pulumi.InputEncoder {
	return func(input pulumi.Input) ([]byte, error) {
		if err := platform.Validate(); err != nil {
			return nil, err
		}
		return EncodePostgresRequest(PostgresRequest{
			Capability:       input.Capability,
			ResourceID:       input.ResourceID,
			ResourceType:     input.ResourceType,
			TargetGeneration: input.TargetGeneration,
			SpecValues:       input.Spec.Values(),
			InfraName:        pulumi.InfraName(identity, namespace, input.ResourceType, input.ResourceID),
			Platform:         platform,
		})
	}
}
