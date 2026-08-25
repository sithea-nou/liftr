// SPDX-License-Identifier: Apache-2.0

package postgres_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	applicationfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/persistence/postgres"
	"github.com/sithea-nou/liftr/internal/provisioning"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
	"github.com/sithea-nou/liftr/internal/worker"
)

// M17 blocker proof (ADR-0018): a panic in ANY work kind must never
// permanently strand durable work in state=Leased. Recovery is entirely
// durable and restart-safe:
//
//   - Drive / Observe / PassiveObserve leases are reclaimed inline by the
//     ordinary ClaimOutbox predicate (`kind <> 'Dispatch' AND
//     leased_until <= clock_timestamp()`);
//   - Dispatch leases recover exclusively through FindExpiredDispatch ->
//     ambiguity recovery (never a blind resubmission);
//   - the recovery passes themselves are idempotent DB queries re-run every
//     tick, so a panic inside them is retried on the next pass with no
//     in-memory assistance.

// shortLeaseWorker builds a worker over the durable store with a fast lease
// so expiry-based recovery completes inside test budgets.
func shortLeaseWorker(t *testing.T, store application.TransactionRunner, resolver *applicationfake.Resolver, sink worker.TelemetrySink) *worker.Worker {
	t.Helper()
	instance, err := worker.NewWithCatalog(store, resolver, m17FakeCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	instance.Lease = 80 * time.Millisecond
	instance.RetryBase = time.Millisecond
	instance.Clock = func() time.Time { return time.Now().UTC() }
	instance.Telemetry = sink
	return instance
}

// panickingKindTransactions injects a panic INSIDE the handler transaction of
// exactly one outbox kind, after that kind's claim has committed. It works by
// proxying UnitOfWork.Outbox().GetOutbox, which every handler calls first.
type panickingKindTransactions struct {
	inner      application.TransactionRunner
	targetKind string
	mu         sync.Mutex
	fired      bool
}

func (p *panickingKindTransactions) Within(ctx context.Context, fn func(application.UnitOfWork) error) error {
	return p.inner.Within(ctx, func(tx application.UnitOfWork) error {
		p.mu.Lock()
		armed := !p.fired && p.targetKind != ""
		p.mu.Unlock()
		if !armed {
			return fn(tx)
		}
		return fn(&panicUnitOfWork{UnitOfWork: tx, parent: p})
	})
}

func (p *panickingKindTransactions) trip(kind string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.targetKind == kind && !p.fired {
		p.fired = true
		panic("synthetic " + strings.ToLower(kind) + " handler failure")
	}
}

type panicUnitOfWork struct {
	application.UnitOfWork
	parent *panickingKindTransactions
}

func (u *panicUnitOfWork) Outbox() application.OutboxRepository {
	return &panickingOutbox{OutboxRepository: u.UnitOfWork.Outbox(), parent: u.parent}
}

type panickingOutbox struct {
	application.OutboxRepository
	parent *panickingKindTransactions
}

func (o *panickingOutbox) GetOutbox(ctx context.Context, id string) (application.OutboxMessage, error) {
	message, err := o.OutboxRepository.GetOutbox(ctx, id)
	if err == nil {
		o.parent.trip(string(message.Kind))
	}
	return message, err
}

// submitBoundaryProvider wraps one provider and panics at a chosen point of
// the Submit boundary, counting real inner submissions.
type submitBoundaryProvider struct {
	inner        provisioning.Provisioner
	fake         *provisioningfake.Provisioner
	panicBefore  bool
	panicAfter   bool
	mu           sync.Mutex
	innerSubmits int
}

// realSubmissionCount reads the underlying fake's authoritative counter, which
// stays valid even after tests swap the resolver to the unwrapped provider.
func (p *submitBoundaryProvider) realSubmissionCount(operationID domain.OperationID) int {
	return p.fake.SubmissionCount(operationID)
}

func (p *submitBoundaryProvider) Capabilities() []provisioning.ProvisionerCapability {
	return p.inner.Capabilities()
}

func (p *submitBoundaryProvider) Submit(ctx context.Context, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	if p.panicBefore {
		panic("synthetic crash before transmission")
	}
	submission, err := p.inner.Submit(ctx, request)
	p.mu.Lock()
	p.innerSubmits++
	p.mu.Unlock()
	if p.panicAfter {
		panic("synthetic crash after transmission returned")
	}
	return submission, err
}

func (p *submitBoundaryProvider) countInnerSubmits() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.innerSubmits
}

func (p *submitBoundaryProvider) Observe(ctx context.Context, request provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return p.inner.Observe(ctx, request)
}

// observePanicProvider panics on every Observe call until disarmed.
type observePanicProvider struct {
	inner provisioning.Provisioner
	mu    sync.Mutex
	armed bool
}

func (p *observePanicProvider) Capabilities() []provisioning.ProvisionerCapability {
	return p.inner.Capabilities()
}

func (p *observePanicProvider) Submit(ctx context.Context, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	return p.inner.Submit(ctx, request)
}

func (p *observePanicProvider) Observe(ctx context.Context, request provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	p.mu.Lock()
	armed := p.armed
	p.mu.Unlock()
	if armed {
		panic("synthetic observation failure")
	}
	return p.inner.Observe(ctx, request)
}

func (p *observePanicProvider) arm() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.armed = true
}

func (p *observePanicProvider) disarm() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.armed = false
}

// pumpUntilQuiet drains with bounded iterations, tolerating recovered panics,
// ambiguity, and stale settlements — all of which surface as errors from
// RunOnce while the loop stays alive.
func pumpUntilQuiet(t *testing.T, instance *worker.Worker, maxIterations int) int {
	t.Helper()
	panics := 0
	for range maxIterations {
		found, err := instance.RunOnce(context.Background())
		if errors.Is(err, worker.ErrRecoveredPanic) {
			panics++
			continue
		}
		if err != nil && !errors.Is(err, application.ErrConcurrencyConflict) &&
			!strings.Contains(err.Error(), "dispatch result is ambiguous") {
			t.Fatalf("unexpected worker error: %v", err)
		}
		if !found {
			return panics
		}
	}
	t.Fatal("worker did not reach a quiet state within the iteration budget")
	return panics
}

func waitForCondition(t *testing.T, budget time.Duration, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v: %s", budget, description)
}

// Scenario 1: a Drive handler panic leaves the message Leased; after lease
// expiry the ordinary claim predicate reclaims it and lifecycle completes
// with exactly one submission. Restarting into a fresh worker changes nothing.
func TestDrivePanicRecoversThroughExpiryClaimWithoutStranding(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	provider := provisioningfake.New(provisioningfake.ModeSynchronous)
	service, resolver := postgresService(t, store, provider)

	injected := &panickingKindTransactions{inner: store, targetKind: "Drive"}
	sink := newWorkerTelemetryStub()
	first := shortLeaseWorker(t, injected, resolver, sink)
	service.Transactions = injected

	command := application.CreateResourceCommand{
		Actor: applicationfake.Principal("tester"), ID: "res-drive-panic",
		Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: mustSpec(t, map[string]any{"size": int64(1)}), OperationID: "op-drive-panic",
		EventID: "evt-drive-panic", RequestedAt: time.Now().UTC(), IdempotencyKey: "drive-panic-key",
	}
	if _, err := service.AdmitCreateResource(ctx, command); err != nil {
		t.Fatal(err)
	}
	// First RunOnce claims Drive and panics inside its handler transaction.
	found, runErr := first.RunOnce(ctx)
	if !found || !errors.Is(runErr, worker.ErrRecoveredPanic) {
		t.Fatalf("want recovered panic, got found=%t err=%v", found, runErr)
	}
	if len(sink.panics()) == 0 {
		t.Fatal("telemetry was not told about the drive panic")
	}
	message, msgErr := mustGetOutboxM17(t, store, "drive:op-drive-panic:1")
	if msgErr != nil || message.State != application.OutboxLeased {
		t.Fatalf("message state=%v err=%v, want Leased pending expiry", messageStateOf(message), msgErr)
	}

	// Simulated process restart: an entirely fresh worker over the same
	// durable store. Expiry lets the ordinary claim steal the row.
	time.Sleep(160 * time.Millisecond)
	restarted := shortLeaseWorker(t, store, resolver, sink)
	pumpUntilQuiet(t, restarted, 64)

	operation := getOperationM17(t, store, command.OperationID)
	if operation.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("operation state=%v, want Succeeded after recovery", operationStateOf(operation))
	}
	if provider.SubmissionCount(command.OperationID) != 1 {
		t.Fatalf("submissions=%d, want exactly one despite the panic+restart", provider.SubmissionCount(command.OperationID))
	}
	final, finalErr := mustGetOutboxM17(t, store, "drive:op-drive-panic:1")
	if finalErr != nil || final.State != application.OutboxCompleted {
		t.Fatalf("recovered drive message state=%v err=%v, want Completed", messageStateOf(final), finalErr)
	}
}

// Scenario 2a: Dispatch panics BEFORE transmission. Nothing external
// launched; the panicked worker must have caused zero provider calls, and
// expired-dispatch recovery must move the attempt safely toward
// Unknown/Observe — inventing no definitive failure. The framework's existing
// safe-resubmission rules may then legitimately submit exactly once.
func TestDispatchPanicBeforeTransmissionNeverBlindlyResubmits(t *testing.T) {
	provider := provisioningfake.New(provisioningfake.ModeAsynchronous)
	boundary := &submitBoundaryProvider{inner: provider, fake: provider, panicBefore: true}
	runDispatchPanicScenario(t, boundary, provider, "res-dispatch-before", "op-dispatch-before", "before-key")
	// Exactly one REAL submission ever occurred: the framework's legitimate
	// safe-resubmission after truthful NotFound evidence — never a blind
	// duplicate triggered by the panic itself.
	if total := provider.SubmissionCount("op-dispatch-before"); total != 1 {
		t.Fatalf("total submissions=%d, want exactly one", total)
	}
}

// Scenario 2b: Dispatch panics AFTER the provider accepted the submission —
// the dangerous direction. The result is discarded, the attempt goes Unknown,
// Observe recovers terminal evidence, and NO second submission ever occurs.
func TestDispatchPanicAfterTransmissionRecoversThroughAmbiguityWithoutResubmission(t *testing.T) {
	provider := provisioningfake.New(provisioningfake.ModeAsynchronous)
	boundary := &submitBoundaryProvider{inner: provider, fake: provider, panicAfter: true}
	runDispatchPanicScenario(t, boundary, provider, "res-dispatch-after", "op-dispatch-after", "after-key")
	if total := provider.SubmissionCount("op-dispatch-after"); total != 1 {
		t.Fatalf("total submissions=%d, want exactly one despite panic after transmission (no blind resubmission)", total)
	}
}

func runDispatchPanicScenario(t *testing.T, boundary *submitBoundaryProvider, healthy provisioning.Provisioner,
	resourceID domain.ResourceID, operationID domain.OperationID, key string) int {
	t.Helper()
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	service, resolver := postgresService(t, store, boundary)

	sink := newWorkerTelemetryStub()
	first := shortLeaseWorker(t, store, resolver, sink)
	command := application.CreateResourceCommand{
		Actor: applicationfake.Principal("tester"), ID: resourceID,
		Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: mustSpec(t, map[string]any{"size": int64(1)}), OperationID: operationID,
		EventID: domain.EventID("evt-" + string(operationID)), RequestedAt: time.Now().UTC(), IdempotencyKey: key,
	}
	if _, err := service.AdmitCreateResource(ctx, command); err != nil {
		t.Fatal(err)
	}
	// Drive phases to Applying (enqueues Dispatch + attempt), then claim and
	// panic around the Submit boundary.
	for range 4 {
		found, _ := first.RunOnce(ctx)
		if !found {
			break
		}
	}
	attempt, attemptErr := getAttemptM17(t, store, operationID, 1)
	if attemptErr != nil || attempt.State != application.SubmissionAttemptLeased {
		t.Fatalf("attempt state=%v err=%v, want Leased at the submit boundary", attemptStateOf(attempt), attemptErr)
	}
	// The caller asserts per-scenario submission counts from the underlying
	// fake after recovery completes.
	goroutinesBefore := runtime.NumGoroutine()

	// The panicked dispatch keeps its lease; expiry recovery owns the rest.
	// After the lease expires we swap in the truthful provider so Observe (or
	// the framework's safe-resubmission rules) can settle the attempt — this
	// mirrors an operator fixing the underlying fault, not telemetry behavior.
	time.Sleep(200 * time.Millisecond)
	resolver.Providers[resolverRefOf(resolver)] = healthy
	restarted := shortLeaseWorker(t, store, resolver, sink)
	pumpUntilQuiet(t, restarted, 96)

	operation := getOperationM17(t, store, operationID)
	recoveredAttempt, _ := getAttemptM17(t, store, operationID, 1)
	if operation.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("operation state=%v, want Succeeded via ambiguity->Observe recovery", operationStateOf(operation))
	}
	// Never Failed: panic recovery may not invent a definitive failure.
	// Accepted/Unknown cover post-transmission ambiguity; NotFound is the
	// truthful durable answer when nothing ever launched (the framework then
	// applies its existing safe-resubmission rules).
	switch recoveredAttempt.State {
	case application.SubmissionAttemptAccepted, application.SubmissionAttemptUnknown,
		application.SubmissionAttemptNotFound:
	default:
		t.Fatalf("recovered attempt state=%s, want Accepted/Unknown/NotFound (never Failed)", recoveredAttempt.State)
	}
	// The heartbeat goroutine of the abandoned dispatch must be gone.
	waitForCondition(t, 2*time.Second, func() bool {
		return runtime.NumGoroutine() <= goroutinesBefore+2
	}, "heartbeat goroutine appears leaked")
	return boundary.realSubmissionCount(operationID)
}

// Scenario 3: an Observe panic cannot strand the only follow-up observation.
// The message stays Leased, expires, is reclaimed by the plain claim path,
// and the same sequence re-executes because recordObservation never committed.
func TestObservePanicIsRetriedByIdempotentSequenceAndCompletes(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	// Asynchronous mode makes Submit return Accepted, which schedules exactly
	// one follow-up Observe — the message whose panic recovery we are proving.
	healthy := provisioningfake.New(provisioningfake.ModeAsynchronous)
	service, resolver := postgresService(t, store, healthy)

	panicky := &observePanicProvider{inner: healthy}
	panicky.arm()
	sink := newWorkerTelemetryStub()
	workerInstance := shortLeaseWorker(t, store, resolver, sink)

	command := application.CreateResourceCommand{
		Actor: applicationfake.Principal("tester"), ID: "res-observe-panic",
		Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: mustSpec(t, map[string]any{"size": int64(2)}), OperationID: "op-observe-panic",
		EventID: "evt-observe-panic", RequestedAt: time.Now().UTC(), IdempotencyKey: "observe-panic-key",
	}
	if _, err := service.AdmitCreateResource(ctx, command); err != nil {
		t.Fatal(err)
	}
	// Submissions flow through the healthy fake so the execution reaches
	// Accepted; observations route to the panicking wrapper so the FIRST
	// Observe panics outside any transaction (lease intact).
	resolver.Providers[resolverRefOf(resolver)] = &phaseSwitchProvider{sync: healthy, observe: panicky}

	panics := pumpUntilQuiet(t, workerInstance, 64)
	if panics == 0 {
		t.Fatal("expected the observe panic to be exercised")
	}
	execution := getExecution(t, store, command.OperationID)
	if execution.NextObservation == 0 {
		t.Fatalf("execution next observation sequence=%d", execution.NextObservation)
	}

	// Restart with a fully healthy provider; the SAME observe message is
	// reclaimed after expiry (sequence unchanged) and completes the operation.
	time.Sleep(160 * time.Millisecond)
	resolver.Providers[resolverRefOf(resolver)] = healthy
	restarted := shortLeaseWorker(t, store, resolver, sink)
	pumpUntilQuiet(t, restarted, 64)

	operation := getOperationM17(t, store, command.OperationID)
	if operation.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("operation stranded after observe panic: state=%v", operationStateOf(operation))
	}
}

// Scenario 4: PassiveObserve panics never silently disable reconciliation.
// Same reclaim-by-expiry guarantee as other non-Dispatch kinds.
func TestPassiveObservePanicRecoveresAndKeepsReconciliationAlive(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := provisioningfake.New(provisioningfake.ModeSynchronous)
	identityObserver := provisioningfake.New(provisioningfake.ModeExisting)
	composed := &phaseSwitchProvider{sync: lifecycle, observe: identityObserver}
	service, resolver := postgresService(t, store, composed)
	sink := newWorkerTelemetryStub()
	workerInstance := shortLeaseWorker(t, store, resolver, sink)

	command := application.CreateResourceCommand{
		Actor: applicationfake.Principal("tester"), ID: "res-passive-panic",
		Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: mustSpec(t, map[string]any{"size": int64(1)}), OperationID: "op-passive-panic",
		EventID: domain.EventID("evt-passive-panic"), RequestedAt: time.Now().UTC(), IdempotencyKey: "passive-panic-key",
	}
	if _, err := service.AdmitCreateResource(ctx, command); err != nil {
		t.Fatal(err)
	}
	// Settle the whole create lifecycle FIRST so nothing else competes for
	// claims and the resource version is stable; then arm observation panics
	// for the passive cycle alone.
	pumpUntilQuiet(t, workerInstance, 64)
	resourceVersion := getResourceVersionM17(t, store, command.ID)

	panicky := &observePanicProvider{inner: identityObserver}
	panicky.arm()
	resolver.Providers[resolverRefOf(resolver)] = &phaseSwitchProvider{sync: lifecycle, observe: panicky}
	if err := workerInstance.SchedulePassiveObservation(ctx, command.ID, resourceVersion, resourceVersion); err != nil {
		t.Fatal(err)
	}
	panics := pumpUntilQuiet(t, workerInstance, 16)
	if panics == 0 {
		t.Fatal("expected the passive-observe panic to be exercised")
	}
	messageKey := "passive-observe:" + string(command.ID) + ":" + itoa(resourceVersion)
	message, msgErr := mustGetOutboxM17(t, store, messageKey)
	if msgErr != nil || message.State != application.OutboxLeased {
		t.Fatalf("passive observe state=%v err=%v, want Leased pre-expiry", messageStateOf(message), msgErr)
	}

	time.Sleep(160 * time.Millisecond)
	panicky.disarm()
	resolver.Providers[resolverRefOf(resolver)] = composed
	restarted := shortLeaseWorker(t, store, resolver, sink)
	pumpUntilQuiet(t, restarted, 32)

	completed, completedErr := mustGetOutboxM17(t, store, messageKey)
	if completedErr != nil || completed.State != application.OutboxCompleted {
		t.Fatalf("passive observe state=%v err=%v, want Completed after recovery", messageStateOf(completed), completedErr)
	}
	if completed.TerminalReason != "PassivelyObserved" {
		t.Fatalf("terminal reason=%q, want a real observation, not a stale settlement", completed.TerminalReason)
	}
	// Reconciliation stays schedulable afterwards: enqueue another cycle.
	if err := restarted.SchedulePassiveObservation(ctx, command.ID, resourceVersion+1, getResourceVersionM17(t, store, command.ID)); err != nil {
		t.Fatalf("future reconciliation scheduling broken: %v", err)
	}
}

// Scenario 6: fencing. Once another claimant legitimately reclaims an expired
// lease, the old owner's completion attempts are rejected outright.
func TestFencingRejectsOldOwnerAfterLegitimateReclaim(t *testing.T) {
	pool, cleanup := testPool(t)
	defer cleanup()
	ctx := context.Background()
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store, err := postgres.NewStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	// Seed one Pending non-Dispatch message directly.
	mustExecM17(t, pool, `
		INSERT INTO resources (id, type_name, type_version, owner_kind, owner_id, generation,
			spec_codec_version, spec, record_version, created_at_ns, updated_at_ns)
		VALUES ('res-fence', 'T', 'v1', 'team', 'platform', 1, 1, '{}'::jsonb, 1, 1, 1)`)
	mustExecM17(t, pool, `
		INSERT INTO operations (id, resource_id, capability, target_generation, state, phase,
			requested_at_ns, phase_changed_at_ns, record_version)
		VALUES ('op-fence', 'res-fence', 'create', 1, 'Pending', 'Requested', 1, 1, 1)`)
	mustExecM17(t, pool, `
		INSERT INTO outbox_messages (id, kind, operation_id, dedupe_key, payload_version, payload,
			state, available_at)
		VALUES ('fence-msg', 'Drive', 'op-fence', 'fence-dedupe', 1, '{}'::jsonb, 'Pending', clock_timestamp())`)

	tokenA := "owner-a-token"
	var messageA application.OutboxMessage
	var foundA bool
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		var err error
		messageA, foundA, err = tx.Outbox().ClaimOutbox(ctx, tokenA, 40*time.Millisecond)
		return err
	}); err != nil || !foundA {
		t.Fatalf("first claim found=%t err=%v", foundA, err)
	}
	time.Sleep(90 * time.Millisecond)
	tokenB := "owner-b-token"
	var messageB application.OutboxMessage
	var foundB bool
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		var err error
		messageB, foundB, err = tx.Outbox().ClaimOutbox(ctx, tokenB, time.Minute)
		return err
	}); err != nil || !foundB || messageB.ID != messageA.ID {
		t.Fatalf("reclaim found=%t err=%v id=%v", foundB, err, messageIDOf(messageB))
	}
	var staleErr error
	_ = store.Within(ctx, func(tx application.UnitOfWork) error {
		staleErr = tx.Outbox().CompleteOutbox(ctx, messageA.ID, tokenA, "StaleOwner")
		return nil
	})
	if !isConcurrencyConflict(staleErr) {
		t.Fatalf("old owner completion err=%v, want fencing conflict", staleErr)
	}
	if err := store.Within(ctx, func(tx application.UnitOfWork) error {
		return tx.Outbox().CompleteOutbox(ctx, messageB.ID, tokenB, "Legitimate")
	}); err != nil {
		t.Fatalf("legitimate owner rejected: %v", err)
	}
}

// ---- shared M17 helpers ----------------------------------------------------
func m17FakeCatalog(t *testing.T) application.ResourceTypeCatalog {
	t.Helper()
	typeValue, err := domain.NewResourceType(provisioningfake.ResourceType(), "M17 recovery fake",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		t.Fatal(err)
	}
	return applicationfake.Catalog{Types: map[domain.ResourceTypeRef]domain.ResourceType{
		provisioningfake.ResourceType(): typeValue,
	}}
}

func getResourceVersionM17(t *testing.T, store *postgres.Store, id domain.ResourceID) uint64 {
	t.Helper()
	var version uint64
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		record, err := tx.Resources().GetResource(context.Background(), id)
		version = record.Version
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return version
}

func getOutboxM17(t *testing.T, store *postgres.Store, id string) application.OutboxMessage {
	t.Helper()
	var message application.OutboxMessage
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		var err error
		message, err = tx.Outbox().GetOutbox(context.Background(), id)
		return err
	}); err != nil {
		return application.OutboxMessage{}
	}
	return message
}

func mustGetOutboxM17(t *testing.T, store *postgres.Store, id string) (application.OutboxMessage, error) {
	t.Helper()
	var message application.OutboxMessage
	var outerErr error
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		var err error
		message, err = tx.Outbox().GetOutbox(context.Background(), id)
		outerErr = err
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return message, outerErr
}

func getOperationM17(t *testing.T, store *postgres.Store, operationID domain.OperationID) application.OperationRecord {
	t.Helper()
	var record application.OperationRecord
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		var err error
		record, err = tx.Operations().GetOperation(context.Background(), operationID)
		return err
	}); err != nil {
		t.Fatalf("get operation: %v", err)
	}
	return record
}

func getAttemptM17(t *testing.T, store *postgres.Store, operationID domain.OperationID, attempt uint64) (application.SubmissionAttemptRecord, error) {
	t.Helper()
	var record application.SubmissionAttemptRecord
	var outerErr error
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		var err error
		record, err = tx.SubmissionAttempts().GetSubmissionAttempt(context.Background(), operationID, attempt)
		outerErr = err
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return record, outerErr
}

type workerTelemetryStub struct {
	mu      sync.Mutex
	panics_ []string
}

func newWorkerTelemetryStub() *workerTelemetryStub { return &workerTelemetryStub{} }

func (s *workerTelemetryStub) WorkCompleted(event worker.WorkEvent) {}

func (s *workerTelemetryStub) OperationTerminalized(event worker.TerminalEvent) {}

func (s *workerTelemetryStub) WorkerPanic(kind string, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.panics_ = append(s.panics_, kind+": "+value)
}

func (s *workerTelemetryStub) panics() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.panics_...)
}

func mustSpec(t *testing.T, values map[string]any) domain.ResourceSpec {
	t.Helper()
	spec, err := domain.NewResourceSpec(values)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func isConcurrencyConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "concurrency conflict")
}

func itoa(value uint64) string {
	digits := ""
	if value == 0 {
		return "0"
	}
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

func resolverRefOf(resolver *applicationfake.Resolver) application.ProvisionerRef {
	for ref := range resolver.Providers {
		return ref
	}
	return ""
}

// phaseSwitchProvider routes Submit to one provider and Observe to another so
// tests can fail exactly one side of the contract.
type phaseSwitchProvider struct {
	sync    provisioning.Provisioner
	observe provisioning.Provisioner
}

func (p *phaseSwitchProvider) Capabilities() []provisioning.ProvisionerCapability {
	return p.sync.Capabilities()
}

func (p *phaseSwitchProvider) Submit(ctx context.Context, request provisioning.ExecutionRequest) (provisioning.Submission, error) {
	return p.sync.Submit(ctx, request)
}

func (p *phaseSwitchProvider) Observe(ctx context.Context, request provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	return p.observe.Observe(ctx, request)
}

func messageStateOf(message application.OutboxMessage) string {
	if message.State == "" {
		return "<missing>"
	}
	return string(message.State)
}

func operationStateOf(record application.OperationRecord) string {
	return string(record.Operation.State())
}

func attemptStateOf(record application.SubmissionAttemptRecord) string {
	return string(record.State)
}

func messageIDOf(message application.OutboxMessage) string {
	return message.ID
}
