// SPDX-License-Identifier: Apache-2.0

package application_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/lifecycle"
	"github.com/sithea-nou/liftr/internal/provisioning"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
)

var applicationTime = time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

func TestCreateResourceUsesRealLifecycleAndSynchronousProvisioning(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-a")
	provider := provisioningfake.New(provisioningfake.ModeSynchronous)
	service, store, selector, resolver := newService(t, ref, provider)

	result, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-1", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-1", EventID: "event-1", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	if result.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("operation state = %s, want Succeeded", result.Operation.State())
	}
	if result.Resource.Status.State() != domain.ResourceStateReady {
		t.Fatalf("resource state = %s, want Ready", result.Resource.Status.State())
	}
	if selector.Calls != 1 {
		t.Fatalf("selector calls = %d, want 1", selector.Calls)
	}
	if resolver.Calls[ref] == 0 {
		t.Fatal("selected provisioner was not resolved")
	}
	if provider.SubmissionCount("operation-1") != 1 {
		t.Fatalf("submission count = %d, want 1", provider.SubmissionCount("operation-1"))
	}
	if _, err := store.GetResource(context.Background(), "resource-1"); err != nil {
		t.Fatalf("stored resource lookup error = %v", err)
	}
}

func TestAsynchronousObservationUsesProvisionerBinding(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-async")
	provider := provisioningfake.New(provisioningfake.ModeAsynchronous)
	service, _, _, _ := newService(t, ref, provider)

	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-async", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-async", EventID: "event-async", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	if created.Operation.State() != domain.OperationStateRunning {
		t.Fatalf("operation state = %s, want Running", created.Operation.State())
	}

	running, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{OperationID: "operation-async", ObservedAt: applicationTime.Add(time.Minute)})
	if err != nil {
		t.Fatalf("first ObserveOperation() error = %v", err)
	}
	if running.Operation.State() != domain.OperationStateRunning {
		t.Fatalf("running operation state = %s, want Running", running.Operation.State())
	}

	succeeded, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{OperationID: "operation-async", ObservedAt: applicationTime.Add(2 * time.Minute)})
	if err != nil {
		t.Fatalf("second ObserveOperation() error = %v", err)
	}
	if succeeded.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("completed operation state = %s, want Succeeded", succeeded.Operation.State())
	}
}

func TestExistingResourceUsesStableBindingWhenSelectorChanges(t *testing.T) {
	firstRef := mustProvisionerRef(t, "provider-first")
	secondRef := mustProvisionerRef(t, "provider-second")
	firstProvider := provisioningfake.New(provisioningfake.ModeSynchronous)
	secondProvider := provisioningfake.New(provisioningfake.ModeSynchronous)
	service, _, selector, resolver := newService(t, firstRef, firstProvider)
	resolver.Providers[secondRef] = secondProvider

	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-bound", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-create", EventID: "event-create", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	selector.Ref = secondRef
	updated, err := service.UpdateResource(context.Background(), application.UpdateResourceCommand{
		ID: "resource-bound", ExpectedGeneration: created.Resource.Resource.Generation(), Spec: testSpec(t),
		OperationID: "operation-update", EventID: "event-update", RequestedAt: applicationTime.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("UpdateResource() error = %v", err)
	}
	if updated.Resource.ProvisionerRef != firstRef {
		t.Fatalf("binding = %q, want %q", updated.Resource.ProvisionerRef, firstRef)
	}
	if selector.Calls != 1 {
		t.Fatalf("selector calls = %d, want 1", selector.Calls)
	}
	if firstProvider.SubmissionCount("operation-update") != 1 || secondProvider.SubmissionCount("operation-update") != 0 {
		t.Fatalf("update used incorrect provider: first=%d second=%d", firstProvider.SubmissionCount("operation-update"), secondProvider.SubmissionCount("operation-update"))
	}
}

func TestPassiveObservationUsesRealLifecycleWithoutSyntheticOperation(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-observe")
	provider := provisioningfake.New(provisioningfake.ModeExisting)
	service, store, _, _ := newService(t, ref, provider)
	resource, err := domain.NewResource("resource-existing", provisioningfake.ResourceType(), domain.OwnerRef{Kind: "team", ID: "platform"}, testSpec(t), applicationTime)
	if err != nil {
		t.Fatalf("NewResource() error = %v", err)
	}
	status, err := domain.NewResourceStatus(resource.ID(), 1, domain.ResourceStateUnknown, nil, applicationTime)
	if err != nil {
		t.Fatalf("NewResourceStatus() error = %v", err)
	}
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		return tx.Resources().CreateResource(context.Background(), application.ResourceRecord{Resource: resource, Status: status, ProvisionerRef: ref})
	}); err != nil {
		t.Fatalf("store resource error = %v", err)
	}

	result, err := service.ObserveResource(context.Background(), application.ObserveResourceCommand{ID: resource.ID(), ObservedAt: applicationTime.Add(time.Minute)})
	if err != nil {
		t.Fatalf("ObserveResource() error = %v", err)
	}
	if result.Operation.ID() != "" || result.Event != nil {
		t.Fatal("passive observation created a synthetic operation or event")
	}
	if result.Resource.Status.State() != domain.ResourceStateReady {
		t.Fatalf("resource state = %s, want Ready", result.Resource.Status.State())
	}
}

func TestUpdateFailurePreservesReadyAndRetryCreatesNewOperation(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-failure")
	provider := provisioningfake.New(provisioningfake.ModeSynchronous)
	service, _, _, resolver := newService(t, ref, provider)
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-retry", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-create-retry", EventID: "event-create-retry", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	failureProvider := provisioningfake.New(provisioningfake.ModeFailure)
	resolver.Providers[ref] = failureProvider
	failed, err := service.UpdateResource(context.Background(), application.UpdateResourceCommand{
		ID: "resource-retry", ExpectedGeneration: created.Resource.Resource.Generation(), Spec: testSpec(t),
		OperationID: "operation-failed-update", EventID: "event-failed-update", RequestedAt: applicationTime.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("UpdateResource() error = %v", err)
	}
	if failed.Operation.State() != domain.OperationStateFailed || failed.Resource.Status.State() != domain.ResourceStateReady {
		t.Fatalf("failed update state=%s resource=%s, want Failed/Ready", failed.Operation.State(), failed.Resource.Status.State())
	}

	resolver.Providers[ref] = provisioningfake.New(provisioningfake.ModeSynchronous)
	retry, err := service.RetryOperation(context.Background(), application.RetryOperationCommand{
		OperationID: "operation-failed-update", NewOperationID: "operation-retry", EventID: "event-retry", RequestedAt: applicationTime.Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("RetryOperation() error = %v", err)
	}
	if retry.Operation.ID() != "operation-retry" || retry.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("retry operation=%q state=%s, want operation-retry/Succeeded", retry.Operation.ID(), retry.Operation.State())
	}
}

func TestCreateIdempotencyReplaysOriginalResult(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-idempotent")
	provider := provisioningfake.New(provisioningfake.ModeSynchronous)
	service, _, _, _ := newService(t, ref, provider)
	command := application.CreateResourceCommand{
		ID: "resource-idempotent", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-idempotent", EventID: "event-idempotent", RequestedAt: applicationTime,
		IdempotencyKey: "key-1", Fingerprint: "fingerprint-1",
	}
	first, err := service.CreateResource(context.Background(), command)
	if err != nil {
		t.Fatalf("first CreateResource() error = %v", err)
	}
	second, err := service.CreateResource(context.Background(), command)
	if err != nil {
		t.Fatalf("second CreateResource() error = %v", err)
	}
	if !second.Replay || second.Operation.ID() != first.Operation.ID() {
		t.Fatalf("replay=%t operation=%q, want replay and %q", second.Replay, second.Operation.ID(), first.Operation.ID())
	}
	if provider.SubmissionCount(command.OperationID) != 1 {
		t.Fatalf("submission count = %d, want 1", provider.SubmissionCount(command.OperationID))
	}
}

func TestIdempotencyKeyRequiresFingerprint(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-idempotency-validation")
	service, _, selector, _ := newService(t, ref, provisioningfake.New(provisioningfake.ModeSynchronous))
	_, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-idempotency-validation", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-idempotency-validation", EventID: "event-idempotency-validation", RequestedAt: applicationTime,
		IdempotencyKey: "missing-fingerprint",
	})
	if err == nil {
		t.Fatal("CreateResource() accepted an idempotency key without a fingerprint")
	}
	if selector.Calls != 0 {
		t.Fatalf("selector calls = %d, want 0", selector.Calls)
	}
}

func TestUpdateIdempotencyReplaysBeforeGenerationValidation(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-update-idempotent")
	provider := provisioningfake.New(provisioningfake.ModeSynchronous)
	service, _, _, _ := newService(t, ref, provider)
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-update-idempotent", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-create-update-idempotent", EventID: "event-create-update-idempotent", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	command := application.UpdateResourceCommand{
		ID: "resource-update-idempotent", ExpectedGeneration: created.Resource.Resource.Generation(), Spec: testSpec(t),
		OperationID: "operation-update-idempotent", EventID: "event-update-idempotent", RequestedAt: applicationTime.Add(time.Minute),
		IdempotencyKey: "update-key", Fingerprint: "update-fingerprint",
	}
	first, err := service.UpdateResource(context.Background(), command)
	if err != nil {
		t.Fatalf("first UpdateResource() error = %v", err)
	}
	second, err := service.UpdateResource(context.Background(), command)
	if err != nil {
		t.Fatalf("second UpdateResource() error = %v", err)
	}
	if !second.Replay || second.Operation.ID() != first.Operation.ID() {
		t.Fatalf("replay=%t operation=%q, want replay and %q", second.Replay, second.Operation.ID(), first.Operation.ID())
	}
	if provider.SubmissionCount(command.OperationID) != 1 {
		t.Fatalf("submission count = %d, want 1", provider.SubmissionCount(command.OperationID))
	}
}

func TestUpdateExpectedGenerationIsCheckedDuringSave(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-generation")
	provider := provisioningfake.New(provisioningfake.ModeSynchronous)
	service, _, _, _ := newService(t, ref, provider)
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-generation", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-create-generation", EventID: "event-create-generation", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	_, err = service.UpdateResource(context.Background(), application.UpdateResourceCommand{
		ID: "resource-generation", ExpectedGeneration: created.Resource.Resource.Generation(), Spec: testSpec(t),
		OperationID: "operation-update-generation-1", EventID: "event-update-generation-1", RequestedAt: applicationTime.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("first UpdateResource() error = %v", err)
	}
	if _, err := service.UpdateResource(context.Background(), application.UpdateResourceCommand{
		ID: "resource-generation", ExpectedGeneration: created.Resource.Resource.Generation(), Spec: testSpec(t),
		OperationID: "operation-update-generation-2", EventID: "event-update-generation-2", RequestedAt: applicationTime.Add(2 * time.Minute),
	}); err == nil {
		t.Fatal("second update with stale generation succeeded")
	}
}

func TestConcurrentUpdateAndDeleteCreateOnlyOneActiveOperation(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-concurrent")
	service, store, _, resolver := newService(t, ref, provisioningfake.New(provisioningfake.ModeSynchronous))
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-concurrent", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-create-concurrent", EventID: "event-create-concurrent", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	resolver.Providers[ref] = provisioningfake.New(provisioningfake.ModeAsynchronous)
	updatedSpec := mustSpecValue(t, "update")
	start := make(chan struct{})
	errors := make(chan error, 2)
	go func() {
		<-start
		_, updateErr := service.UpdateResource(context.Background(), application.UpdateResourceCommand{
			ID: created.Resource.Resource.ID(), ExpectedGeneration: 1, Spec: updatedSpec,
			OperationID: "operation-concurrent-update", EventID: "event-concurrent-update", RequestedAt: applicationTime.Add(time.Minute),
		})
		errors <- updateErr
	}()
	go func() {
		<-start
		_, deleteErr := service.DeleteResource(context.Background(), application.DeleteResourceCommand{
			ID: created.Resource.Resource.ID(), ExpectedGeneration: 1,
			OperationID: "operation-concurrent-delete", EventID: "event-concurrent-delete", RequestedAt: applicationTime.Add(time.Minute),
		})
		errors <- deleteErr
	}()
	close(start)
	firstErr, secondErr := <-errors, <-errors
	if (firstErr == nil) == (secondErr == nil) {
		t.Fatalf("request errors = %v and %v, want exactly one success", firstErr, secondErr)
	}
	_, updateLookupErr := store.GetOperation(context.Background(), "operation-concurrent-update")
	_, deleteLookupErr := store.GetOperation(context.Background(), "operation-concurrent-delete")
	if (updateLookupErr == nil) == (deleteLookupErr == nil) {
		t.Fatalf("operation lookup errors = %v and %v, want exactly one operation", updateLookupErr, deleteLookupErr)
	}
}

func TestFakeTransactionRollsBackRequestWhenEventAppendFails(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-rollback")
	service, store, _, _ := newService(t, ref, provisioningfake.New(provisioningfake.ModeSynchronous))
	collision, err := domain.NewEvent("event-collision", "existing-resource", "", 1, "Existing", "Existing", "", applicationTime)
	if err != nil {
		t.Fatalf("NewEvent() error = %v", err)
	}
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error { return tx.Events().Append(context.Background(), collision) }); err != nil {
		t.Fatalf("seed event error = %v", err)
	}
	_, err = service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-rollback", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-rollback", EventID: collision.ID(), RequestedAt: applicationTime,
		IdempotencyKey: "rollback-key", Fingerprint: "rollback-fingerprint",
	})
	if err == nil {
		t.Fatal("CreateResource() succeeded with duplicate event ID")
	}
	if _, err := store.GetResource(context.Background(), "resource-rollback"); err == nil {
		t.Fatal("resource remained after transaction rollback")
	}
	if _, err := store.GetOperation(context.Background(), "operation-rollback"); err == nil {
		t.Fatal("operation remained after transaction rollback")
	}
	if _, err := store.GetExecution(context.Background(), "operation-rollback"); err == nil {
		t.Fatal("execution remained after transaction rollback")
	}
	if _, err := store.GetIdempotency(context.Background(), "rollback-key"); err == nil {
		t.Fatal("idempotency record remained after transaction rollback")
	}
}

func TestFakeTransactionRollsBackNestedExecutionMutation(t *testing.T) {
	store := fake.NewStore()
	handle, _ := provisioning.NewExecutionHandle("rollback-nested")
	submission := provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle}}}
	seed := application.ProvisioningExecutionRecord{OperationID: "operation-nested-rollback", State: application.AttemptAccepted, Submission: &submission, Version: 1}
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		return tx.Executions().CreateExecution(context.Background(), seed)
	}); err != nil {
		t.Fatalf("seed execution error = %v", err)
	}
	rollbackErr := errors.New("rollback")
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		execution, loadErr := tx.Executions().GetExecution(context.Background(), seed.OperationID)
		if loadErr != nil {
			return loadErr
		}
		execution.Submission.Observation.Execution.State = provisioning.ExecutionStateFailed
		return rollbackErr
	}); !errors.Is(err, rollbackErr) {
		t.Fatalf("transaction error = %v, want rollback", err)
	}
	stored, err := store.GetExecution(context.Background(), seed.OperationID)
	if err != nil {
		t.Fatalf("GetExecution() error = %v", err)
	}
	if stored.Submission.Observation.Execution.State != provisioning.ExecutionStateAccepted {
		t.Fatalf("nested execution state = %s, want Accepted", stored.Submission.Observation.Execution.State)
	}
	stored.Submission.Observation.Execution.State = provisioning.ExecutionStateFailed
	again, err := store.GetExecution(context.Background(), seed.OperationID)
	if err != nil || again.Submission.Observation.Execution.State != provisioning.ExecutionStateAccepted {
		t.Fatalf("post-commit alias error=%v state=%s, want Accepted", err, again.Submission.Observation.Execution.State)
	}
}

func TestSubmitTimeoutRemainsUnknownAndRecoveryObservesBeforeResubmit(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-timeout")
	provider := &scriptedProvisioner{
		submitErr: context.DeadlineExceeded,
		observe: provisioning.ExecutionObservation{Resource: provisioning.ResourceObservation{
			Presence: provisioning.ResourcePresenceUnknown, Readiness: provisioning.ResourceReadinessUnknown, Drift: provisioning.ResourceDriftUnknown,
		}},
	}
	service, store, _, _ := newService(t, ref, provider)
	command := application.CreateResourceCommand{
		ID: "resource-timeout", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-timeout", EventID: "event-timeout", RequestedAt: applicationTime,
		IdempotencyKey: "timeout-key", Fingerprint: "timeout-fingerprint",
	}
	if _, err := service.CreateResource(context.Background(), command); err == nil {
		t.Fatal("CreateResource() succeeded despite timeout")
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil {
		t.Fatalf("GetExecution() error = %v", err)
	}
	if execution.State != application.AttemptUnknown {
		t.Fatalf("execution state = %s, want Unknown", execution.State)
	}
	operation, err := store.GetOperation(context.Background(), command.OperationID)
	if err != nil || operation.Operation.IsTerminal() {
		t.Fatalf("operation error=%v terminal=%t, want nonterminal", err, operation.Operation.IsTerminal())
	}
	replay, err := service.CreateResource(context.Background(), command)
	if err != nil || !replay.Replay {
		t.Fatalf("CreateResource() replay error=%v replay=%t", err, replay.Replay)
	}
	if _, err := service.RecoverOperation(context.Background(), command.OperationID, applicationTime.Add(time.Minute)); err != nil {
		t.Fatalf("RecoverOperation() error = %v", err)
	}
	if provider.submissions != 1 || provider.observations != 1 {
		t.Fatalf("submissions=%d observations=%d, want 1/1", provider.submissions, provider.observations)
	}
	execution, err = store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.State != application.AttemptPending {
		t.Fatalf("recovered execution error=%v state=%s, want Pending", err, execution.State)
	}
}

func TestUnknownAttemptObservesThenResubmitsWithSameOperationID(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-unknown-recovery")
	provider := &scriptedProvisioner{observe: provisioning.ExecutionObservation{Resource: provisioning.ResourceObservation{
		Presence: provisioning.ResourcePresenceUnknown, Readiness: provisioning.ResourceReadinessUnknown, Drift: provisioning.ResourceDriftUnknown,
	}}}
	service, store, _, _ := newService(t, ref, provider)
	command := application.CreateResourceCommand{
		ID: "resource-unknown-recovery", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-unknown-recovery", EventID: "event-unknown-recovery", RequestedAt: applicationTime,
	}
	if _, err := service.CreateResource(context.Background(), command); err == nil {
		t.Fatal("CreateResource() succeeded with malformed submission")
	}
	if provider.submissions != 1 {
		t.Fatalf("initial submissions = %d, want 1", provider.submissions)
	}
	if _, err := service.RecoverOperation(context.Background(), command.OperationID, applicationTime.Add(time.Minute)); err != nil {
		t.Fatalf("first recovery dispatch error = %v", err)
	}
	if provider.submissions != 1 || provider.observations != 1 {
		t.Fatalf("after observation submissions=%d observations=%d, want 1/1", provider.submissions, provider.observations)
	}
	handle, _ := provisioning.NewExecutionHandle("recovered-same-id")
	provider.submit = provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle}}}
	if _, err := service.DispatchOperation(context.Background(), command.OperationID); err != nil {
		t.Fatalf("second recovery dispatch error = %v", err)
	}
	if provider.submissions != 2 {
		t.Fatalf("submissions = %d, want same-ID resubmission", provider.submissions)
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.OperationID != command.OperationID || execution.State != application.AttemptAccepted {
		t.Fatalf("execution error=%v operation=%q state=%s", err, execution.OperationID, execution.State)
	}
}

func TestRecoveryAndResubmissionPreserveExistingHandle(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-handle-preservation")
	handle, _ := provisioning.NewExecutionHandle("preserved-handle")
	provider := &scriptedProvisioner{
		submit:    provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateUnknown, Handle: &handle}}},
		submitErr: provisioning.ErrAmbiguousSubmission,
	}
	service, store, _, _ := newService(t, ref, provider)
	command := application.CreateResourceCommand{
		ID: "resource-handle-preservation", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-handle-preservation", EventID: "event-handle-preservation", RequestedAt: applicationTime,
	}
	if _, err := service.CreateResource(context.Background(), command); err == nil {
		t.Fatal("CreateResource() succeeded despite ambiguous submission")
	}
	provider.mu.Lock()
	provider.submitErr = nil
	provider.observe = provisioning.ExecutionObservation{ObservedAt: applicationTime.Add(time.Minute)}
	provider.mu.Unlock()
	if _, err := service.RecoverOperation(context.Background(), command.OperationID, applicationTime.Add(time.Minute)); err != nil {
		t.Fatalf("RecoverOperation() error = %v", err)
	}
	provider.mu.Lock()
	provider.submit = provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted}}}
	provider.mu.Unlock()
	if _, err := service.DispatchOperation(context.Background(), command.OperationID); err != nil {
		t.Fatalf("DispatchOperation() error = %v", err)
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.Handle == nil || execution.Handle.String() != handle.String() {
		t.Fatalf("execution error=%v handle=%v, want preserved handle", err, execution.Handle)
	}
}

func TestAcceptedAttemptObservationFailureDoesNotEnableResubmission(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-accepted-observation-failure")
	handle, _ := provisioning.NewExecutionHandle("accepted-observation-failure")
	provider := &scriptedProvisioner{
		submit:     provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle}}},
		observeErr: provisioning.ObservationError{Failure: provisioning.ExecutionFailure{Kind: provisioning.FailureUnavailable, Reason: "Unavailable"}},
	}
	service, store, _, _ := newService(t, ref, provider)
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-accepted-observation-failure", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-accepted-observation-failure", EventID: "event-accepted-observation-failure", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	if _, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{OperationID: created.Operation.ID(), ObservedAt: applicationTime.Add(time.Minute)}); err == nil {
		t.Fatal("ObserveOperation() succeeded despite observation failure")
	}
	provider.mu.Lock()
	provider.observeErr = nil
	provider.observe = provisioning.ExecutionObservation{Resource: provisioning.ResourceObservation{Presence: provisioning.ResourcePresenceUnknown, Readiness: provisioning.ResourceReadinessUnknown, Drift: provisioning.ResourceDriftUnknown}}
	provider.mu.Unlock()
	if _, err := service.RecoverOperation(context.Background(), created.Operation.ID(), applicationTime.Add(2*time.Minute)); err != nil {
		t.Fatalf("RecoverOperation() error = %v", err)
	}
	execution, err := store.GetExecution(context.Background(), created.Operation.ID())
	if err != nil {
		t.Fatalf("GetExecution() error = %v", err)
	}
	if execution.State != application.AttemptUnknown || !execution.AcceptanceConfirmed || provider.submissions != 1 {
		t.Fatalf("state=%s accepted=%t submissions=%d", execution.State, execution.AcceptanceConfirmed, provider.submissions)
	}
}

func TestStaleNoExecutionObservationDoesNotEnableResubmission(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-stale-no-execution")
	provider := &scriptedProvisioner{
		submitErr: context.DeadlineExceeded,
		observe: provisioning.ExecutionObservation{
			Resource:   provisioning.ResourceObservation{Presence: provisioning.ResourcePresenceUnknown, Readiness: provisioning.ResourceReadinessUnknown, Drift: provisioning.ResourceDriftUnknown},
			ObservedAt: applicationTime.Add(-time.Minute),
		},
	}
	service, store, _, _ := newService(t, ref, provider)
	command := application.CreateResourceCommand{
		ID: "resource-stale-no-execution", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-stale-no-execution", EventID: "event-stale-no-execution", RequestedAt: applicationTime,
	}
	if _, err := service.CreateResource(context.Background(), command); err == nil {
		t.Fatal("CreateResource() succeeded despite timeout")
	}
	if _, err := service.RecoverOperation(context.Background(), command.OperationID, applicationTime.Add(time.Hour)); err == nil {
		t.Fatal("RecoverOperation() accepted stale no-execution evidence")
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.State != application.AttemptUnknown {
		t.Fatalf("execution error=%v state=%s, want Unknown", err, execution.State)
	}
	if provider.submissions != 1 {
		t.Fatalf("submissions = %d, want 1", provider.submissions)
	}
}

func TestOutOfOrderNoExecutionObservationDoesNotEnableResubmission(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-out-of-order-observation")
	provider := &scriptedProvisioner{submitErr: context.DeadlineExceeded}
	service, store, _, _ := newService(t, ref, provider)
	command := application.CreateResourceCommand{
		ID: "resource-out-of-order-observation", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-out-of-order-observation", EventID: "event-out-of-order-observation", RequestedAt: applicationTime,
	}
	if _, err := service.CreateResource(context.Background(), command); err == nil {
		t.Fatal("CreateResource() succeeded despite timeout")
	}
	provider.mu.Lock()
	provider.submitErr = nil
	provider.observe = provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateUnknown}, ObservedAt: applicationTime.Add(2 * time.Minute)}
	provider.mu.Unlock()
	if _, err := service.RecoverOperation(context.Background(), command.OperationID, applicationTime.Add(3*time.Minute)); err != nil {
		t.Fatalf("first RecoverOperation() error = %v", err)
	}
	provider.mu.Lock()
	provider.observe = provisioning.ExecutionObservation{ObservedAt: applicationTime.Add(time.Minute)}
	provider.mu.Unlock()
	if _, err := service.RecoverOperation(context.Background(), command.OperationID, applicationTime.Add(4*time.Minute)); err == nil {
		t.Fatal("second RecoverOperation() accepted out-of-order no-execution evidence")
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.State != application.AttemptUnknown {
		t.Fatalf("execution error=%v state=%s, want Unknown", err, execution.State)
	}
}

func TestEqualTimestampNoExecutionObservationDoesNotEnableResubmission(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-equal-observation")
	provider := &scriptedProvisioner{submitErr: context.DeadlineExceeded}
	service, store, _, _ := newService(t, ref, provider)
	command := application.CreateResourceCommand{
		ID: "resource-equal-observation", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-equal-observation", EventID: "event-equal-observation", RequestedAt: applicationTime,
	}
	if _, err := service.CreateResource(context.Background(), command); err == nil {
		t.Fatal("CreateResource() succeeded despite timeout")
	}
	provider.mu.Lock()
	provider.submitErr = nil
	provider.observe = provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateUnknown}, ObservedAt: applicationTime.Add(time.Minute)}
	provider.mu.Unlock()
	if _, err := service.RecoverOperation(context.Background(), command.OperationID, applicationTime.Add(2*time.Minute)); err != nil {
		t.Fatalf("first RecoverOperation() error = %v", err)
	}
	provider.mu.Lock()
	provider.observe = provisioning.ExecutionObservation{ObservedAt: applicationTime.Add(time.Minute)}
	provider.mu.Unlock()
	if _, err := service.RecoverOperation(context.Background(), command.OperationID, applicationTime.Add(3*time.Minute)); err == nil {
		t.Fatal("second RecoverOperation() accepted equal-time contradictory evidence")
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.State != application.AttemptUnknown {
		t.Fatalf("execution error=%v state=%s, want Unknown", err, execution.State)
	}
}

func TestAcceptanceLearnedFromObservationPreventsResubmission(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-observed-acceptance")
	provider := &scriptedProvisioner{submit: provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateUnknown}}}, submitErr: provisioning.ErrAmbiguousSubmission}
	service, store, _, _ := newService(t, ref, provider)
	command := application.CreateResourceCommand{
		ID: "resource-observed-acceptance", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-observed-acceptance", EventID: "event-observed-acceptance", RequestedAt: applicationTime,
	}
	if _, err := service.CreateResource(context.Background(), command); err == nil {
		t.Fatal("CreateResource() succeeded despite ambiguous submission")
	}
	provider.mu.Lock()
	provider.submitErr = nil
	provider.observe = provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateRunning}, ObservedAt: applicationTime.Add(time.Minute)}
	provider.mu.Unlock()
	if _, err := service.RecoverOperation(context.Background(), command.OperationID, applicationTime.Add(time.Minute)); err != nil {
		t.Fatalf("RecoverOperation() error = %v", err)
	}
	provider.mu.Lock()
	provider.observeErr = provisioning.ObservationError{Failure: provisioning.ExecutionFailure{Kind: provisioning.FailureUnavailable, Reason: "Unavailable"}}
	provider.mu.Unlock()
	if _, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{OperationID: command.OperationID, ObservedAt: applicationTime.Add(2 * time.Minute)}); err == nil {
		t.Fatal("ObserveOperation() succeeded despite observation failure")
	}
	provider.mu.Lock()
	provider.observeErr = nil
	provider.observe = provisioning.ExecutionObservation{ObservedAt: applicationTime.Add(3 * time.Minute)}
	provider.mu.Unlock()
	if _, err := service.RecoverOperation(context.Background(), command.OperationID, applicationTime.Add(3*time.Minute)); err != nil {
		t.Fatalf("second RecoverOperation() error = %v", err)
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.State != application.AttemptUnknown || !execution.AcceptanceConfirmed || provider.submissions != 1 {
		t.Fatalf("error=%v state=%s accepted=%t submissions=%d", err, execution.State, execution.AcceptanceConfirmed, provider.submissions)
	}
}

func TestMalformedTerminalSubmissionRemainsTerminal(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-rejected-terminal")
	provider := &scriptedProvisioner{submit: provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateFailed, Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureExecution}},
	}}}
	service, store, _, _ := newService(t, ref, provider)
	result, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-rejected-terminal", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-rejected-terminal", EventID: "event-rejected-terminal", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	execution, loadErr := store.GetExecution(context.Background(), "operation-rejected-terminal")
	if loadErr != nil || execution.State != application.AttemptFailed || execution.LastFailure == nil || execution.LastFailure.Reason != "MalformedExecutionFailure" || result.Operation.State() != domain.OperationStateFailed {
		t.Fatalf("execution error=%v state=%s failure=%#v operation=%s", loadErr, execution.State, execution.LastFailure, result.Operation.State())
	}
}

func TestInvalidExecutionStateMapsToUnknown(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-invalid-state")
	provider := &scriptedProvisioner{submit: provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: "Invalid"}}}}
	service, _, _, _ := newService(t, ref, provider)
	result, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-invalid-state", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-invalid-state", EventID: "event-invalid-state", RequestedAt: applicationTime,
	})
	if err == nil {
		t.Fatal("CreateResource() accepted an invalid execution state")
	}
	if result.Execution.State != application.AttemptUnknown || result.Execution.LastFailure == nil || result.Execution.LastFailure.Reason != "MalformedExecutionState" {
		t.Fatalf("execution state=%s failure=%#v", result.Execution.State, result.Execution.LastFailure)
	}
}

func TestInvalidObservedExecutionStateDoesNotReplaceValidEvidence(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-invalid-observed-state")
	provider := &scriptedProvisioner{
		submit:  provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted}}},
		observe: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: "Invalid"}, ObservedAt: applicationTime.Add(time.Minute)},
	}
	service, store, _, _ := newService(t, ref, provider)
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-invalid-observed-state", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-invalid-observed-state", EventID: "event-invalid-observed-state", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	if _, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{OperationID: created.Operation.ID(), ObservedAt: applicationTime.Add(time.Minute)}); err == nil {
		t.Fatal("ObserveOperation() accepted invalid execution state")
	}
	execution, loadErr := store.GetExecution(context.Background(), created.Operation.ID())
	if loadErr != nil || execution.LastObservation != nil || execution.LastFailure == nil || execution.LastFailure.Reason != "MalformedExecutionState" {
		t.Fatalf("execution error=%v observation=%#v failure=%#v", loadErr, execution.LastObservation, execution.LastFailure)
	}
}

func TestConclusiveNonAcceptanceIsNotRecordedAsAcceptance(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-non-acceptance")
	provider := &scriptedProvisioner{submit: provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{
		State:   provisioning.ExecutionStateFailed,
		Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureInvalidRequest, Reason: "InvalidRequest"},
	}}}}
	service, _, _, _ := newService(t, ref, provider)
	result, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-non-acceptance", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-non-acceptance", EventID: "event-non-acceptance", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	if result.Execution.AcceptanceConfirmed {
		t.Fatal("conclusive non-acceptance was recorded as accepted")
	}
}

func TestExplicitNormalizedSubmissionFailureIsTerminal(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-normalized-timeout")
	provider := &scriptedProvisioner{submit: provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{
		State:   provisioning.ExecutionStateFailed,
		Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureTimeout, Reason: "Timeout"},
	}}}}
	service, store, _, _ := newService(t, ref, provider)
	result, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-normalized-timeout", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-normalized-timeout", EventID: "event-normalized-timeout", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	execution, loadErr := store.GetExecution(context.Background(), "operation-normalized-timeout")
	if loadErr != nil || execution.State != application.AttemptFailed {
		t.Fatalf("execution error=%v state=%s, want Failed", loadErr, execution.State)
	}
	operation, loadErr := store.GetOperation(context.Background(), "operation-normalized-timeout")
	if loadErr != nil || operation.Operation.State() != domain.OperationStateFailed || result.Operation.State() != domain.OperationStateFailed {
		t.Fatalf("operation error=%v persisted=%s result=%s, want Failed", loadErr, operation.Operation.State(), result.Operation.State())
	}
}

func TestExplicitFailedSubmissionRemainsTerminalWhenCallAlsoErrors(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-failed-with-error")
	provider := &scriptedProvisioner{
		submit: provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{
			State:   provisioning.ExecutionStateFailed,
			Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureTimeout, Reason: "ExecutionTimedOut"},
		}}},
		submitErr: context.DeadlineExceeded,
	}
	service, _, _, _ := newService(t, ref, provider)
	result, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-failed-with-error", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-failed-with-error", EventID: "event-failed-with-error", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	if result.Operation.State() != domain.OperationStateFailed || result.Execution.State != application.AttemptFailed {
		t.Fatalf("operation=%s execution=%s, want Failed/Failed", result.Operation.State(), result.Execution.State)
	}
}

func TestMalformedTerminalObservationRemainsTerminal(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-rejected-observation")
	handle, _ := provisioning.NewExecutionHandle("rejected-observation")
	provider := &scriptedProvisioner{
		submit:  provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle}}},
		observe: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateFailed, Handle: &handle, Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureExecution}}, ObservedAt: applicationTime.Add(time.Minute)},
	}
	service, store, _, _ := newService(t, ref, provider)
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-rejected-observation", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-rejected-observation", EventID: "event-rejected-observation", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	result, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{OperationID: created.Operation.ID(), ObservedAt: applicationTime.Add(time.Minute)})
	if err != nil {
		t.Fatalf("ObserveOperation() error = %v", err)
	}
	execution, loadErr := store.GetExecution(context.Background(), created.Operation.ID())
	if loadErr != nil || execution.State != application.AttemptFailed || execution.LastFailure == nil || execution.LastFailure.Reason != "MalformedExecutionFailure" || result.Operation.State() != domain.OperationStateFailed {
		t.Fatalf("execution error=%v state=%s failure=%#v operation=%s", loadErr, execution.State, execution.LastFailure, result.Operation.State())
	}
}

func TestExplicitTerminalObservationWinsWhenCallAlsoErrors(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-observation-terminal-error")
	handle, _ := provisioning.NewExecutionHandle("observation-terminal-error")
	provider := &scriptedProvisioner{
		submit: provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle}}},
		observe: provisioning.ExecutionObservation{Execution: &provisioning.Execution{
			State: provisioning.ExecutionStateFailed, Handle: &handle,
			Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureExecution, Reason: "ExecutionFailed"},
		}, ObservedAt: applicationTime.Add(time.Minute)},
		observeErr: context.DeadlineExceeded,
	}
	service, _, _, _ := newService(t, ref, provider)
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-observation-terminal-error", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-observation-terminal-error", EventID: "event-observation-terminal-error", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	result, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{OperationID: created.Operation.ID(), ObservedAt: applicationTime.Add(time.Minute)})
	if err != nil {
		t.Fatalf("ObserveOperation() error = %v", err)
	}
	if result.Operation.State() != domain.OperationStateFailed || result.Execution.State != application.AttemptFailed {
		t.Fatalf("operation=%s execution=%s, want Failed/Failed", result.Operation.State(), result.Execution.State)
	}
}

func TestPersistedTerminalEvidenceCompletesWithoutAnotherProviderCall(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-persisted-terminal")
	handle, _ := provisioning.NewExecutionHandle("persisted-terminal")
	provider := &scriptedProvisioner{submit: provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle}}}}
	service, store, _, _ := newService(t, ref, provider)
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-persisted-terminal", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-persisted-terminal", EventID: "event-persisted-terminal", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	terminal := provisioning.ExecutionObservation{
		Execution:  &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle},
		Resource:   provisioning.ResourceObservation{Presence: provisioning.ResourcePresenceUnknown, Readiness: provisioning.ResourceReadinessReady, Drift: provisioning.ResourceDriftUnknown},
		ObservedAt: applicationTime.Add(time.Minute),
	}
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		execution, loadErr := tx.Executions().GetExecution(context.Background(), created.Operation.ID())
		if loadErr != nil {
			return loadErr
		}
		execution.State = application.AttemptSucceeded
		execution.LastObservation = &terminal
		execution.LastObservedAt = terminal.ObservedAt
		return tx.Executions().SaveExecution(context.Background(), execution, execution.Version)
	}); err != nil {
		t.Fatalf("persist terminal evidence error = %v", err)
	}
	result, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{OperationID: created.Operation.ID(), ObservedAt: applicationTime.Add(2 * time.Minute)})
	if err != nil {
		t.Fatalf("ObserveOperation() error = %v", err)
	}
	if result.Operation.State() != domain.OperationStateSucceeded || provider.observations != 0 {
		t.Fatalf("operation=%s provider observations=%d, want Succeeded/0", result.Operation.State(), provider.observations)
	}
	stored, err := store.GetExecution(context.Background(), created.Operation.ID())
	if err != nil || stored.LastObservation == nil || stored.LastObservation.Resource != (domain.ObservedFacts{}) {
		t.Fatalf("stored terminal evidence error=%v observation=%#v, want sanitized facts", err, stored.LastObservation)
	}
}

func TestPersistedTerminalAttemptMustMatchExecutionEvidence(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-contradictory-terminal")
	handle, _ := provisioning.NewExecutionHandle("contradictory-terminal")
	provider := &scriptedProvisioner{submit: provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle}}}}
	service, store, _, _ := newService(t, ref, provider)
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-contradictory-terminal", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-contradictory-terminal", EventID: "event-contradictory-terminal", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	contradictory := provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateFailed, Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureExecution, Reason: "Failed"}}, ObservedAt: applicationTime.Add(time.Minute)}
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		execution, loadErr := tx.Executions().GetExecution(context.Background(), created.Operation.ID())
		if loadErr != nil {
			return loadErr
		}
		execution.State = application.AttemptSucceeded
		execution.LastObservation = &contradictory
		execution.LastObservedAt = contradictory.ObservedAt
		return tx.Executions().SaveExecution(context.Background(), execution, execution.Version)
	}); err != nil {
		t.Fatalf("persist contradictory evidence error = %v", err)
	}
	if _, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{OperationID: created.Operation.ID(), ObservedAt: applicationTime.Add(2 * time.Minute)}); err == nil {
		t.Fatal("ObserveOperation() completed contradictory persisted terminal evidence")
	}
}

func TestInvalidNonterminalObservedFactsAreNotPersistedAsValid(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-invalid-nonterminal-facts")
	provider := &scriptedProvisioner{submit: provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted},
		Resource:  provisioning.ResourceObservation{Presence: provisioning.ResourcePresenceUnknown, Readiness: provisioning.ResourceReadinessReady, Drift: provisioning.ResourceDriftUnknown},
	}}}
	service, store, _, _ := newService(t, ref, provider)
	_, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-invalid-nonterminal-facts", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-invalid-nonterminal-facts", EventID: "event-invalid-nonterminal-facts", RequestedAt: applicationTime,
	})
	if err == nil {
		t.Fatal("CreateResource() accepted invalid nonterminal facts")
	}
	execution, loadErr := store.GetExecution(context.Background(), "operation-invalid-nonterminal-facts")
	if loadErr != nil || execution.State != application.AttemptUnknown || execution.LastFailure == nil || execution.LastFailure.Reason != "MalformedObservedFacts" || execution.Submission != nil {
		t.Fatalf("execution error=%v state=%s failure=%#v submission=%#v", loadErr, execution.State, execution.LastFailure, execution.Submission)
	}
}

func TestTerminalExecutionSurvivesMalformedObservedFacts(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-terminal-invalid-facts")
	provider := &scriptedProvisioner{submit: provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded},
		Resource:  provisioning.ResourceObservation{Presence: provisioning.ResourcePresenceUnknown, Readiness: provisioning.ResourceReadinessReady, Drift: provisioning.ResourceDriftUnknown},
	}}}
	service, store, _, _ := newService(t, ref, provider)
	result, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-terminal-invalid-facts", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-terminal-invalid-facts", EventID: "event-terminal-invalid-facts", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	if result.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("operation state = %s, want Succeeded", result.Operation.State())
	}
	execution, loadErr := store.GetExecution(context.Background(), result.Operation.ID())
	if loadErr != nil || execution.Submission == nil || execution.Submission.Observation.Resource != (domain.ObservedFacts{}) {
		t.Fatalf("execution error=%v submission=%#v, want sanitized facts", loadErr, execution.Submission)
	}
}

func TestObservationCallFailureIsNormalizedBeforeStorage(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-invalid-observation-error")
	provider := &scriptedProvisioner{
		submit:     provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted}}},
		observeErr: provisioning.ObservationError{Failure: provisioning.ExecutionFailure{Kind: "NativeFailure"}},
	}
	service, store, _, _ := newService(t, ref, provider)
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-invalid-observation-error", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-invalid-observation-error", EventID: "event-invalid-observation-error", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	if _, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{OperationID: created.Operation.ID(), ObservedAt: applicationTime.Add(time.Minute)}); err == nil {
		t.Fatal("ObserveOperation() succeeded despite call failure")
	}
	execution, loadErr := store.GetExecution(context.Background(), created.Operation.ID())
	if loadErr != nil || execution.LastFailure == nil || execution.LastFailure.Kind != provisioning.FailureUnknown || execution.LastFailure.Reason != "MalformedExecutionFailure" {
		t.Fatalf("execution error=%v failure=%#v", loadErr, execution.LastFailure)
	}
}

func TestPointerObservationErrorRetainsNormalizedClassification(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-pointer-observation-error")
	provider := &scriptedProvisioner{
		submit:     provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted}}},
		observeErr: &provisioning.ObservationError{Failure: provisioning.ExecutionFailure{Kind: provisioning.FailureUnavailable, Reason: "Unavailable"}},
	}
	service, store, _, _ := newService(t, ref, provider)
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-pointer-observation-error", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-pointer-observation-error", EventID: "event-pointer-observation-error", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	if _, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{OperationID: created.Operation.ID(), ObservedAt: applicationTime.Add(time.Minute)}); err == nil {
		t.Fatal("ObserveOperation() succeeded despite call failure")
	}
	execution, loadErr := store.GetExecution(context.Background(), created.Operation.ID())
	if loadErr != nil || execution.LastFailure == nil || execution.LastFailure.Kind != provisioning.FailureUnavailable || execution.LastFailure.Reason != "Unavailable" {
		t.Fatalf("execution error=%v failure=%#v", loadErr, execution.LastFailure)
	}
}

func TestTypedNilObservationErrorDoesNotPanic(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-nil-observation-error")
	var nilObservationError *provisioning.ObservationError
	provider := &scriptedProvisioner{
		submit:     provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted}}},
		observeErr: nilObservationError,
	}
	service, store, _, _ := newService(t, ref, provider)
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-nil-observation-error", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-nil-observation-error", EventID: "event-nil-observation-error", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	if _, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{OperationID: created.Operation.ID(), ObservedAt: applicationTime.Add(time.Minute)}); err == nil {
		t.Fatal("ObserveOperation() treated typed nil error as success")
	} else if err.Error() == "" {
		t.Fatal("ObserveOperation() returned an unusable normalized error")
	}
	execution, loadErr := store.GetExecution(context.Background(), created.Operation.ID())
	if loadErr != nil || execution.LastFailure == nil || execution.LastFailure.Kind != provisioning.FailureUnknown {
		t.Fatalf("execution error=%v failure=%#v", loadErr, execution.LastFailure)
	}
}

func TestInvalidFailureKindIsNormalizedInStoredEvidence(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-invalid-failure-kind")
	provider := &scriptedProvisioner{submit: provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{
		State:   provisioning.ExecutionStateFailed,
		Failure: &provisioning.ExecutionFailure{Kind: "NativeFailure", Reason: "NativeReason"},
	}}}}
	service, store, _, _ := newService(t, ref, provider)
	result, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-invalid-failure-kind", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-invalid-failure-kind", EventID: "event-invalid-failure-kind", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	execution, loadErr := store.GetExecution(context.Background(), result.Operation.ID())
	if loadErr != nil || execution.LastFailure == nil || execution.LastFailure.Kind != provisioning.FailureUnknown || execution.Submission.Observation.Execution.Failure.Kind != provisioning.FailureUnknown {
		t.Fatalf("execution error=%v last=%#v submission=%#v", loadErr, execution.LastFailure, execution.Submission)
	}
}

func TestPassiveReadyDoesNotCompleteActiveOperation(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-passive-ready")
	handle, _ := provisioning.NewExecutionHandle("passive-ready")
	provider := &scriptedProvisioner{
		submit: provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle}}},
		observe: provisioning.ExecutionObservation{Resource: provisioning.ResourceObservation{
			Presence: provisioning.ResourcePresencePresent, Readiness: provisioning.ResourceReadinessReady, Drift: provisioning.ResourceDriftInSync,
		}},
	}
	service, _, _, _ := newService(t, ref, provider)
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-passive-ready", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-passive-ready", EventID: "event-passive-ready", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	observed, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{OperationID: created.Operation.ID(), ObservedAt: applicationTime.Add(time.Minute)})
	if err != nil {
		t.Fatalf("ObserveOperation() error = %v", err)
	}
	if observed.Operation.State() != domain.OperationStateRunning {
		t.Fatalf("operation state = %s, want Running", observed.Operation.State())
	}
	if observed.Execution.State != application.AttemptAccepted {
		t.Fatalf("execution state = %s, want Accepted", observed.Execution.State)
	}
	if _, err := service.DispatchOperation(context.Background(), created.Operation.ID()); err != nil {
		t.Fatalf("DispatchOperation() error = %v", err)
	}
	if provider.submissions != 1 {
		t.Fatalf("submissions = %d, want 1", provider.submissions)
	}
}

func TestDispatchingClaimPreventsOverlappingSubmit(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-dispatch-claim")
	handle, _ := provisioning.NewExecutionHandle("dispatch-claim")
	provider := &blockingSubmitProvisioner{
		entered: make(chan struct{}), release: make(chan struct{}),
		submission: provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle}}},
	}
	service, _, _, _ := newService(t, ref, provider)
	spec := testSpec(t)
	result := make(chan error, 1)
	go func() {
		_, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
			ID: "resource-dispatch-claim", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
			Spec: spec, OperationID: "operation-dispatch-claim", EventID: "event-dispatch-claim", RequestedAt: applicationTime,
		})
		result <- err
	}()
	<-provider.entered
	second, err := service.DispatchOperation(context.Background(), "operation-dispatch-claim")
	if err != nil {
		t.Fatalf("overlapping DispatchOperation() error = %v", err)
	}
	if second.Execution.State != application.AttemptDispatching {
		t.Fatalf("execution state = %s, want Dispatching", second.Execution.State)
	}
	observed, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{OperationID: "operation-dispatch-claim", ObservedAt: applicationTime.Add(time.Minute)})
	if err != nil || observed.Execution.State != application.AttemptDispatching {
		t.Fatalf("ObserveOperation() error=%v state=%s, want Dispatching", err, observed.Execution.State)
	}
	close(provider.release)
	if err := <-result; err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	if provider.submissions != 1 || provider.observations != 0 {
		t.Fatalf("submissions=%d observations=%d, want 1/0", provider.submissions, provider.observations)
	}
}

func TestDispatchRejectsOperationBeforeFinalPhase(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-early-dispatch")
	provider := provisioningfake.New(provisioningfake.ModeSynchronous)
	service, store, _, _ := newService(t, ref, provider)
	resource, err := domain.NewResource("resource-early-dispatch", provisioningfake.ResourceType(), domain.OwnerRef{Kind: "team", ID: "platform"}, testSpec(t), applicationTime)
	if err != nil {
		t.Fatalf("NewResource() error = %v", err)
	}
	status, err := domain.NewResourceStatus(resource.ID(), 0, domain.ResourceStateUnknown, nil, applicationTime)
	if err != nil {
		t.Fatalf("NewResourceStatus() error = %v", err)
	}
	resourceType, _ := domain.NewResourceType(resource.Type(), "fake resource", []domain.Capability{domain.CapabilityCreate})
	transition, err := (lifecycle.Engine{}).Request(resource, resourceType, status, nil, domain.CapabilityCreate, "operation-early-dispatch", "event-early-dispatch", applicationTime)
	if err != nil {
		t.Fatalf("Lifecycle.Request() error = %v", err)
	}
	execution := application.ProvisioningExecutionRecord{
		OperationID: transition.Operation.ID(), ProvisionerRef: ref, ResourceID: resource.ID(), ResourceType: resource.Type(),
		Capability: domain.CapabilityCreate, TargetGeneration: resource.Generation(), Spec: resource.Spec(), State: application.AttemptPending, Version: 1,
	}
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		if createErr := tx.Resources().CreateResource(context.Background(), application.ResourceRecord{Resource: resource, Status: transition.Status, ProvisionerRef: ref, Version: 1}); createErr != nil {
			return createErr
		}
		if createErr := tx.Operations().CreateOperation(context.Background(), application.OperationRecord{Operation: transition.Operation, Version: 1}); createErr != nil {
			return createErr
		}
		return tx.Executions().CreateExecution(context.Background(), execution)
	}); err != nil {
		t.Fatalf("seed request error = %v", err)
	}
	if _, err := service.DispatchOperation(context.Background(), transition.Operation.ID()); err == nil {
		t.Fatal("DispatchOperation() submitted before final lifecycle phase")
	}
	if provider.SubmissionCount(transition.Operation.ID()) != 0 {
		t.Fatal("early dispatch reached provider")
	}
}

func TestPublicAdvanceRejectsReservedEventNamespace(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-reserved-event")
	service, _, _, _ := newService(t, ref, provisioningfake.New(provisioningfake.ModeAsynchronous))
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-reserved-event", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-reserved-event", EventID: "event-reserved-event", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	if _, err := service.AdvanceOperation(context.Background(), application.AdvanceOperationCommand{
		OperationID: created.Operation.ID(), Phase: domain.OperationPhaseApplying,
		EventID: "liftr-internal-reserved", ChangedAt: applicationTime.Add(time.Minute),
	}); err == nil {
		t.Fatal("AdvanceOperation() accepted reserved Event ID")
	}
}

func TestTerminalExecutionOutcomeIsAppliedBeforeDrift(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-terminal-drift")
	provider := &scriptedProvisioner{submit: provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Execution:  &provisioning.Execution{State: provisioning.ExecutionStateSucceeded},
		Resource:   provisioning.ResourceObservation{Presence: provisioning.ResourcePresencePresent, Readiness: provisioning.ResourceReadinessReady, Drift: provisioning.ResourceDriftDrifted},
		ObservedAt: applicationTime.Add(time.Minute),
	}}}
	service, _, _, _ := newService(t, ref, provider)
	result, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-terminal-drift", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-terminal-drift", EventID: "event-terminal-drift", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	if result.Operation.State() != domain.OperationStateSucceeded || result.Resource.Status.State() != domain.ResourceStateReady {
		t.Fatalf("operation=%s resource=%s, want Succeeded/Ready", result.Operation.State(), result.Resource.Status.State())
	}
	assertApplicationCondition(t, result.Resource.Status, "Drifted", domain.ConditionStatusTrue)
	assertApplicationCondition(t, result.Resource.Status, "Reconciled", domain.ConditionStatusFalse)
}

func TestPostOperationFactsDoNotOverwriteDeletedOrFailedState(t *testing.T) {
	t.Run("successful delete remains deleted", func(t *testing.T) {
		ref := mustProvisionerRef(t, "provider-delete-facts")
		service, _, _, resolver := newService(t, ref, provisioningfake.New(provisioningfake.ModeSynchronous))
		created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
			ID: "resource-delete-facts", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
			Spec: testSpec(t), OperationID: "operation-create-delete-facts", EventID: "event-create-delete-facts", RequestedAt: applicationTime,
		})
		if err != nil {
			t.Fatalf("CreateResource() error = %v", err)
		}
		resolver.Providers[ref] = &scriptedProvisioner{submit: provisioning.Submission{Observation: provisioning.ExecutionObservation{
			Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded},
			Resource:  provisioning.ResourceObservation{Presence: provisioning.ResourcePresenceNotFound, Readiness: provisioning.ResourceReadinessUnknown, Drift: provisioning.ResourceDriftUnknown},
		}}}
		deleted, err := service.DeleteResource(context.Background(), application.DeleteResourceCommand{
			ID: created.Resource.Resource.ID(), ExpectedGeneration: created.Resource.Resource.Generation(),
			OperationID: "operation-delete-facts", EventID: "event-delete-facts", RequestedAt: applicationTime.Add(time.Minute),
		})
		if err != nil {
			t.Fatalf("DeleteResource() error = %v", err)
		}
		if deleted.Resource.Status.State() != domain.ResourceStateDeleted {
			t.Fatalf("resource state = %s, want Deleted", deleted.Resource.Status.State())
		}
	})

	t.Run("failed create remains failed", func(t *testing.T) {
		ref := mustProvisionerRef(t, "provider-create-failure-facts")
		provider := &scriptedProvisioner{submit: provisioning.Submission{Observation: provisioning.ExecutionObservation{
			Execution: &provisioning.Execution{State: provisioning.ExecutionStateFailed, Failure: &provisioning.ExecutionFailure{Kind: provisioning.FailureExecution, Reason: "ExecutionFailed"}},
			Resource:  provisioning.ResourceObservation{Presence: provisioning.ResourcePresencePresent, Readiness: provisioning.ResourceReadinessReady, Drift: provisioning.ResourceDriftInSync},
		}}}
		service, _, _, _ := newService(t, ref, provider)
		failed, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
			ID: "resource-create-failure-facts", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
			Spec: testSpec(t), OperationID: "operation-create-failure-facts", EventID: "event-create-failure-facts", RequestedAt: applicationTime,
		})
		if err != nil {
			t.Fatalf("CreateResource() error = %v", err)
		}
		if failed.Operation.State() != domain.OperationStateFailed || failed.Resource.Status.State() != domain.ResourceStateFailed {
			t.Fatalf("operation=%s resource=%s, want Failed/Failed", failed.Operation.State(), failed.Resource.Status.State())
		}
	})
}

func TestPassiveObservationPreservesTerminalLifecycleState(t *testing.T) {
	t.Run("deleted", func(t *testing.T) {
		ref := mustProvisionerRef(t, "provider-passive-deleted")
		service, _, _, resolver := newService(t, ref, provisioningfake.New(provisioningfake.ModeSynchronous))
		created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
			ID: "resource-passive-deleted", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
			Spec: testSpec(t), OperationID: "operation-create-passive-deleted", EventID: "event-create-passive-deleted", RequestedAt: applicationTime,
		})
		if err != nil {
			t.Fatalf("CreateResource() error = %v", err)
		}
		deleted, err := service.DeleteResource(context.Background(), application.DeleteResourceCommand{
			ID: created.Resource.Resource.ID(), ExpectedGeneration: created.Resource.Resource.Generation(),
			OperationID: "operation-delete-passive-deleted", EventID: "event-delete-passive-deleted", RequestedAt: applicationTime.Add(time.Minute),
		})
		if err != nil {
			t.Fatalf("DeleteResource() error = %v", err)
		}
		resolver.Providers[ref] = provisioningfake.New(provisioningfake.ModeExisting)
		observed, err := service.ObserveResource(context.Background(), application.ObserveResourceCommand{ID: deleted.Resource.Resource.ID(), ObservedAt: applicationTime.Add(2 * time.Minute)})
		if err != nil {
			t.Fatalf("ObserveResource() error = %v", err)
		}
		if observed.Resource.Status.State() != domain.ResourceStateDeleted {
			t.Fatalf("resource state = %s, want Deleted", observed.Resource.Status.State())
		}
	})

	t.Run("failed", func(t *testing.T) {
		ref := mustProvisionerRef(t, "provider-passive-failed")
		service, _, _, resolver := newService(t, ref, provisioningfake.New(provisioningfake.ModeFailure))
		failed, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
			ID: "resource-passive-failed", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
			Spec: testSpec(t), OperationID: "operation-create-passive-failed", EventID: "event-create-passive-failed", RequestedAt: applicationTime,
		})
		if err != nil {
			t.Fatalf("CreateResource() error = %v", err)
		}
		resolver.Providers[ref] = provisioningfake.New(provisioningfake.ModeExisting)
		observed, err := service.ObserveResource(context.Background(), application.ObserveResourceCommand{ID: failed.Resource.Resource.ID(), ObservedAt: applicationTime.Add(time.Minute)})
		if err != nil {
			t.Fatalf("ObserveResource() error = %v", err)
		}
		if observed.Resource.Status.State() != domain.ResourceStateFailed {
			t.Fatalf("resource state = %s, want Failed", observed.Resource.Status.State())
		}
	})
}

func TestRepeatedTerminalObservationReturnsPersistedResult(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-terminal-replay")
	handle, _ := provisioning.NewExecutionHandle("terminal-replay")
	provider := &scriptedProvisioner{
		submit:  provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle}}},
		observe: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle}},
	}
	service, _, _, _ := newService(t, ref, provider)
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-terminal-replay", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-terminal-replay", EventID: "event-terminal-replay", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	first, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{OperationID: created.Operation.ID(), ObservedAt: applicationTime.Add(time.Minute)})
	if err != nil {
		t.Fatalf("first ObserveOperation() error = %v", err)
	}
	second, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{OperationID: created.Operation.ID(), ObservedAt: applicationTime.Add(2 * time.Minute)})
	if err != nil {
		t.Fatalf("second ObserveOperation() error = %v", err)
	}
	if first.Operation.State() != domain.OperationStateSucceeded || second.Operation.State() != domain.OperationStateSucceeded || provider.observations != 1 {
		t.Fatalf("first=%s second=%s observations=%d", first.Operation.State(), second.Operation.State(), provider.observations)
	}
}

func TestTerminalOperationRemainsReadableAfterPassiveObservation(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-terminal-after-passive")
	service, _, _, resolver := newService(t, ref, provisioningfake.New(provisioningfake.ModeSynchronous))
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-terminal-after-passive", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-terminal-after-passive", EventID: "event-terminal-after-passive", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	resolver.Providers[ref] = provisioningfake.New(provisioningfake.ModeExisting)
	if _, err := service.ObserveResource(context.Background(), application.ObserveResourceCommand{ID: created.Resource.Resource.ID(), ObservedAt: applicationTime.Add(time.Minute)}); err != nil {
		t.Fatalf("ObserveResource() error = %v", err)
	}
	replayed, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{OperationID: created.Operation.ID(), ObservedAt: applicationTime.Add(2 * time.Minute)})
	if err != nil {
		t.Fatalf("ObserveOperation() error = %v", err)
	}
	if replayed.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("operation state = %s, want Succeeded", replayed.Operation.State())
	}
}

func TestConcurrentTerminalObservationsReturnPersistedResult(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-concurrent-terminal")
	handle, _ := provisioning.NewExecutionHandle("concurrent-terminal")
	provider := newConcurrentObservationProvisioner(handle)
	service, _, _, _ := newService(t, ref, provider)
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-concurrent-terminal", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-concurrent-terminal", EventID: "event-concurrent-terminal", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	results := make(chan struct {
		result application.Result
		err    error
	}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			result, observeErr := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{OperationID: created.Operation.ID(), ObservedAt: applicationTime.Add(time.Minute)})
			results <- struct {
				result application.Result
				err    error
			}{result: result, err: observeErr}
		}()
	}
	succeeded := 0
	for i := 0; i < 2; i++ {
		observed := <-results
		if observed.err != nil {
			t.Fatalf("observation error = %v", observed.err)
		}
		if observed.result.Operation.State() == domain.OperationStateSucceeded {
			succeeded++
		}
	}
	if succeeded != 2 {
		t.Fatalf("succeeded=%d, want 2", succeeded)
	}
}

func TestCreateReplayDoesNotConsultChangedDefaults(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-create-replay")
	service, _, selector, _ := newService(t, ref, provisioningfake.New(provisioningfake.ModeSynchronous))
	command := application.CreateResourceCommand{
		ID: "resource-create-replay", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-create-replay", EventID: "event-create-replay", RequestedAt: applicationTime,
		IdempotencyKey: "create-replay-key", Fingerprint: "create-replay-fingerprint",
	}
	first, err := service.CreateResource(context.Background(), command)
	if err != nil {
		t.Fatalf("first CreateResource() error = %v", err)
	}
	selector.Err = context.DeadlineExceeded
	second, err := service.CreateResource(context.Background(), command)
	if err != nil {
		t.Fatalf("replayed CreateResource() error = %v", err)
	}
	if !second.Replay || second.Operation.ID() != first.Operation.ID() || selector.Calls != 1 {
		t.Fatalf("replay=%t operation=%q selector calls=%d", second.Replay, second.Operation.ID(), selector.Calls)
	}
}

func TestCreateRejectsExistingActiveOperationInsideTransaction(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-create-active")
	provider := provisioningfake.New(provisioningfake.ModeSynchronous)
	service, store, _, _ := newService(t, ref, provider)
	active, err := domain.NewOperation("operation-orphan-active", "resource-create-active", domain.CapabilityCreate, 1, applicationTime)
	if err != nil {
		t.Fatalf("NewOperation() error = %v", err)
	}
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		return tx.Operations().CreateOperation(context.Background(), application.OperationRecord{Operation: active, Version: 1})
	}); err != nil {
		t.Fatalf("seed active operation error = %v", err)
	}
	if _, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-create-active", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-create-active", EventID: "event-create-active", RequestedAt: applicationTime,
	}); err == nil {
		t.Fatal("CreateResource() accepted a resource with an active operation")
	}
	if provider.SubmissionCount("operation-create-active") != 0 {
		t.Fatal("rejected create reached provider")
	}
}

func TestProviderObservationTimestampRejectsStaleFacts(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-stale-observation")
	service, _, _, resolver := newService(t, ref, provisioningfake.New(provisioningfake.ModeSynchronous))
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-stale-observation", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-create-stale-observation", EventID: "event-create-stale-observation", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	resolver.Providers[ref] = &scriptedProvisioner{observe: provisioning.ExecutionObservation{
		Resource:   provisioning.ResourceObservation{Presence: provisioning.ResourcePresencePresent, Readiness: provisioning.ResourceReadinessReady, Drift: provisioning.ResourceDriftInSync},
		ObservedAt: applicationTime,
	}}
	if _, err := service.ObserveResource(context.Background(), application.ObserveResourceCommand{ID: created.Resource.Resource.ID(), ObservedAt: applicationTime.Add(time.Hour)}); err == nil {
		t.Fatal("ObserveResource() accepted stale provider facts using caller timestamp")
	}
}

func TestSubmitRejectsStaleProviderTerminalTimestamp(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-stale-submission")
	provider := &scriptedProvisioner{submit: provisioning.Submission{Observation: provisioning.ExecutionObservation{
		Execution:  &provisioning.Execution{State: provisioning.ExecutionStateSucceeded},
		Resource:   provisioning.ResourceObservation{Presence: provisioning.ResourcePresencePresent, Readiness: provisioning.ResourceReadinessReady, Drift: provisioning.ResourceDriftInSync},
		ObservedAt: applicationTime.Add(-time.Minute),
	}}}
	service, store, _, _ := newService(t, ref, provider)
	_, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-stale-submission", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-stale-submission", EventID: "event-stale-submission", RequestedAt: applicationTime,
	})
	if err == nil {
		t.Fatal("CreateResource() accepted stale terminal submission timestamp")
	}
	operation, loadErr := store.GetOperation(context.Background(), "operation-stale-submission")
	if loadErr != nil {
		t.Fatalf("GetOperation() error = %v", loadErr)
	}
	if operation.Operation.IsTerminal() {
		t.Fatalf("operation state = %s, want nonterminal", operation.Operation.State())
	}
	execution, loadErr := store.GetExecution(context.Background(), "operation-stale-submission")
	if loadErr != nil || execution.State != application.AttemptUnknown {
		t.Fatalf("execution error=%v state=%s, want Unknown", loadErr, execution.State)
	}
}

func TestDeleteIdempotencyReusesLogicalOperation(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-delete-idempotent")
	provider := provisioningfake.New(provisioningfake.ModeSynchronous)
	service, _, _, _ := newService(t, ref, provider)
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-delete-idempotent", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-create-delete-idempotent", EventID: "event-create-delete-idempotent", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	command := application.DeleteResourceCommand{
		ID: created.Resource.Resource.ID(), ExpectedGeneration: created.Resource.Resource.Generation(),
		OperationID: "operation-delete-idempotent", EventID: "event-delete-idempotent", RequestedAt: applicationTime.Add(time.Minute),
		IdempotencyKey: "delete-key", Fingerprint: "delete-fingerprint",
	}
	first, err := service.DeleteResource(context.Background(), command)
	if err != nil {
		t.Fatalf("first DeleteResource() error = %v", err)
	}
	second, err := service.DeleteResource(context.Background(), command)
	if err != nil || !second.Replay || second.Operation.ID() != first.Operation.ID() {
		t.Fatalf("replay error=%v replay=%t operation=%q", err, second.Replay, second.Operation.ID())
	}
	if provider.SubmissionCount(command.OperationID) != 1 {
		t.Fatalf("submission count = %d, want 1", provider.SubmissionCount(command.OperationID))
	}
}

func TestObserveOperationUsesSubmittedIntentSnapshot(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-intent-snapshot")
	service, store, _, resolver := newService(t, ref, provisioningfake.New(provisioningfake.ModeSynchronous))
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-intent-snapshot", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: mustSpecValue(t, "generation-1"), OperationID: "operation-create-intent-snapshot", EventID: "event-create-intent-snapshot", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	handle, _ := provisioning.NewExecutionHandle("intent-snapshot")
	provider := &scriptedProvisioner{
		submit:  provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle}}},
		observe: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateRunning, Handle: &handle}},
	}
	resolver.Providers[ref] = provider
	updated, err := service.UpdateResource(context.Background(), application.UpdateResourceCommand{
		ID: created.Resource.Resource.ID(), ExpectedGeneration: 1, Spec: mustSpecValue(t, "generation-2"),
		OperationID: "operation-update-intent-snapshot", EventID: "event-update-intent-snapshot", RequestedAt: applicationTime.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("UpdateResource() error = %v", err)
	}
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		record, loadErr := tx.Resources().GetResource(context.Background(), created.Resource.Resource.ID())
		if loadErr != nil {
			return loadErr
		}
		if updateErr := record.Resource.UpdateSpec(mustSpecValue(t, "generation-3"), applicationTime.Add(2*time.Minute)); updateErr != nil {
			return updateErr
		}
		return tx.Resources().SaveResource(context.Background(), record, record.Version)
	}); err != nil {
		t.Fatalf("advance resource generation error = %v", err)
	}
	if _, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{OperationID: updated.Operation.ID(), ObservedAt: applicationTime.Add(3 * time.Minute)}); err != nil {
		t.Fatalf("ObserveOperation() error = %v", err)
	}
	if provider.lastObservation.TargetGeneration != 2 || provider.lastObservation.Spec.Values()["intent"] != "generation-2" {
		t.Fatalf("observed generation=%d spec=%v, want generation 2 intent", provider.lastObservation.TargetGeneration, provider.lastObservation.Spec.Values())
	}
}

func TestDriftDoesNotEraseNewerGenerationPending(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-stale-drift")
	service, store, _, resolver := newService(t, ref, provisioningfake.New(provisioningfake.ModeSynchronous))
	created, err := service.CreateResource(context.Background(), application.CreateResourceCommand{
		ID: "resource-stale-drift", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: mustSpecValue(t, "generation-1"), OperationID: "operation-create-stale-drift", EventID: "event-create-stale-drift", RequestedAt: applicationTime,
	})
	if err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	handle, _ := provisioning.NewExecutionHandle("stale-drift")
	provider := &scriptedProvisioner{submit: provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle}}}}
	resolver.Providers[ref] = provider
	updated, err := service.UpdateResource(context.Background(), application.UpdateResourceCommand{
		ID: created.Resource.Resource.ID(), ExpectedGeneration: 1, Spec: mustSpecValue(t, "generation-2"),
		OperationID: "operation-update-stale-drift", EventID: "event-update-stale-drift", RequestedAt: applicationTime.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("UpdateResource() error = %v", err)
	}
	if err := store.Within(context.Background(), func(tx application.UnitOfWork) error {
		record, loadErr := tx.Resources().GetResource(context.Background(), created.Resource.Resource.ID())
		if loadErr != nil {
			return loadErr
		}
		if updateErr := record.Resource.UpdateSpec(mustSpecValue(t, "generation-3"), applicationTime.Add(2*time.Minute)); updateErr != nil {
			return updateErr
		}
		return tx.Resources().SaveResource(context.Background(), record, record.Version)
	}); err != nil {
		t.Fatalf("advance generation error = %v", err)
	}
	provider.mu.Lock()
	provider.observe = provisioning.ExecutionObservation{
		Execution:  &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle},
		Resource:   provisioning.ResourceObservation{Presence: provisioning.ResourcePresencePresent, Readiness: provisioning.ResourceReadinessReady, Drift: provisioning.ResourceDriftDrifted},
		ObservedAt: applicationTime.Add(3 * time.Minute),
	}
	provider.mu.Unlock()
	result, err := service.ObserveOperation(context.Background(), application.ObserveOperationCommand{OperationID: updated.Operation.ID(), ObservedAt: applicationTime.Add(4 * time.Minute)})
	if err != nil {
		t.Fatalf("ObserveOperation() error = %v", err)
	}
	if reason := applicationConditionReason(t, result.Resource.Status, "Reconciled"); reason != "NewerGenerationPending" {
		t.Fatalf("Reconciled reason = %q, want NewerGenerationPending", reason)
	}
	assertApplicationCondition(t, result.Resource.Status, "Drifted", domain.ConditionStatusTrue)
}

type scriptedProvisioner struct {
	mu              sync.Mutex
	submit          provisioning.Submission
	submitErr       error
	observe         provisioning.ExecutionObservation
	observeErr      error
	submissions     int
	observations    int
	lastObservation provisioning.ObservationRequest
}

func (p *scriptedProvisioner) Capabilities() []provisioning.ProvisionerCapability { return nil }

func (p *scriptedProvisioner) Submit(_ context.Context, _ provisioning.ExecutionRequest) (provisioning.Submission, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.submissions++
	return p.submit, p.submitErr
}

func (p *scriptedProvisioner) Observe(_ context.Context, request provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.observations++
	p.lastObservation = request
	return p.observe, p.observeErr
}

type blockingSubmitProvisioner struct {
	entered      chan struct{}
	release      chan struct{}
	submission   provisioning.Submission
	submissions  int
	observations int
}

func (p *blockingSubmitProvisioner) Capabilities() []provisioning.ProvisionerCapability { return nil }

func (p *blockingSubmitProvisioner) Submit(_ context.Context, _ provisioning.ExecutionRequest) (provisioning.Submission, error) {
	p.submissions++
	close(p.entered)
	<-p.release
	return p.submission, nil
}

func (p *blockingSubmitProvisioner) Observe(_ context.Context, _ provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	p.observations++
	return provisioning.ExecutionObservation{}, nil
}

type concurrentObservationProvisioner struct {
	mu       sync.Mutex
	handle   provisioning.ExecutionHandle
	observed int
	release  chan struct{}
}

func newConcurrentObservationProvisioner(handle provisioning.ExecutionHandle) *concurrentObservationProvisioner {
	return &concurrentObservationProvisioner{handle: handle, release: make(chan struct{})}
}

func (p *concurrentObservationProvisioner) Capabilities() []provisioning.ProvisionerCapability {
	return nil
}

func (p *concurrentObservationProvisioner) Submit(_ context.Context, _ provisioning.ExecutionRequest) (provisioning.Submission, error) {
	return provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &p.handle}}}, nil
}

func (p *concurrentObservationProvisioner) Observe(_ context.Context, _ provisioning.ObservationRequest) (provisioning.ExecutionObservation, error) {
	p.mu.Lock()
	p.observed++
	if p.observed == 2 {
		close(p.release)
	}
	p.mu.Unlock()
	<-p.release
	return provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &p.handle}}, nil
}

func assertApplicationCondition(t *testing.T, status domain.ResourceStatus, typeName string, want domain.ConditionStatus) {
	t.Helper()
	for _, condition := range status.Conditions() {
		if condition.Type() == typeName {
			if condition.Status() != want {
				t.Fatalf("condition %s = %s, want %s", typeName, condition.Status(), want)
			}
			return
		}
	}
	t.Fatalf("condition %s missing", typeName)
}

func applicationConditionReason(t *testing.T, status domain.ResourceStatus, typeName string) string {
	t.Helper()
	for _, condition := range status.Conditions() {
		if condition.Type() == typeName {
			return condition.Reason()
		}
	}
	t.Fatalf("condition %s missing", typeName)
	return ""
}

func mustSpecValue(t *testing.T, intent string) domain.ResourceSpec {
	t.Helper()
	spec, err := domain.NewResourceSpec(map[string]any{"intent": intent})
	if err != nil {
		t.Fatalf("NewResourceSpec() error = %v", err)
	}
	return spec
}

func newService(t *testing.T, ref application.ProvisionerRef, provider provisioning.Provisioner) (*application.Service, *fake.Store, *fake.Selector, *fake.Resolver) {
	t.Helper()
	typeValue, err := domain.NewResourceType(provisioningfake.ResourceType(), "fake resource", []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		t.Fatalf("NewResourceType() error = %v", err)
	}
	store := fake.NewStore()
	selector := &fake.Selector{Ref: ref}
	resolver := &fake.Resolver{Providers: map[application.ProvisionerRef]provisioning.Provisioner{ref: provider}}
	service, err := application.NewService(fake.Catalog{Types: map[domain.ResourceTypeRef]domain.ResourceType{typeValue.Ref(): typeValue}}, selector, resolver, store)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	return service, store, selector, resolver
}

func mustProvisionerRef(t *testing.T, value string) application.ProvisionerRef {
	t.Helper()
	ref, err := application.NewProvisionerRef(value)
	if err != nil {
		t.Fatalf("NewProvisionerRef() error = %v", err)
	}
	return ref
}

func testSpec(t *testing.T) domain.ResourceSpec {
	t.Helper()
	spec, err := domain.NewResourceSpec(map[string]any{"intent": "test"})
	if err != nil {
		t.Fatalf("NewResourceSpec() error = %v", err)
	}
	return spec
}
