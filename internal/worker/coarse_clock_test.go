// SPDX-License-Identifier: Apache-2.0

package worker_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

// coarseClockProvider models backends whose history timestamps are coarser
// than Liftr's own clock: its single observation reports terminal success at
// a whole-second instant that can predate state Liftr advanced nanoseconds
// after launching the very same correlated execution.
type coarseClockProvider struct {
	mu          sync.Mutex
	submissions map[domain.OperationID]int
	observedAt  time.Time
}

func newCoarseClockProvider(observedAt time.Time) *coarseClockProvider {
	return &coarseClockProvider{submissions: map[domain.OperationID]int{}, observedAt: observedAt}
}

func (*coarseClockProvider) Capabilities() []provisioning.ProvisionerCapability { return nil }

func (p *coarseClockProvider) Submit(_ context.Context, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	p.mu.Lock()
	p.submissions[request.OperationID]++
	p.mu.Unlock()
	handle, _ := provisioning.NewExecutionHandle("h-" + string(request.OperationID))
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateUnknown, Handle: &handle,
			Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureUnknown, Reason: "SubmissionOutcomeUnknown", Message: "unknown"}},
		Resource: readyFacts(),
	}}, provisioning.ErrAmbiguousSubmission
}

func (p *coarseClockProvider) Observe(_ context.Context, request provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	handle, _ := provisioning.NewExecutionHandle("h-" + string(request.OperationID))
	return provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle},
		Resource:    readyFacts(),
		ObservedAt:  p.observedAt,
	}, nil
}

func (p *coarseClockProvider) submissionCount(id domain.OperationID) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.submissions[id]
}

// TestCoarseBackendTimestampAlignsWithPersistedFrontier pins the M10 fix for
// mixed-clock granularity: correlated terminal success whose backend end time
// is truncated below Liftr's own transition timestamps still completes the
// lifecycle, with the completion lifted onto the persisted frontier instead
// of being rejected as regressive.
func TestCoarseBackendTimestampAlignsWithPersistedFrontier(t *testing.T) {
	provider := newCoarseClockProvider(testTime)
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = 0

	command := createCommand(t, "resource-coarse-clock", "operation-coarse-clock")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 8 {
		if worked, err := instance.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce error: %v", err)
		} else if !worked {
			break
		}
	}

	record, err := store.GetOperation(context.Background(), command.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if !record.Operation.IsTerminal() || record.Operation.State() != domain.OperationStateSucceeded {
		failure, _ := record.Operation.Failure()
		t.Fatalf("operation did not complete from coarse backend evidence: %s %+v", record.Operation.State(), failure)
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if !record.Operation.CompletedAt().Equal(execution.LastObservedAt) {
		t.Fatalf("completion %v diverges from effective evidence %v", record.Operation.CompletedAt(), execution.LastObservedAt)
	}
	view := readView(t, store, "resource-coarse-clock")
	if view.Resource.Status.State() != domain.ResourceStateReady {
		t.Fatalf("state = %s", view.Resource.Status.State())
	}
	if record.Operation.CompletedAt().Before(view.Resource.Resource.UpdatedAt()) {
		t.Fatalf("completion %v regressed below persisted resource state %v", record.Operation.CompletedAt(), view.Resource.Resource.UpdatedAt())
	}
	if provider.submissionCount(command.OperationID) != 1 {
		t.Fatalf("alignment triggered resubmission (%d)", provider.submissionCount(command.OperationID))
	}
}
