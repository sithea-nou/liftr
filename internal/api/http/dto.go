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

// resourceOutputsDTO is the public realized-value envelope. observedGeneration
// identifies the desired generation whose successful reconciliation produced
// the values; it is an explicit freshness association, not a claim that the
// values describe current runtime health. Values are flat non-secret scalars
// whose exact names and types are declared by the ResourceType's output
// contract in discovery.
type resourceOutputsDTO struct {
	ObservedGeneration uint64         `json:"observedGeneration"`
	Values             map[string]any `json:"values"`
}

// resourceDTO is the public v1 Resource representation. It carries desired
// state, normalized observed state, and a monitor link for the latest
// Operation. Internal concepts such as phases, provisioner references,
// execution handles, attempts, outbox records, fingerprints, and storage
// versions have no representation here. references exposes the canonical
// DESIRED dependency set (M21); applied references are internal protective
// evidence and never serialized.
type resourceDTO struct {
	ID              string                 `json:"id"`
	Type            resourceTypeDTO        `json:"type"`
	Owner           ownerDTO               `json:"owner"`
	Generation      uint64                 `json:"generation"`
	Spec            map[string]any         `json:"spec"`
	Status          resourceStatusDTO      `json:"status"`
	LatestOperation *latestOperationRefDTO `json:"latestOperation,omitempty"`
	Outputs         *resourceOutputsDTO    `json:"outputs,omitempty"`
	References      map[string][]string    `json:"references,omitempty"`
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
	RetryOf          string               `json:"retryOf,omitempty"`
	Capability       string               `json:"capability"`
	State            string               `json:"state"`
	TargetGeneration uint64               `json:"targetGeneration"`
	RequestedAt      time.Time            `json:"requestedAt"`
	StartedAt        *time.Time           `json:"startedAt,omitempty"`
	CompletedAt      *time.Time           `json:"completedAt,omitempty"`
	Failure          *operationFailureDTO `json:"failure,omitempty"`
}

type operationListDTO struct {
	Items      []operationDTO `json:"items"`
	NextCursor string         `json:"nextCursor,omitempty"`
}

func instant(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	value := at.UTC()
	return &value
}

func newResourceDTO(record application.ResourceRecord, latest *domain.Operation, outputs *domain.ResourceOutputs, references []application.ReferenceEdge) resourceDTO {
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
	if len(references) > 0 {
		grouped := map[string][]string{}
		for _, edge := range references {
			grouped[edge.Slot] = append(grouped[edge.Slot], string(edge.TargetID))
		}
		dto.References = grouped
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
	if outputs != nil {
		dto.Outputs = &resourceOutputsDTO{
			ObservedGeneration: outputs.ObservedGeneration(),
			Values:             outputs.Values(),
		}
	}
	return dto
}

// resourceSummaryStatusDTO is the inventory observation of a Resource: state
// and freshness only. Conditions belong to the detail representation.
type resourceSummaryStatusDTO struct {
	State              string    `json:"state"`
	ObservedGeneration uint64    `json:"observedGeneration"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

// resourceSummaryDTO is the public v1 inventory summary. It is built from the
// application's dedicated inventory read model, so spec, conditions, outputs,
// provisioner bindings, execution metadata, and private sequences have no
// representation here by construction (ADR-0016). Clients read the full
// representation from /v1/resources/{id}.
type resourceSummaryDTO struct {
	ID              string                   `json:"id"`
	Type            resourceTypeDTO          `json:"type"`
	Owner           ownerDTO                 `json:"owner"`
	Generation      uint64                   `json:"generation"`
	Status          resourceSummaryStatusDTO `json:"status"`
	LatestOperation *latestOperationRefDTO   `json:"latestOperation,omitempty"`
	CreatedAt       time.Time                `json:"createdAt"`
	UpdatedAt       time.Time                `json:"updatedAt"`
}

// resourceListDTO is one ownership-scoped inventory page. items is never nil
// so an empty page serializes as an empty array; nextCursor is absent on the
// final page and there is deliberately no total count.
type resourceListDTO struct {
	Items      []resourceSummaryDTO `json:"items"`
	NextCursor string               `json:"nextCursor,omitempty"`
}

func newResourceSummaryDTO(item application.ResourceInventoryItem) resourceSummaryDTO {
	summary := resourceSummaryDTO{
		ID:         string(item.ID),
		Type:       resourceTypeDTO{Name: item.Type.Name, Version: item.Type.Version},
		Owner:      ownerDTO{Kind: item.Owner.Kind, ID: item.Owner.ID},
		Generation: item.Generation,
		Status: resourceSummaryStatusDTO{
			State:              string(item.Status.State),
			ObservedGeneration: item.Status.ObservedGeneration,
			UpdatedAt:          item.Status.UpdatedAt.UTC(),
		},
		CreatedAt: item.CreatedAt.UTC(),
		UpdatedAt: item.UpdatedAt.UTC(),
	}
	if item.Latest != nil {
		id := string(item.Latest.ID)
		summary.LatestOperation = &latestOperationRefDTO{
			ID:               id,
			Capability:       string(item.Latest.Capability),
			State:            string(item.Latest.State),
			TargetGeneration: item.Latest.TargetGeneration,
			Href:             "/v1/operations/" + id,
		}
	}
	return summary
}

func newResourceListDTO(page application.ResourceInventoryPageView) resourceListDTO {
	body := resourceListDTO{Items: make([]resourceSummaryDTO, 0, len(page.Items))}
	for i := range page.Items {
		body.Items = append(body.Items, newResourceSummaryDTO(page.Items[i]))
	}
	body.NextCursor = page.NextCursor
	return body
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
		RetryOf:          string(operation.RetryOfOperationID()),
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
