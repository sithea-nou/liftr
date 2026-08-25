// SPDX-License-Identifier: Apache-2.0

package worker_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	applicationfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
	"github.com/sithea-nou/liftr/internal/worker"
)

func provisioningfakeSynchronous() *provisioningfake.Provisioner {
	return provisioningfake.New(provisioningfake.ModeSynchronous)
}

func domainCapabilityCreate() domain.Capability {
	return domain.CapabilityCreate
}

// telemetrySinkStub records every worker telemetry event.
type telemetrySinkStub struct {
	mu        sync.Mutex
	work      []worker.WorkEvent
	terminals []worker.TerminalEvent
	panics    []string
}

func (s *telemetrySinkStub) WorkCompleted(event worker.WorkEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.work = append(s.work, event)
}

func (s *telemetrySinkStub) OperationTerminalized(event worker.TerminalEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.terminals = append(s.terminals, event)
}

func (s *telemetrySinkStub) WorkerPanic(kind string, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.panics = append(s.panics, kind+": "+value)
}

func (s *telemetrySinkStub) terminalCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.terminals)
}

func runUntilIdle(t *testing.T, instance *worker.Worker) {
	t.Helper()
	for range 64 {
		found, err := instance.RunOnce(context.Background())
		if err != nil && !errors.Is(err, worker.ErrRecoveredPanic) && !errors.Is(err, application.ErrConcurrencyConflict) {
			t.Fatalf("RunOnce error=%v", err)
		}
		if !found {
			return
		}
	}
	t.Fatal("worker did not drain within the bounded iteration budget")
}

// A real terminalization counts exactly once with its persisted duration.
func TestTerminalTransitionCountsExactlyOnce(t *testing.T) {
	sink := &telemetrySinkStub{}
	provider := provisioningfakeSynchronous()
	service, _, instance := newHarness(t, provider)
	instance.Telemetry = sink
	command := createCommand(t, "resource-terminal-count", "operation-terminal-count")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	runUntilIdle(t, instance)
	if sink.terminalCount() != 1 {
		t.Fatalf("terminal events=%d, want exactly 1", sink.terminalCount())
	}
	event := sink.terminals[0]
	if event.Capability != string(domainCapabilityCreate()) || event.TerminalState != "Succeeded" {
		t.Fatalf("event=%+v", event)
	}
	if event.DurationSeconds < 0 {
		t.Fatalf("duration must come from persisted timestamps, got %f", event.DurationSeconds)
	}
}

// A stale outbox item that finds an already-terminal Operation settles as
// stale and never increments terminal counters again.
func TestStaleReobserveOfTerminalOperationDoesNotCountAgain(t *testing.T) {
	sink := &telemetrySinkStub{}
	provider := provisioningfakeSynchronous()
	service, store, instance := newHarness(t, provider)
	instance.Telemetry = sink
	command := createCommand(t, "resource-stale-terminal", "operation-stale-terminal")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	runUntilIdle(t, instance)
	if sink.terminalCount() != 1 {
		t.Fatalf("terminal events=%d after success, want 1", sink.terminalCount())
	}

	// Re-enqueue an Observe for the now-terminal operation. Fencing/version
	// checks classify it stale; the disposition must not report a transition.
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	staleMessage := application.ObserveMessage(execution.OperationID, execution.NextObservation, execution.Version)
	if err := store.Enqueue(context.Background(), staleMessage); err != nil {
		t.Fatal(err)
	}
	runUntilIdle(t, instance)

	if sink.terminalCount() != 1 {
		t.Fatalf("stale re-observation changed terminal count to %d", sink.terminalCount())
	}
	staleSeen := false
	for _, event := range sink.work {
		if event.Outcome == worker.OutcomeStale {
			staleSeen = true
		}
	}
	if !staleSeen {
		t.Fatalf("expected a stale outcome among %+v", sink.work)
	}
}

// A panic inside one work item is recovered at the boundary: the loop
// survives, the lease stays intact for expiry recovery, and nothing is marked
// successful or failed.
func TestWorkerPanicKeepsLeaseAndReportsBoundary(t *testing.T) {
	sink := &telemetrySinkStub{}
	provider := provisioningfakeSynchronous()
	service, store, instance := newHarness(t, provider)
	instance.Telemetry = sink
	command := createCommand(t, "resource-panic-lease", "operation-panic-lease")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	// Within-call sequence for the first RunOnce after admission:
	// (1) expired-dispatch recovery pass, (2) claim, (3) drive handler
	// transaction. Panicking on the third call means the claim has committed,
	// so the message is durably Leased while its handler dies.
	panicRunner := &panickingTransactions{inner: store, panicOn: 3}
	instance.Transactions = panicRunner

	var recovered error
	found := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				recovered = errors.New("panic escaped RunOnce")
			}
		}()
		found, recovered = instance.RunOnce(context.Background())
	}()
	if !found || recovered == nil || !errors.Is(recovered, worker.ErrRecoveredPanic) {
		t.Fatalf("want (found, ErrRecoveredPanic), got (%t, %v)", found, recovered)
	}
	if len(sink.panics) == 0 {
		t.Fatal("telemetry sink was not told about the panic")
	}
	// The claimed message stays Leased: expiry recovery owns it.
	message, err := store.GetOutbox(context.Background(), "drive:"+string(command.OperationID)+":1")
	if err != nil {
		t.Fatal(err)
	}
	if message.State != application.OutboxLeased {
		t.Fatalf("message state=%s, want Leased so expiry recovery can reclaim it", message.State)
	}
	_ = service
}

type panickingTransactions struct {
	inner   application.TransactionRunner
	panicOn int
	calls   int
}

func (p *panickingTransactions) Within(ctx context.Context, fn func(application.UnitOfWork) error) error {
	p.calls++
	if p.calls == p.panicOn {
		panic("synthetic worker invariant failure")
	}
	return p.inner.Within(ctx, fn)
}

// panickyProvider converts one Submit into a panic, mimicking a defect in a
// provider integration mid-flight.
type panickyProvider struct {
	inner provisioning.Provisioner
	done  bool
	mu    sync.Mutex
}

func (p *panickyProvider) Capabilities() []provisioning.ProvisionerCapability {
	return p.inner.Capabilities()
}

func (p *panickyProvider) Submit(ctx context.Context, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	p.mu.Lock()
	already := p.done
	p.done = true
	p.mu.Unlock()
	if !already {
		panic("synthetic provider crash during submit")
	}
	return p.inner.Submit(ctx, request)
}

func (p *panickyProvider) Observe(ctx context.Context, request provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return p.inner.Observe(ctx, request)
}

// A panic inside Submit is ambiguity, never failure: the lease stays intact
// and expiry recovery routes the attempt through Unknown -> Observe.
func TestSubmitPanicBecomesAmbiguousAndKeepsLease(t *testing.T) {
	sink := &telemetrySinkStub{}
	service, store, instance := newHarness(t, provisioningfakeSynchronous())
	wrapped := &panickyProvider{inner: provisioningfakeSynchronous()}
	ref, refErr := application.NewProvisionerRef("test-provider")
	if refErr != nil {
		t.Fatal(refErr)
	}
	resolver := &applicationfake.Resolver{Providers: map[application.ProvisionerRef]provisioning.Provisioner{ref: wrapped}}
	replacement, err := worker.New(store, resolver)
	if err != nil {
		t.Fatal(err)
	}
	replacement.Clock = instance.Clock
	replacement.Telemetry = sink
	_ = service

	command := createCommand(t, "resource-submit-panic", "operation-submit-panic")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	// Drive phases with the ORIGINAL worker; dispatch with the panicking one.
	for range 3 {
		if _, err := instance.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	_, err = replacement.RunOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "panicked") {
		t.Fatalf("want surfaced ambiguity mentioning the panic, got %v", err)
	}
	attempt, attemptErr := store.GetSubmissionAttempt(context.Background(), command.OperationID, 1)
	if attemptErr != nil || attempt.State != application.SubmissionAttemptLeased {
		t.Fatalf("attempt error=%v state=%s, want Leased", attemptErr, attempt.State)
	}
	message, messageErr := store.GetOutbox(context.Background(), "dispatch:"+string(command.OperationID)+":1")
	if messageErr != nil || message.State != application.OutboxLeased {
		t.Fatalf("dispatch message error=%v state=%s, want Leased", messageErr, message.State)
	}
	if sink.terminalCount() != 0 {
		t.Fatalf("terminal events=%d after ambiguous panic, want 0", sink.terminalCount())
	}
	// Ambiguity with the lease retained must be reported as "ambiguous",
	// never as lease_lost (ADR-0018).
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, event := range sink.work {
		if event.Outcome == worker.OutcomeLeaseLos {
			t.Fatalf("retained-lease ambiguity mislabeled lease_lost: %+v", event)
		}
		if event.Outcome == worker.OutcomeAmbiguous && event.ErrorClass != "provisioner_submission_ambiguous" {
			t.Fatalf("ambiguous event has wrong class: %+v", event)
		}
	}
	foundAmbiguous := false
	for _, event := range sink.work {
		if event.Outcome == worker.OutcomeAmbiguous {
			foundAmbiguous = true
		}
	}
	if !foundAmbiguous {
		t.Fatalf("no ambiguous outcome among %+v", sink.work)
	}
}

// renewFailingTransactions fails every RenewOutbox after the first call, so
// the dispatch heartbeat provably loses fenced ownership.
type renewFailingTransactions struct {
	inner application.TransactionRunner
	mu    sync.Mutex
	calls int
}

func (r *renewFailingTransactions) Within(ctx context.Context, fn func(application.UnitOfWork) error) error {
	return r.inner.Within(ctx, func(tx application.UnitOfWork) error {
		return fn(&renewFailingUnitOfWork{UnitOfWork: tx, parent: r})
	})
}

type renewFailingUnitOfWork struct {
	application.UnitOfWork
	parent *renewFailingTransactions
}

func (u *renewFailingUnitOfWork) Outbox() application.OutboxRepository {
	return &renewFailingOutbox{OutboxRepository: u.UnitOfWork.Outbox(), parent: u.parent}
}

type renewFailingOutbox struct {
	application.OutboxRepository
	parent *renewFailingTransactions
}

func (o *renewFailingOutbox) RenewOutbox(ctx context.Context, id, token string, lease time.Duration) error {
	o.parent.mu.Lock()
	o.parent.calls++
	call := o.parent.calls
	o.parent.mu.Unlock()
	if call > 1 {
		return errors.New("synthetic fencing loss: renewal rejected")
	}
	return o.OutboxRepository.RenewOutbox(ctx, id, token, lease)
}

// A genuinely lost lease reports lease_lost — a different diagnosis from
// submission ambiguity.
func TestHeartbeatLossReportsLeaseLostNotAmbiguous(t *testing.T) {
	sink := &telemetrySinkStub{}
	service, store, instance := newHarness(t, provisioningfakeSynchronous())
	instance.Lease = time.Millisecond
	instance.Telemetry = sink
	wrapped := &renewFailingTransactions{inner: store}
	replacement, err := worker.New(wrapped, mustResolverOf(t, store, instance))
	if err != nil {
		t.Fatal(err)
	}
	replacement.Lease = time.Millisecond
	replacement.RetryBase = time.Millisecond
	replacement.Clock = instance.Clock
	replacement.Telemetry = sink

	command := createCommand(t, "resource-lease-lost", "operation-lease-lost")
	if _, err := service.AdmitCreateResource(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	sawLeaseLost := false
	for time.Now().Before(deadline) && !sawLeaseLost {
		_, _ = replacement.RunOnce(context.Background())
		sink.mu.Lock()
		for _, event := range sink.work {
			if event.Outcome == worker.OutcomeLeaseLos {
				sawLeaseLost = true
			}
			if event.Outcome == worker.OutcomeLeaseLos && event.ErrorClass != "lease_lost" {
				t.Fatalf("lease_lost event has wrong class: %+v", event)
			}
		}
		sink.mu.Unlock()
	}
	if !sawLeaseLost {
		t.Fatal("genuine fencing loss was never reported as lease_lost")
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, event := range sink.work {
		if event.ErrorClass == "provisioner_submission_ambiguous" {
			t.Fatalf("fencing loss mislabeled as provider ambiguity: %+v", event)
		}
	}
}

func mustResolverOf(t *testing.T, _ *applicationfake.Store, instance *worker.Worker) application.ProvisionerResolver {
	t.Helper()
	return instance.Resolver
}
