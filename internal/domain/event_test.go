// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
)

func TestNewEvent(t *testing.T) {
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		id          domain.EventID
		resourceID  domain.ResourceID
		operationID domain.OperationID
		generation  uint64
		typeName    string
		reason      string
		at          time.Time
		wantErr     bool
	}{
		{name: "operation event", id: "event-1", resourceID: "resource-1", operationID: "operation-1", generation: 4, typeName: "OperationStarted", reason: "CreateRequested", at: now},
		{name: "resource event without operation", id: "event-1", resourceID: "resource-1", generation: 4, typeName: "ResourceUpdated", reason: "SpecChanged", at: now},
		{name: "missing ID", resourceID: "resource-1", generation: 4, typeName: "ResourceUpdated", reason: "SpecChanged", at: now, wantErr: true},
		{name: "missing resource ID", id: "event-1", generation: 4, typeName: "ResourceUpdated", reason: "SpecChanged", at: now, wantErr: true},
		{name: "missing generation", id: "event-1", resourceID: "resource-1", typeName: "ResourceUpdated", reason: "SpecChanged", at: now, wantErr: true},
		{name: "missing type", id: "event-1", resourceID: "resource-1", generation: 4, reason: "SpecChanged", at: now, wantErr: true},
		{name: "missing reason", id: "event-1", resourceID: "resource-1", generation: 4, typeName: "ResourceUpdated", at: now, wantErr: true},
		{name: "missing occurrence time", id: "event-1", resourceID: "resource-1", generation: 4, typeName: "ResourceUpdated", reason: "SpecChanged", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event, err := domain.NewEvent(tt.id, tt.resourceID, tt.operationID, tt.generation, tt.typeName, tt.reason, "message", tt.at)
			if (err != nil) != tt.wantErr {
				t.Fatalf("NewEvent() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if event.Generation() != tt.generation {
				t.Fatalf("Generation() = %d, want %d", event.Generation(), tt.generation)
			}
			if event.OperationID() != tt.operationID {
				t.Fatalf("OperationID() = %q, want %q", event.OperationID(), tt.operationID)
			}
		})
	}
}
