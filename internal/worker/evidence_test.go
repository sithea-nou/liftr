// SPDX-License-Identifier: Apache-2.0

package worker_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

func invalidFacts() provisioning.ResourceObservation {
	return provisioning.ResourceObservation{Presence: "BogusPresence"}
}

func unknownFacts() provisioning.ResourceObservation {
	return provisioning.ResourceObservation{Presence: provisioning.ResourcePresenceUnknown, Readiness: provisioning.ResourceReadinessUnknown, Drift: provisioning.ResourceDriftUnknown}
}

// staleSubmissionProvider returns a per-attempt submission and a single
// observation so tests can exercise stale resubmission evidence.
type staleSubmissionProvider struct {
	mu          sync.Mutex
	submits     map[uint64]provisioning.Submission
	submissions int
	observed    int
	observe     provisioning.ExecutionObservation
}

func (p *staleSubmissionProvider) Capabilities() []provisioning.ProvisionerCapability { return nil }

func (p *staleSubmissionProvider) Submit(_ context.Context, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.submissions++
	if submission, ok := p.submits[request.AttemptNumber]; ok {
		return submission, nil
	}
	return p.submits[1], nil
}

func (p *staleSubmissionProvider) Observe(_ context.Context, _ provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.observed++
	return p.observe, nil
}

// staleThenAdvancingProvider observes NotFound once (to move an ambiguous
// attempt to a resubmission) and then reports fresh terminal evidence.
type staleThenAdvancingProvider struct {
	mu       sync.Mutex
	submits  map[uint64]provisioning.Submission
	observed int
}

func (p *staleThenAdvancingProvider) Capabilities() []provisioning.ProvisionerCapability { return nil }

func (p *staleThenAdvancingProvider) Submit(_ context.Context, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.submits[request.AttemptNumber], nil
}

func (p *staleThenAdvancingProvider) Observe(_ context.Context, _ provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.observed++
	if p.observed == 1 {
		return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationNotFound,
			Resource: unknownFacts(), ObservedAt: testTime.Add(4 * time.Hour)}, nil
	}
	return provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded}, Resource: readyFacts(), ObservedAt: testTime.Add(5 * time.Hour)}, nil
}

// staleObserveProvider returns a fixed submission and sequenced observations.
type staleObserveProvider struct {
	mu       sync.Mutex
	submit   provisioning.Submission
	observes []provisioning.ExecutionObservation
	observed int
}

func (p *staleObserveProvider) Capabilities() []provisioning.ProvisionerCapability { return nil }

func (p *staleObserveProvider) Submit(_ context.Context, _ provisioning.ExecutionRequest) (provisioning.Submission, error) {
	return p.submit, nil
}

func (p *staleObserveProvider) Observe(_ context.Context, _ provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.observed >= len(p.observes) {
		return p.observes[len(p.observes)-1], nil
	}
	observation := p.observes[p.observed]
	p.observed++
	return observation, nil
}

func acceptedSubmission(at time.Time) provisioning.Submission {
	handle, _ := provisioning.NewExecutionHandle("stale-handle")
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationFound,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle},
		Resource:    runningFacts(), ObservedAt: at,
	}}
}

func TestStaleTerminalSubmissionIsAmbiguousAndDoesNotTransition(t *testing.T) {
	handle, _ := provisioning.NewExecutionHandle("stale-terminal")
	provider := &staleSubmissionProvider{
		submits: map[uint64]provisioning.Submission{
			1: {Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationUnknown,
				Execution:  &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle},
				ObservedAt: testTime.Add(3 * time.Hour)}},
			2: {Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
				Execution:  &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle},
				Resource:   readyFacts(),
				ObservedAt: testTime.Add(2 * time.Hour)}},
		},
		observe: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationNotFound,
			Resource: unknownFacts(), ObservedAt: testTime.Add(4 * time.Hour)},
	}
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = 0
	command := createCommand(t, "resource-stale-submission", "operation-stale-submission")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := instance.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("first dispatch worked=%t error=%v", worked, err)
	}
	if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("observation worked=%t error=%v", worked, err)
	}
	if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("stale resubmission dispatch worked=%t error=%v", worked, err)
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if execution.State != application.AttemptUnknown {
		t.Fatalf("execution state=%s, want Unknown", execution.State)
	}
	if execution.Correlation != provisioning.RequestCorrelationUnknown {
		t.Fatalf("execution correlation=%s, want Unknown", execution.Correlation)
	}
	if execution.LastFailure == nil || execution.LastFailure.Reason != "StaleSubmissionEvidence" {
		t.Fatalf("execution failure=%v, want StaleSubmissionEvidence", execution.LastFailure)
	}
	if !execution.LastObservedAt.Equal(testTime.Add(4 * time.Hour)) {
		t.Fatalf("execution LastObservedAt=%v, want T+4h (no regression to stale evidence)", execution.LastObservedAt)
	}
	attempt, err := store.GetSubmissionAttempt(context.Background(), command.OperationID, 2)
	if err != nil || attempt.State != application.SubmissionAttemptUnknown {
		t.Fatalf("attempt error=%v state=%s, want Unknown", err, attempt.State)
	}
	if attempt.Failure == nil || attempt.Failure.Reason != "StaleSubmissionEvidence" {
		t.Fatalf("attempt failure=%v, want StaleSubmissionEvidence", attempt.Failure)
	}
	followUp, err := store.GetOutbox(context.Background(), application.ObserveMessage(command.OperationID, 2, 0).ID)
	if err != nil || followUp.State != application.OutboxPending {
		t.Fatalf("follow-up observe error=%v state=%s, want pending", err, followUp.State)
	}
	operation, err := store.GetOperation(context.Background(), command.OperationID)
	if err != nil || operation.Operation.IsTerminal() {
		t.Fatalf("operation error=%v terminal=%t, want nonterminal", err, operation.Operation.IsTerminal())
	}
}

func TestStaleFailedSubmissionDoesNotFailOperation(t *testing.T) {
	handle, _ := provisioning.NewExecutionHandle("stale-failed")
	provider := &staleSubmissionProvider{
		submits: map[uint64]provisioning.Submission{
			1: {Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationUnknown,
				Execution:  &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle},
				ObservedAt: testTime.Add(3 * time.Hour)}},
			2: {Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
				Execution: &provisioning.Execution{State: provisioning.ExecutionStateFailed, Handle: &handle,
					Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureExecution, Reason: "ExecutionFailed", Message: "stale failure"}},
				ObservedAt: testTime.Add(2 * time.Hour)}},
		},
		observe: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationNotFound,
			Resource: unknownFacts(), ObservedAt: testTime.Add(4 * time.Hour)},
	}
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = 0
	command := createCommand(t, "resource-stale-failed", "operation-stale-failed")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		if _, err := instance.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if execution.State == application.AttemptFailed {
		t.Fatalf("execution state=%s, want nonterminal despite stale failed evidence", execution.State)
	}
	if execution.LastFailure == nil || execution.LastFailure.Reason != "StaleSubmissionEvidence" {
		t.Fatalf("execution failure=%v, want StaleSubmissionEvidence", execution.LastFailure)
	}
	operation, err := store.GetOperation(context.Background(), command.OperationID)
	if err != nil || operation.Operation.State() == domain.OperationStateFailed {
		t.Fatalf("operation error=%v state=%s, want non-failed", err, operation.Operation.State())
	}
}

func TestEqualTimestampSubmissionIsStale(t *testing.T) {
	handle, _ := provisioning.NewExecutionHandle("equal-timestamp")
	provider := &staleSubmissionProvider{
		submits: map[uint64]provisioning.Submission{
			1: {Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationUnknown,
				Execution:  &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle},
				ObservedAt: testTime.Add(3 * time.Hour)}},
			2: {Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
				Execution:  &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle},
				Resource:   readyFacts(),
				ObservedAt: testTime.Add(4 * time.Hour)}},
		},
		observe: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationNotFound,
			Resource: unknownFacts(), ObservedAt: testTime.Add(4 * time.Hour)},
	}
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = 0
	command := createCommand(t, "resource-equal-submission", "operation-equal-submission")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		if _, err := instance.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if execution.LastFailure == nil || execution.LastFailure.Reason != "StaleSubmissionEvidence" {
		t.Fatalf("execution failure=%v, want StaleSubmissionEvidence", execution.LastFailure)
	}
	if execution.State == application.AttemptSucceeded {
		t.Fatalf("execution state=%s, want no terminal transition", execution.State)
	}
	operation, err := store.GetOperation(context.Background(), command.OperationID)
	if err != nil || operation.Operation.IsTerminal() {
		t.Fatalf("operation error=%v terminal=%t, want nonterminal", err, operation.Operation.IsTerminal())
	}
}

func TestStaleSubmissionRecoversThroughFreshObservation(t *testing.T) {
	handle, _ := provisioning.NewExecutionHandle("stale-recovery")
	provider := &staleThenAdvancingProvider{
		submits: map[uint64]provisioning.Submission{
			1: {Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationUnknown,
				Execution:  &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle},
				ObservedAt: testTime.Add(3 * time.Hour)}},
			2: {Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
				Execution:  &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle},
				Resource:   readyFacts(),
				ObservedAt: testTime.Add(2 * time.Hour)}},
		},
	}
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = 0
	command := createCommand(t, "resource-stale-recovery", "operation-stale-recovery")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 7 {
		if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
			t.Fatalf("RunOnce worked=%t error=%v", worked, err)
		}
	}
	operation, err := store.GetOperation(context.Background(), command.OperationID)
	if err != nil || operation.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("operation error=%v state=%s, want Succeeded after fresh observation", err, operation.Operation.State())
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.State != application.AttemptSucceeded {
		t.Fatalf("execution error=%v state=%s, want Succeeded", err, execution.State)
	}
}

func TestStaleTerminalObservationSchedulesFollowUpWithoutTransition(t *testing.T) {
	provider := &staleObserveProvider{
		submit: acceptedSubmission(testTime.Add(3 * time.Hour)),
		observes: []provisioning.ExecutionObservation{
			{Correlation: provisioning.RequestCorrelationFound,
				Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded},
				Resource:  readyFacts(), ObservedAt: testTime.Add(3 * time.Hour)},
		},
	}
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = 0
	command := createCommand(t, "resource-stale-terminal-observe", "operation-stale-terminal-observe")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
			t.Fatalf("RunOnce worked=%t error=%v", worked, err)
		}
	}
	message, err := store.GetOutbox(context.Background(), application.ObserveMessage(command.OperationID, 1, 0).ID)
	if err != nil || message.State != application.OutboxCompleted || message.TerminalReason != "StaleObservation" {
		t.Fatalf("observe message error=%v state=%s reason=%s", err, message.State, message.TerminalReason)
	}
	followUp, err := store.GetOutbox(context.Background(), application.ObserveMessage(command.OperationID, 2, 0).ID)
	if err != nil || followUp.State != application.OutboxPending {
		t.Fatalf("follow-up observe error=%v state=%s, want pending", err, followUp.State)
	}
	operation, err := store.GetOperation(context.Background(), command.OperationID)
	if err != nil || operation.Operation.IsTerminal() {
		t.Fatalf("operation error=%v terminal=%t, want nonterminal", err, operation.Operation.IsTerminal())
	}
}

func TestStaleFailedObservationSchedulesFollowUpWithoutFailure(t *testing.T) {
	provider := &staleObserveProvider{
		submit: acceptedSubmission(testTime.Add(3 * time.Hour)),
		observes: []provisioning.ExecutionObservation{
			{Correlation: provisioning.RequestCorrelationFound,
				Execution: &provisioning.Execution{State: provisioning.ExecutionStateFailed,
					Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureExecution, Reason: "ExecutionFailed", Message: "stale failure"}},
				ObservedAt: testTime.Add(3 * time.Hour)},
		},
	}
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = 0
	command := createCommand(t, "resource-stale-failed-observe", "operation-stale-failed-observe")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
			t.Fatalf("RunOnce worked=%t error=%v", worked, err)
		}
	}
	operation, err := store.GetOperation(context.Background(), command.OperationID)
	if err != nil || operation.Operation.State() == domain.OperationStateFailed {
		t.Fatalf("operation error=%v state=%s, want non-failed", err, operation.Operation.State())
	}
	if _, err := store.GetOutbox(context.Background(), application.ObserveMessage(command.OperationID, 2, 0).ID); err != nil {
		t.Fatalf("stale failed observation did not schedule a follow-up: %v", err)
	}
}

func TestFreshTerminalObservationCompletesAndStopsLoop(t *testing.T) {
	provider := &staleObserveProvider{
		submit: acceptedSubmission(testTime.Add(3 * time.Hour)),
		observes: []provisioning.ExecutionObservation{
			{Correlation: provisioning.RequestCorrelationFound,
				Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded},
				Resource:  readyFacts(), ObservedAt: testTime.Add(4 * time.Hour)},
		},
	}
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = 0
	command := createCommand(t, "resource-fresh-terminal", "operation-fresh-terminal")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
			t.Fatalf("RunOnce worked=%t error=%v", worked, err)
		}
	}
	operation, err := store.GetOperation(context.Background(), command.OperationID)
	if err != nil || operation.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("operation error=%v state=%s, want Succeeded", err, operation.Operation.State())
	}
	if worked, err := instance.RunOnce(context.Background()); err != nil || worked {
		t.Fatalf("terminal operation left work behind worked=%t error=%v", worked, err)
	}
}

func TestStaleObservationFollowUpUsesBoundedRetryDelay(t *testing.T) {
	provider := &staleObserveProvider{
		submit: acceptedSubmission(testTime.Add(3 * time.Hour)),
		observes: []provisioning.ExecutionObservation{
			{Correlation: provisioning.RequestCorrelationFound,
				Execution:  &provisioning.Execution{State: provisioning.ExecutionStateRunning},
				ObservedAt: testTime.Add(3 * time.Hour)},
		},
	}
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = 50 * time.Millisecond
	command := createCommand(t, "resource-bounded-stale", "operation-bounded-stale")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
			t.Fatalf("RunOnce worked=%t error=%v", worked, err)
		}
	}
	time.Sleep(70 * time.Millisecond)
	if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
		t.Fatalf("observe RunOnce worked=%t error=%v", worked, err)
	}
	followUp, err := store.GetOutbox(context.Background(), application.ObserveMessage(command.OperationID, 2, 0).ID)
	if err != nil || followUp.State != application.OutboxPending {
		t.Fatalf("follow-up observe error=%v state=%s, want pending", err, followUp.State)
	}
	if followUp.Delay != 50*time.Millisecond {
		t.Fatalf("follow-up delay=%v, want RetryBase", followUp.Delay)
	}
	if !followUp.AvailableAt.After(time.Now()) {
		t.Fatalf("follow-up AvailableAt=%v, want delayed beyond now", followUp.AvailableAt)
	}
	if worked, err := instance.RunOnce(context.Background()); err != nil || worked {
		t.Fatalf("delayed follow-up was claimed prematurely worked=%t error=%v", worked, err)
	}
}

func TestMalformedFactsOnNonterminalSubmissionAreAmbiguousNotQuarantined(t *testing.T) {
	handle, _ := provisioning.NewExecutionHandle("malformed-submission")
	provider := &staleSubmissionProvider{
		submits: map[uint64]provisioning.Submission{
			1: {Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
				Execution:  &provisioning.Execution{State: provisioning.ExecutionStateRunning, Handle: &handle},
				Resource:   invalidFacts(),
				ObservedAt: testTime.Add(time.Hour)}},
			2: {Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
				Execution:  &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle},
				Resource:   readyFacts(),
				ObservedAt: testTime.Add(3 * time.Hour)}},
		},
		observe: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationNotFound,
			Resource: unknownFacts(), ObservedAt: testTime.Add(2 * time.Hour)},
	}
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = 0
	command := createCommand(t, "resource-malformed-submission", "operation-malformed-submission")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 6 {
		if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
			t.Fatalf("RunOnce worked=%t error=%v", worked, err)
		}
	}
	attempt, err := store.GetSubmissionAttempt(context.Background(), command.OperationID, 1)
	if err != nil || attempt.State != application.SubmissionAttemptNotFound {
		t.Fatalf("attempt error=%v state=%s, want NotFound after observation", err, attempt.State)
	}
	if attempt.Failure == nil || attempt.Failure.Reason != "MalformedObservedFacts" {
		t.Fatalf("attempt failure=%v, want MalformedObservedFacts", attempt.Failure)
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.LastFailure == nil || execution.LastFailure.Reason != "MalformedObservedFacts" {
		t.Fatalf("execution error=%v failure=%v, want MalformedObservedFacts preserved", err, execution.LastFailure)
	}
	operation, err := store.GetOperation(context.Background(), command.OperationID)
	if err != nil || operation.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("operation error=%v state=%s, want Succeeded after clean resubmission", err, operation.Operation.State())
	}
}

func TestMalformedFactsOnTerminalSubmissionAreSanitized(t *testing.T) {
	handle, _ := provisioning.NewExecutionHandle("malformed-terminal")
	provider := &staleSubmissionProvider{
		submits: map[uint64]provisioning.Submission{
			1: {Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
				Execution:  &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle},
				Resource:   invalidFacts(),
				ObservedAt: testTime.Add(time.Hour)}},
		},
	}
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = 0
	command := createCommand(t, "resource-malformed-terminal", "operation-malformed-terminal")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
			t.Fatalf("RunOnce worked=%t error=%v", worked, err)
		}
	}
	operation, err := store.GetOperation(context.Background(), command.OperationID)
	if err != nil || operation.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("operation error=%v state=%s, want Succeeded with sanitized facts", err, operation.Operation.State())
	}
	message, err := store.GetOutbox(context.Background(), application.DispatchMessage(command.OperationID, 1, 0).ID)
	if err != nil || message.State != application.OutboxCompleted || message.TerminalReason != "TerminalExecution" {
		t.Fatalf("dispatch message error=%v state=%s reason=%s, want completed TerminalExecution", err, message.State, message.TerminalReason)
	}
}

func TestMalformedFactsOnNonterminalObservationRetryNotQuarantined(t *testing.T) {
	handle, _ := provisioning.NewExecutionHandle("malformed-observation")
	provider := &staleSubmissionProvider{
		submits: map[uint64]provisioning.Submission{
			1: {Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationUnknown,
				Execution:  &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle},
				ObservedAt: testTime.Add(time.Hour)}},
		},
		observe: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
			Execution: &provisioning.Execution{State: provisioning.ExecutionStateRunning, Handle: &handle},
			Resource:  invalidFacts(), ObservedAt: testTime.Add(2 * time.Hour)},
	}
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = 0
	command := createCommand(t, "resource-malformed-observation", "operation-malformed-observation")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
			t.Fatalf("RunOnce worked=%t error=%v", worked, err)
		}
	}
	if worked, err := instance.RunOnce(context.Background()); err == nil || !worked {
		t.Fatalf("malformed observation RunOnce worked=%t error=%v, want retryable error", worked, err)
	}
	message, err := store.GetOutbox(context.Background(), application.ObserveMessage(command.OperationID, 1, 0).ID)
	if err != nil || message.State != application.OutboxPending {
		t.Fatalf("observe message error=%v state=%s, want retried, not quarantined", err, message.State)
	}
	if message.LastError == "" || message.LastError == "malformed observed facts" {
		t.Fatalf("observe message LastError=%q, want malformed-facts detail", message.LastError)
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	if execution.LastObservation != nil && execution.LastObservation.ObservedAt.Equal(testTime.Add(2*time.Hour)) {
		t.Fatalf("execution LastObservation=%v, want malformed observation unrecorded", execution.LastObservation)
	}
}

func TestMalformedFactsOnTerminalObservationAreSanitized(t *testing.T) {
	handle, _ := provisioning.NewExecutionHandle("malformed-terminal-observation")
	provider := &staleSubmissionProvider{
		submits: map[uint64]provisioning.Submission{
			1: {Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationUnknown,
				Execution:  &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle},
				ObservedAt: testTime.Add(time.Hour)}},
		},
		observe: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
			Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle},
			Resource:  invalidFacts(), ObservedAt: testTime.Add(2 * time.Hour)},
	}
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = 0
	command := createCommand(t, "resource-malformed-terminal-observation", "operation-malformed-terminal-observation")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
			t.Fatalf("RunOnce worked=%t error=%v", worked, err)
		}
	}
	operation, err := store.GetOperation(context.Background(), command.OperationID)
	if err != nil || operation.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("operation error=%v state=%s, want Succeeded with sanitized facts", err, operation.Operation.State())
	}
	message, err := store.GetOutbox(context.Background(), application.ObserveMessage(command.OperationID, 1, 0).ID)
	if err != nil || message.State != application.OutboxCompleted || message.TerminalReason != "TerminalExecution" {
		t.Fatalf("observe message error=%v state=%s reason=%s, want completed TerminalExecution", err, message.State, message.TerminalReason)
	}
}
func TestWorkerLifecycleEventIDsUseCanonicalInternalLabels(t *testing.T) {
	handle, _ := provisioning.NewExecutionHandle("worker-event-ids")
	provider := &staleSubmissionProvider{
		submits: map[uint64]provisioning.Submission{
			1: {Observation: provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound,
				Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle},
				Resource:  readyFacts(), ObservedAt: testTime.Add(time.Hour)}},
		},
	}
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = 0
	command := createCommand(t, "resource-worker-event-ids", "operation-worker-event-ids")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
			t.Fatalf("RunOnce worked=%t error=%v", worked, err)
		}
	}
	operation, err := store.GetOperation(context.Background(), command.OperationID)
	if err != nil || operation.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("operation error=%v state=%s, want Succeeded", err, operation.Operation.State())
	}
	for _, phase := range []domain.OperationPhase{domain.OperationPhaseValidating, domain.OperationPhasePlanning, domain.OperationPhaseApplying} {
		if _, err := store.GetEvent(context.Background(), application.InternalEventID(command.OperationID, application.InternalTransitionLabel(phase))); err != nil {
			t.Fatalf("phase %s event missing: %v", phase, err)
		}
	}
	if _, err := store.GetEvent(context.Background(), application.InternalEventID(command.OperationID, "succeeded")); err != nil {
		t.Fatalf("succeeded event missing: %v", err)
	}
}

func TestStaleNotFoundObservationDoesNotResubmit(t *testing.T) {
	provider := &staleObserveProvider{
		submit: acceptedSubmission(testTime.Add(3 * time.Hour)),
		observes: []provisioning.ExecutionObservation{
			{Correlation: provisioning.RequestCorrelationNotFound,
				Resource: unknownFacts(), ObservedAt: testTime.Add(3 * time.Hour)},
		},
	}
	service, store, instance := newHarness(t, provider)
	instance.RetryBase = 0
	command := createCommand(t, "resource-stale-notfound", "operation-stale-notfound")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if worked, err := instance.RunOnce(context.Background()); err != nil || !worked {
			t.Fatalf("RunOnce worked=%t error=%v", worked, err)
		}
	}
	message, err := store.GetOutbox(context.Background(), application.ObserveMessage(command.OperationID, 1, 0).ID)
	if err != nil || message.State != application.OutboxCompleted || message.TerminalReason != "StaleObservation" {
		t.Fatalf("observe message error=%v state=%s reason=%s, want completed StaleObservation", err, message.State, message.TerminalReason)
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.State != application.AttemptAccepted {
		t.Fatalf("execution error=%v state=%s, want Accepted (no resubmission from stale NotFound)", err, execution.State)
	}
	if _, err := store.GetSubmissionAttempt(context.Background(), command.OperationID, 2); err == nil {
		t.Fatal("stale NotFound created a second submission attempt")
	}
	operation, err := store.GetOperation(context.Background(), command.OperationID)
	if err != nil || operation.Operation.IsTerminal() {
		t.Fatalf("operation error=%v terminal=%t, want nonterminal", err, operation.Operation.IsTerminal())
	}
}
