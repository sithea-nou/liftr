// SPDX-License-Identifier: Apache-2.0

package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
)

func applicationReadyFacts() provisioning.ResourceObservation {
	return provisioning.ResourceObservation{Presence: provisioning.ResourcePresencePresent, Readiness: provisioning.ResourceReadinessReady, Drift: provisioning.ResourceDriftInSync}
}

func TestStaleSubmissionEvidenceDoesNotApplyEagerly(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-eager-stale")
	handle, _ := provisioning.NewExecutionHandle("eager-stale")
	provider := &scriptedProvisioner{
		submit:    provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateUnknown, Handle: &handle}, ObservedAt: applicationTime.Add(10 * time.Minute)}},
		submitErr: provisioning.ErrAmbiguousSubmission,
	}
	service, store, _, _ := newService(t, ref, provider)
	command := application.CreateResourceCommand{Actor: fake.Principal("tester"),
		ID: "resource-eager-stale", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-eager-stale", EventID: "event-eager-stale", RequestedAt: applicationTime,
	}
	if _, err := service.CreateResource(context.Background(), command); err == nil {
		t.Fatal("CreateResource() succeeded despite ambiguous submission")
	}
	execution, err := store.GetExecution(context.Background(), command.OperationID)
	if err != nil || execution.State != application.AttemptUnknown {
		t.Fatalf("execution error=%v state=%s, want Unknown", err, execution.State)
	}
	if execution.AcceptanceConfirmed {
		t.Fatal("execution acceptance confirmed, want false")
	}
	if !execution.LastObservedAt.Equal(applicationTime.Add(10 * time.Minute)) {
		t.Fatalf("execution LastObservedAt=%v, want T+10m", execution.LastObservedAt)
	}
	provider.mu.Lock()
	provider.observe = provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationNotFound, Resource: provisioning.ResourceObservation{
		Presence: provisioning.ResourcePresenceUnknown, Readiness: provisioning.ResourceReadinessUnknown, Drift: provisioning.ResourceDriftUnknown,
	}, ObservedAt: applicationTime.Add(20 * time.Minute)}
	provider.mu.Unlock()
	if _, err := service.RecoverOperation(context.Background(), command.OperationID, applicationTime.Add(20*time.Minute)); err != nil {
		t.Fatalf("RecoverOperation() error = %v", err)
	}
	provider.mu.Lock()
	provider.submit = provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle}, Resource: applicationReadyFacts(), ObservedAt: applicationTime.Add(5 * time.Minute)}}
	provider.mu.Unlock()
	if _, err := service.DispatchOperation(context.Background(), command.OperationID); err == nil {
		t.Fatal("DispatchOperation() succeeded despite stale submission evidence")
	}
	execution, err = store.GetExecution(context.Background(), command.OperationID)
	if err != nil {
		t.Fatalf("GetExecution() error = %v", err)
	}
	if execution.State != application.AttemptUnknown {
		t.Fatalf("execution state = %s, want Unknown", execution.State)
	}
	if execution.Correlation != provisioning.RequestCorrelationUnknown {
		t.Fatalf("execution correlation = %s, want Unknown", execution.Correlation)
	}
	if execution.LastFailure == nil || execution.LastFailure.Reason != "StaleSubmissionEvidence" {
		t.Fatalf("execution failure = %v, want StaleSubmissionEvidence", execution.LastFailure)
	}
	if !execution.LastObservedAt.Equal(applicationTime.Add(20 * time.Minute)) {
		t.Fatalf("execution LastObservedAt = %v, want T+20m (no regression to stale evidence)", execution.LastObservedAt)
	}
	operation, err := store.GetOperation(context.Background(), command.OperationID)
	if err != nil || operation.Operation.IsTerminal() {
		t.Fatalf("operation error=%v terminal=%t, want nonterminal", err, operation.Operation.IsTerminal())
	}
}

func TestFreshSubmissionEvidenceCompletesEagerly(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-eager-fresh-control")
	handle, _ := provisioning.NewExecutionHandle("eager-fresh-control")
	provider := &scriptedProvisioner{
		submit:    provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateUnknown, Handle: &handle}, ObservedAt: applicationTime.Add(10 * time.Minute)}},
		submitErr: provisioning.ErrAmbiguousSubmission,
	}
	service, store, _, _ := newService(t, ref, provider)
	command := application.CreateResourceCommand{Actor: fake.Principal("tester"),
		ID: "resource-eager-fresh-control", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-eager-fresh-control", EventID: "event-eager-fresh-control", RequestedAt: applicationTime,
	}
	if _, err := service.CreateResource(context.Background(), command); err == nil {
		t.Fatal("CreateResource() succeeded despite ambiguous submission")
	}
	provider.mu.Lock()
	provider.observe = provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationNotFound, Resource: provisioning.ResourceObservation{
		Presence: provisioning.ResourcePresenceUnknown, Readiness: provisioning.ResourceReadinessUnknown, Drift: provisioning.ResourceDriftUnknown,
	}, ObservedAt: applicationTime.Add(20 * time.Minute)}
	provider.mu.Unlock()
	if _, err := service.RecoverOperation(context.Background(), command.OperationID, applicationTime.Add(20*time.Minute)); err != nil {
		t.Fatalf("RecoverOperation() error = %v", err)
	}
	provider.mu.Lock()
	provider.submit = provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle}, Resource: applicationReadyFacts(), ObservedAt: applicationTime.Add(25 * time.Minute)}}
	provider.mu.Unlock()
	if _, err := service.DispatchOperation(context.Background(), command.OperationID); err != nil {
		t.Fatalf("DispatchOperation() error = %v", err)
	}
	operation, err := store.GetOperation(context.Background(), command.OperationID)
	if err != nil || operation.Operation.State() != domain.OperationStateSucceeded {
		t.Fatalf("operation error=%v state=%s, want Succeeded", err, operation.Operation.State())
	}
}

func TestCreateResourceNulFingerprintConflict(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-nul")
	handle, _ := provisioning.NewExecutionHandle("nul-fingerprint")
	provider := &scriptedProvisioner{submit: provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateAccepted, Handle: &handle}}}}
	firstType, err := domain.NewResourceType(domain.ResourceTypeRef{Name: "app", Version: "v1"}, "app resource", []domain.Capability{domain.CapabilityCreate})
	if err != nil {
		t.Fatal(err)
	}
	secondType, err := domain.NewResourceType(domain.ResourceTypeRef{Name: "web\x00app", Version: "v1"}, "nul resource", []domain.Capability{domain.CapabilityCreate})
	if err != nil {
		t.Fatal(err)
	}
	store := fake.NewStore()
	selector := &fake.Selector{Ref: ref}
	resolver := &fake.Resolver{Providers: map[application.ProvisionerRef]provisioning.Provisioner{ref: provider}}
	service, err := application.NewService(fake.Catalog{Types: map[domain.ResourceTypeRef]domain.ResourceType{firstType.Ref(): firstType, secondType.Ref(): secondType}}, selector, resolver, store, fake.AllowAll{})
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	service.EnableEagerExecutionForTesting()
	command := application.CreateResourceCommand{Actor: fake.Principal("tester"),
		ID: "r\x00web", Type: firstType.Ref(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-nul-1", EventID: "event-nul-1", RequestedAt: applicationTime, IdempotencyKey: "nul-key",
	}
	if _, err := service.CreateResource(context.Background(), command); err != nil {
		t.Fatalf("first CreateResource() error = %v", err)
	}
	conflicting := application.CreateResourceCommand{Actor: fake.Principal("tester"),
		ID: "r", Type: secondType.Ref(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-nul-2", EventID: "event-nul-2", RequestedAt: applicationTime, IdempotencyKey: "nul-key",
	}
	if _, err := service.CreateResource(context.Background(), conflicting); !errors.Is(err, application.ErrIdempotencyConflict) {
		t.Fatalf("conflicting CreateResource() error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestLifecycleEventIDsUseCanonicalInternalLabels(t *testing.T) {
	ref := mustProvisionerRef(t, "provider-event-ids")
	handle, _ := provisioning.NewExecutionHandle("event-ids")
	provider := &scriptedProvisioner{submit: provisioning.Submission{Observation: provisioning.ExecutionObservation{Execution: &provisioning.Execution{State: provisioning.ExecutionStateSucceeded, Handle: &handle}, Resource: applicationReadyFacts(), ObservedAt: applicationTime.Add(time.Hour)}}}
	service, store, _, _ := newService(t, ref, provider)
	command := application.CreateResourceCommand{Actor: fake.Principal("tester"),
		ID: "resource-event-ids", Type: provisioningfake.ResourceType(), Owner: domain.OwnerRef{Kind: "team", ID: "platform"},
		Spec: testSpec(t), OperationID: "operation-event-ids", EventID: "event-event-ids", RequestedAt: applicationTime,
	}
	if _, err := service.CreateResource(context.Background(), command); err != nil {
		t.Fatalf("CreateResource() error = %v", err)
	}
	phases := []struct {
		phase      domain.OperationPhase
		transition string
	}{
		{domain.OperationPhaseValidating, application.InternalTransitionLabel(domain.OperationPhaseValidating)},
		{domain.OperationPhasePlanning, application.InternalTransitionLabel(domain.OperationPhasePlanning)},
		{domain.OperationPhaseApplying, application.InternalTransitionLabel(domain.OperationPhaseApplying)},
	}
	for _, entry := range phases {
		if _, err := store.GetEvent(context.Background(), application.InternalEventID(command.OperationID, entry.transition)); err != nil {
			t.Fatalf("phase %s event missing: %v", entry.phase, err)
		}
	}
	if _, err := store.GetEvent(context.Background(), application.InternalEventID(command.OperationID, "succeeded")); err != nil {
		t.Fatalf("succeeded event missing: %v", err)
	}
}
