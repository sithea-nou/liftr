// SPDX-License-Identifier: Apache-2.0

package crossplane

import (
	"fmt"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube"
)

// Provisioner implements the unchanged provider-neutral provisioning
// contract against one Crossplane control plane. Submission persists
// platform-owned desired state and reports Accepted; convergence is proven
// exclusively through later Observe calls. No Kubernetes concept crosses
// this type's public surface.
type Provisioner struct {
	identity string
	client   kube.Client
	bindings map[domain.ResourceTypeRef]*resolvedBinding
}

var _ provisioning.Provisioner = (*Provisioner)(nil)

// New composes the adapter against a real control plane using an explicit
// kubeconfig or, when no path is configured, standard in-cluster credentials.
func New(config Config) (*Provisioner, error) {
	if _, _, err := config.resolve(); err != nil {
		return nil, err
	}
	var restConfig *kube.RestConfig
	var err error
	if config.KubeconfigPath != "" {
		restConfig, err = kube.RestConfigFromKubeconfig(config.KubeconfigPath, config.ContextName)
	} else {
		restConfig, err = kube.RestConfigInCluster()
	}
	if err != nil {
		return nil, fmt.Errorf("resolve crossplane cluster credentials: %w", err)
	}
	timeout := config.RequestTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	client, err := kube.NewClient(restConfig, timeout)
	if err != nil {
		return nil, fmt.Errorf("build crossplane cluster client: %w", err)
	}
	return newProvisioner(config, client)
}

// NewWithClient composes the adapter with a pre-built Kubernetes client.
// It exists for deterministic in-process tests and alternative composition;
// production deployments use New.
func NewWithClient(config Config, client kube.Client) (*Provisioner, error) {
	if client == nil {
		return nil, fmt.Errorf("kubernetes client is required")
	}
	return newProvisioner(config, client)
}

func newProvisioner(config Config, client kube.Client) (*Provisioner, error) {
	digest, bindings, err := config.resolve()
	if err != nil {
		return nil, err
	}
	if digest == "" || len(bindings) == 0 {
		return nil, fmt.Errorf("crossplane configuration is incomplete")
	}
	return &Provisioner{identity: config.Identity, client: client, bindings: bindings}, nil
}

func (p *Provisioner) identityFor(ref domain.ResourceTypeRef, id domain.ResourceID) identityMetadata {
	return identityMetadata{
		platformDigest: PlatformDigest(p.identity),
		resourceID:     id,
		resourceType:   ref,
	}
}

func (p *Provisioner) Capabilities() []provisioning.ProvisionerCapability {
	result := make([]provisioning.ProvisionerCapability, 0, len(p.bindings)*3)
	for ref, binding := range p.bindings {
		for _, capability := range binding.binding.Capabilities {
			result = append(result, provisioning.ProvisionerCapability{ResourceType: ref, Capability: capability})
		}
	}
	sortCapabilities(result)
	return result
}

// OutputMappingRef implements the worker's OutputMappingSource: the private
// mapping identity declared for create/update executions of a type. Delete
// executions never carry outputs.
func (p *Provisioner) OutputMappingRef(resourceType domain.ResourceTypeRef, capability domain.Capability) string {
	binding, ok := p.bindings[resourceType]
	if !ok || capability == domain.CapabilityDelete || !binding.supportsCapability(capability) {
		return ""
	}
	return binding.binding.CurrentOutputMappingRef
}

// SelectOutputRecoveryMapping returns only an explicitly compatible repair.
// Ordinary observations resolve their requested mapping exactly; recovery is
// the only path allowed to select a different implementation.
func (p *Provisioner) SelectOutputRecoveryMapping(resourceType domain.ResourceTypeRef, capability domain.Capability, sourceMappingRef string) (string, bool) {
	binding, ok := p.bindings[resourceType]
	if !ok || sourceMappingRef == "" || capability == domain.CapabilityDelete || !binding.supportsCapability(capability) {
		return "", false
	}
	for _, mapping := range binding.binding.OutputMappings {
		if mapping.Ref != sourceMappingRef && mapping.CompatibleSourceMappingRef == sourceMappingRef {
			return mapping.Ref, true
		}
	}
	return "", false
}

// operationRequest abstracts over ExecutionRequest and ObservationRequest so
// submit and observe share correlation helpers.
type operationRequest interface {
	resourceType() domain.ResourceTypeRef
	capability() domain.Capability
	targetGeneration() uint64
	operationID() domain.OperationID
	handle() *provisioning.ExecutionHandle
}

type executionRequestView struct{ request provisioning.ExecutionRequest }

func (v executionRequestView) resourceType() domain.ResourceTypeRef { return v.request.ResourceType }
func (v executionRequestView) capability() domain.Capability        { return v.request.Capability }
func (v executionRequestView) targetGeneration() uint64             { return v.request.TargetGeneration }
func (v executionRequestView) operationID() domain.OperationID      { return v.request.OperationID }
func (v executionRequestView) handle() *provisioning.ExecutionHandle {
	return nil
}

type observationRequestView struct {
	request provisioning.ObservationRequest
}

func (v observationRequestView) resourceType() domain.ResourceTypeRef { return v.request.ResourceType }
func (v observationRequestView) capability() domain.Capability        { return v.request.Capability }
func (v observationRequestView) targetGeneration() uint64             { return v.request.TargetGeneration }
func (v observationRequestView) operationID() domain.OperationID      { return v.request.OperationID }
func (v observationRequestView) handle() *provisioning.ExecutionHandle {
	return v.request.Handle
}

// failurePair is one classified preflight rejection.
type failurePair struct {
	kind   provisioning.ExecutionFailureKind
	reason string
}

// classifyAPIError maps structured control-plane failures to curated
// provider-neutral kinds before any conclusive rejection. Raw server
// messages never cross.
func classifyAPIError(err error) failurePair {
	switch {
	case kube.IsForbidden(err):
		return failurePair{provisioning.FailureUnavailable, reasonAccessDenied}
	case kube.IsInvalid(err):
		return failurePair{provisioning.FailureInvalidRequest, reasonAdmissionRejected}
	case kube.IsNotFound(err):
		// A 404 on create/apply means the target kind itself is not served:
		// the adapter only addresses objects after a successful read or an
		// authoritative absence on its own deterministic name.
		return failurePair{provisioning.FailureUnsupported, reasonTargetKindUnregistered}
	default:
		return failurePair{provisioning.FailureUnknown, "ControlPlaneRejected"}
	}
}

func (p *Provisioner) resolveBinding(request operationRequest) (*resolvedBinding, *failurePair) {
	binding, ok := p.bindings[request.resourceType()]
	if !ok {
		return nil, &failurePair{provisioning.FailureUnsupported, "ResourceTypeUnsupported"}
	}
	capability := request.capability()
	if capability != "" && !binding.supportsCapability(capability) {
		return nil, &failurePair{provisioning.FailureUnsupported, "CapabilityUnsupported"}
	}
	return binding, nil
}

func (p *Provisioner) targetName(binding *resolvedBinding, id domain.ResourceID) string {
	return ObjectName(p.identity, binding.namespace, binding.binding.ResourceType, id)
}
