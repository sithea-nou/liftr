// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"fmt"
	"math"
	"strings"
	"time"
)

type ResourceID string

type ResourceTypeRef struct {
	Name    string
	Version string
}

func (r ResourceTypeRef) validate() error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("resource type name is required")
	}
	if strings.TrimSpace(r.Version) == "" {
		return fmt.Errorf("resource type version is required")
	}
	return nil
}

// OwnerRef identifies an owner without coupling ownership to an identity provider.
type OwnerRef struct {
	Kind string
	ID   string
}

func (o OwnerRef) validate() error {
	if strings.TrimSpace(o.Kind) == "" {
		return fmt.Errorf("owner kind is required")
	}
	if strings.TrimSpace(o.ID) == "" {
		return fmt.Errorf("owner ID is required")
	}
	return nil
}

// Resource contains developer-owned desired state. Observed state is held separately in ResourceStatus.
type Resource struct {
	id         ResourceID
	typeRef    ResourceTypeRef
	owner      OwnerRef
	generation uint64
	spec       ResourceSpec
	createdAt  time.Time
	updatedAt  time.Time
}

// ResourceSnapshot is the persistence representation of a Resource. It exists
// to restore private domain state without exposing persistence technology.
type ResourceSnapshot struct {
	ID         ResourceID
	Type       ResourceTypeRef
	Owner      OwnerRef
	Generation uint64
	Spec       ResourceSpec
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func RestoreResource(snapshot ResourceSnapshot) (Resource, error) {
	if snapshot.Generation == 0 {
		return Resource{}, fmt.Errorf("resource generation must be greater than zero")
	}
	resource, err := NewResource(snapshot.ID, snapshot.Type, snapshot.Owner, snapshot.Spec, snapshot.CreatedAt)
	if err != nil {
		return Resource{}, err
	}
	if snapshot.UpdatedAt.IsZero() {
		return Resource{}, fmt.Errorf("resource update time is required")
	}
	if snapshot.UpdatedAt.Before(snapshot.CreatedAt) {
		return Resource{}, fmt.Errorf("resource update time cannot precede creation")
	}
	resource.generation = snapshot.Generation
	resource.updatedAt = snapshot.UpdatedAt
	return resource, nil
}

func NewResource(id ResourceID, typeRef ResourceTypeRef, owner OwnerRef, spec ResourceSpec, createdAt time.Time) (Resource, error) {
	if strings.TrimSpace(string(id)) == "" {
		return Resource{}, fmt.Errorf("resource ID is required")
	}
	if err := typeRef.validate(); err != nil {
		return Resource{}, err
	}
	if err := owner.validate(); err != nil {
		return Resource{}, err
	}
	if createdAt.IsZero() {
		return Resource{}, fmt.Errorf("resource creation time is required")
	}

	return Resource{
		id:         id,
		typeRef:    typeRef,
		owner:      owner,
		generation: 1,
		spec:       spec,
		createdAt:  createdAt,
		updatedAt:  createdAt,
	}, nil
}

func (r Resource) ID() ResourceID        { return r.id }
func (r Resource) Type() ResourceTypeRef { return r.typeRef }
func (r Resource) Owner() OwnerRef       { return r.owner }
func (r Resource) Generation() uint64    { return r.generation }
func (r Resource) Spec() ResourceSpec    { return r.spec }
func (r Resource) CreatedAt() time.Time  { return r.createdAt }
func (r Resource) UpdatedAt() time.Time  { return r.updatedAt }

// UpdateSpec records a new desired-state revision.
func (r *Resource) UpdateSpec(spec ResourceSpec, updatedAt time.Time) error {
	if updatedAt.IsZero() {
		return fmt.Errorf("resource update time is required")
	}
	if updatedAt.Before(r.updatedAt) {
		return fmt.Errorf("resource update time cannot precede the previous update")
	}
	if r.generation == math.MaxUint64 {
		return fmt.Errorf("resource generation cannot be incremented")
	}

	r.spec = spec
	r.generation++
	r.updatedAt = updatedAt
	return nil
}
