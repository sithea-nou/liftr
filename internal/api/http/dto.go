// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
)

// resourceTypeDTO identifies one versioned ResourceType.
type resourceTypeDTO struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ownerDTO identifies the owner of a Resource.
type ownerDTO struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// conditionDTO is one normalized provider-neutral fact about a Resource.
type conditionDTO struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason"`
	Message            string    `json:"message,omitempty"`
	ObservedGeneration uint64    `json:"observedGeneration"`
	LastTransitionAt   time.Time `json:"lastTransitionAt"`
}

// resourceStatusDTO is Liftr's explicit provider-neutral observation of a
// Resource. It contains no provider or persistence metadata.
type resourceStatusDTO struct {
	State              string         `json:"state"`
	ObservedGeneration uint64         `json:"observedGeneration"`
	Conditions         []conditionDTO `json:"conditions,omitempty"`
	UpdatedAt          time.Time      `json:"updatedAt"`
}

// latestOperationRefDTO points at the most recent lifecycle Operation of a
// Resource without duplicating its full representation.
type latestOperationRefDTO struct {
	ID               string `json:"id"`
	Capability       string `json:"capability"`
	State            string `json:"state"`
	TargetGeneration uint64 `json:"targetGeneration"`
	Href             string `json:"href"`
}

// resourceDTO is the public v1 Resource representation. It carries desired
// state, normalized observed state, and a monitor link for the latest
// Operation. Internal concepts such as phases, provisioner references,
// execution handles, attempts, outbox records, fingerprints, and storage
// versions have no representation here.
type resourceDTO struct {
	ID              string                 `json:"id"`
	Type            resourceTypeDTO        `json:"type"`
	Owner           ownerDTO               `json:"owner"`
	Generation      uint64                 `json:"generation"`
	Spec            map[string]any         `json:"spec"`
	Status          resourceStatusDTO      `json:"status"`
	LatestOperation *latestOperationRefDTO `json:"latestOperation,omitempty"`
	CreatedAt       time.Time              `json:"createdAt"`
	UpdatedAt       time.Time              `json:"updatedAt"`
}

// operationFailureDTO explains why an Operation reached the Failed state.
type operationFailureDTO struct {
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

// operationDTO is the public v1 Operation representation. OperationPhase and
// phaseChangedAt are deliberately not part of v1.
type operationDTO struct {
	ID               string               `json:"id"`
	ResourceID       string               `json:"resourceId"`
	Capability       string               `json:"capability"`
	State            string               `json:"state"`
	TargetGeneration uint64               `json:"targetGeneration"`
	RequestedAt      time.Time            `json:"requestedAt"`
	StartedAt        *time.Time           `json:"startedAt,omitempty"`
	CompletedAt      *time.Time           `json:"completedAt,omitempty"`
	Failure          *operationFailureDTO `json:"failure,omitempty"`
}

func instant(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	value := at.UTC()
	return &value
}

func newResourceDTO(record application.ResourceRecord, latest *domain.Operation) resourceDTO {
	resource := record.Resource
	resourceType := resource.Type()
	owner := resource.Owner()
	status := record.Status

	dto := resourceDTO{
		ID:         string(resource.ID()),
		Type:       resourceTypeDTO{Name: resourceType.Name, Version: resourceType.Version},
		Owner:      ownerDTO{Kind: owner.Kind, ID: owner.ID},
		Generation: resource.Generation(),
		Spec:       resource.Spec().Values(),
		Status: resourceStatusDTO{
			State:              string(status.State()),
			ObservedGeneration: status.ObservedGeneration(),
			Conditions:         []conditionDTO{},
			UpdatedAt:          status.UpdatedAt().UTC(),
		},
		CreatedAt: resource.CreatedAt().UTC(),
		UpdatedAt: resource.UpdatedAt().UTC(),
	}
	for _, condition := range status.Conditions() {
		dto.Status.Conditions = append(dto.Status.Conditions, conditionDTO{
			Type:               condition.Type(),
			Status:             string(condition.Status()),
			Reason:             condition.Reason(),
			Message:            condition.Message(),
			ObservedGeneration: condition.ObservedGeneration(),
			LastTransitionAt:   condition.LastTransitionAt().UTC(),
		})
	}
	if latest != nil {
		ref := newLatestOperationRef(*latest)
		dto.LatestOperation = &ref
	}
	return dto
}

func newLatestOperationRef(operation domain.Operation) latestOperationRefDTO {
	id := string(operation.ID())
	return latestOperationRefDTO{
		ID:               id,
		Capability:       string(operation.Capability()),
		State:            string(operation.State()),
		TargetGeneration: operation.TargetGeneration(),
		Href:             "/v1/operations/" + id,
	}
}

func newOperationDTO(operation domain.Operation) operationDTO {
	dto := operationDTO{
		ID:               string(operation.ID()),
		ResourceID:       string(operation.ResourceID()),
		Capability:       string(operation.Capability()),
		State:            string(operation.State()),
		TargetGeneration: operation.TargetGeneration(),
		RequestedAt:      operation.RequestedAt().UTC(),
		StartedAt:        instant(operation.StartedAt()),
		CompletedAt:      instant(operation.CompletedAt()),
	}
	if failure, failed := operation.Failure(); failed {
		dto.Failure = &operationFailureDTO{Reason: failure.Reason(), Message: failure.Message()}
	}
	return dto
}
