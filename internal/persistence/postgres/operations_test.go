// SPDX-License-Identifier: Apache-2.0

package postgres

import (
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
)

func TestRestoreOperationHistoryMetadata(t *testing.T) {
	requestedAt := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	retryOf := "operation-failed"
	record, err := restoreOperation(
		"operation-retry", "resource-1", "42", &retryOf, string(domain.CapabilityUpdate), "7",
		string(domain.OperationStatePending), string(domain.OperationPhaseRequested), requestedAt.UnixNano(), nil,
		requestedAt.UnixNano(), nil, nil, nil, "3",
	)
	if err != nil {
		t.Fatal(err)
	}
	if record.Sequence != 42 || record.Version != 3 || record.Operation.RetryOfOperationID() != domain.OperationID(retryOf) {
		t.Fatalf("restored record sequence=%d version=%d retryOf=%q", record.Sequence, record.Version, record.Operation.RetryOfOperationID())
	}
}
