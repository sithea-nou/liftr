// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

type EventID string

// EventActor identifies the authenticated principal that requested a lifecycle
// mutation. It carries deliberately selected normalized audit fields only —
// never tokens, raw claims, memberships, or authorization decisions (ADR-0012).
type EventActor struct {
	ID   string // stable PrincipalID of the requesting principal
	Kind string // principal kind, e.g. "user"
}

// EventAdmission records private provenance for a successfully admitted
// desired-state mutation. The lifecycle engine never sets it; application
// admission stamps it after platform policy succeeds.
type EventAdmission struct {
	PolicyRevision string
}

// Event is an append-only audit/history record, not an event-sourcing primitive.
type Event struct {
	id          EventID
	resourceID  ResourceID
	operationID OperationID
	generation  uint64
	typeName    string
	reason      string
	message     string
	occurredAt  time.Time
	actor       *EventActor
	admission   *EventAdmission
}

func NewEvent(id EventID, resourceID ResourceID, operationID OperationID, generation uint64, typeName, reason, message string, occurredAt time.Time) (Event, error) {
	if strings.TrimSpace(string(id)) == "" {
		return Event{}, fmt.Errorf("event ID is required")
	}
	if strings.TrimSpace(string(resourceID)) == "" {
		return Event{}, fmt.Errorf("resource ID is required")
	}
	if generation == 0 {
		return Event{}, fmt.Errorf("event generation must be greater than zero")
	}
	if strings.TrimSpace(typeName) == "" {
		return Event{}, fmt.Errorf("event type is required")
	}
	if strings.TrimSpace(reason) == "" {
		return Event{}, fmt.Errorf("event reason is required")
	}
	if occurredAt.IsZero() {
		return Event{}, fmt.Errorf("event occurrence time is required")
	}

	return Event{
		id:          id,
		resourceID:  resourceID,
		operationID: operationID,
		generation:  generation,
		typeName:    typeName,
		reason:      reason,
		message:     message,
		occurredAt:  occurredAt,
	}, nil
}

func (e Event) ID() EventID              { return e.id }
func (e Event) ResourceID() ResourceID   { return e.resourceID }
func (e Event) OperationID() OperationID { return e.operationID }
func (e Event) Generation() uint64       { return e.generation }
func (e Event) Type() string             { return e.typeName }
func (e Event) Reason() string           { return e.reason }
func (e Event) Message() string          { return e.message }
func (e Event) OccurredAt() time.Time    { return e.occurredAt }

// Actor returns the requesting principal when one was recorded. Internal
// lifecycle transitions carry no actor; admission events for authenticated
// mutations always do (ADR-0012).
func (e Event) Actor() (EventActor, bool) {
	if e.actor == nil {
		return EventActor{}, false
	}
	return *e.actor, true
}

func (e Event) Admission() (EventAdmission, bool) {
	if e.admission == nil {
		return EventAdmission{}, false
	}
	return *e.admission, true
}

// WithActor returns a copy of the event stamped with the requesting
// principal. The lifecycle engine never sets actors; the application layer
// stamps the admission event with the authenticated caller before it is
// appended, so worker-driven transitions remain unattributed system actions.
// Actor identity is validated here: an actor without a stable ID and kind is
// rejected rather than persisted in a lossy form.
func (e Event) WithActor(actor EventActor) (Event, error) {
	if strings.TrimSpace(actor.ID) == "" {
		return Event{}, fmt.Errorf("event actor ID is required")
	}
	if strings.TrimSpace(actor.Kind) == "" {
		return Event{}, fmt.Errorf("event actor kind is required")
	}
	copied := e
	copied.actor = &EventActor{ID: strings.TrimSpace(actor.ID), Kind: strings.TrimSpace(actor.Kind)}
	return copied, nil
}

// WithAdmissionPolicyRevision returns a copy carrying the immutable policy
// revision used for this admission. It is typed metadata, never an arbitrary
// config-controlled payload.
func (e Event) WithAdmissionPolicyRevision(revision string) (Event, error) {
	const prefix = "pol_v1_"
	digest := strings.TrimPrefix(revision, prefix)
	_, decodeErr := hex.DecodeString(digest)
	if revision != strings.TrimSpace(revision) || !strings.HasPrefix(revision, prefix) || len(digest) != 64 || digest != strings.ToLower(digest) || decodeErr != nil {
		return Event{}, fmt.Errorf("event admission policy revision is invalid")
	}
	copied := e
	copied.admission = &EventAdmission{PolicyRevision: revision}
	return copied, nil
}
