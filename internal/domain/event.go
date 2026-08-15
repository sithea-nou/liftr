// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"fmt"
	"strings"
	"time"
)

type EventID string

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
