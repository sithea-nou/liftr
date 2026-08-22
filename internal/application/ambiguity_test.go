// SPDX-License-Identifier: Apache-2.0

package application_test

import (
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

// ambiguityFixture builds a nonterminal execution/attempt pair as it exists
// after a dispatched submission attempt.
func ambiguityFixture(t *testing.T) (application.ProvisioningExecutionRecord, application.SubmissionAttemptRecord) {
	t.Helper()
	handle, err := provisioning.NewExecutionHandle("ambiguity-test-handle")
	if err != nil {
		t.Fatal(err)
	}
	execution := application.ProvisioningExecutionRecord{
		OperationID:      "op-ambiguity",
		ResourceID:       "resource-1",
		ResourceType:     domain.ResourceTypeRef{Name: "Widget", Version: "v1"},
		Capability:       domain.CapabilityCreate,
		TargetGeneration: 1,
		Handle:           &handle,
		State:            application.AttemptDispatching,
		CurrentAttempt:   1,
		Version:          1,
	}
	attempt := application.SubmissionAttemptRecord{
		OperationID:     execution.OperationID,
		AttemptNumber:   1,
		State:           application.SubmissionAttemptLeased,
		DispatchMessage: "dispatch:op-ambiguity:1",
	}
	return execution, attempt
}

func ambiguousObservation(handle provisioning.ExecutionHandle) provisioning.ExecutionObservation {
	return provisioning.ExecutionObservation{
		Correlation: provisioning.RequestCorrelationUnknown,
		Execution:   &provisioning.Execution{State: provisioning.ExecutionStateUnknown, Handle: &handle},
	}
}

// TestAmbiguousSubmissionNeverAuthorizesResubmission pins Correction 1 case
// C: an ambiguous outcome keeps the attempt Unknown, produces no terminal
// finish evidence, and never advances the attempt counter. Resubmission can
// only ever be authorized later by fresh conclusive observation evidence.
func TestAmbiguousSubmissionNeverAuthorizesResubmission(t *testing.T) {
	execution, attempt := ambiguityFixture(t)
	handle, err := provisioning.NewExecutionHandle("ambiguity-test-handle")
	if err != nil {
		t.Fatal(err)
	}

	next, resultAttempt, outcome, finish, err := application.InterpretSubmission(
		execution, attempt, provisioning.Submission{Observation: ambiguousObservation(handle)}, provisioning.ErrAmbiguousSubmission, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if outcome != application.SubmissionOutcomeAmbiguous {
		t.Fatalf("outcome = %d, want ambiguous", outcome)
	}
	if finish != nil {
		t.Fatal("ambiguous submission produced terminal finish evidence")
	}
	if resultAttempt.State != application.SubmissionAttemptUnknown {
		t.Fatalf("attempt state = %s, want Unknown", resultAttempt.State)
	}
	if next.CurrentAttempt != execution.CurrentAttempt {
		t.Fatalf("ambiguous submission advanced the attempt counter to %d", next.CurrentAttempt)
	}
	if next.State != application.AttemptUnknown {
		t.Fatalf("execution state = %s, want Unknown", next.State)
	}
	if next.AcceptanceConfirmed {
		t.Fatal("ambiguous correlation confirmed acceptance")
	}

	// Re-observing the same ambiguity with Unknown correlation again must not
	// authorize resubmission either.
	_, _, observedOutcome, observedFinish, observeErr := application.InterpretObservation(
		next, resultAttempt, ambiguousObservation(handle), time.Now().Add(time.Second))
	if observeErr != nil {
		t.Fatal(observeErr)
	}
	if observedOutcome != application.ObservationOutcomeObserve {
		t.Fatalf("observation outcome = %d, want continue observing", observedOutcome)
	}
	if observedFinish != nil {
		t.Fatal("unknown observation produced terminal finish evidence")
	}
}

// TestFreshConclusiveNotFoundIsTheOnlyResubmissionPath pins Correction 1 case
// D: a fresh observation with explicit RequestCorrelationNotFound — and no
// previously confirmed acceptance — is the only evidence that advances to
// attempt+1 with Pending state; Found blocks resubmission even when Execution
// is nil.
func TestFreshConclusiveNotFoundIsTheOnlyResubmissionPath(t *testing.T) {
	execution, attempt := ambiguityFixture(t)
	handle, err := provisioning.NewExecutionHandle("ambiguity-test-handle")
	if err != nil {
		t.Fatal(err)
	}

	// Reality chain: an ambiguous submission first leaves the attempt
	// Unknown; only then can fresh evidence authorize resubmission.
	execution, attempt, _, _, err = application.InterpretSubmission(
		execution, attempt, provisioning.Submission{Observation: ambiguousObservation(handle)}, provisioning.ErrAmbiguousSubmission, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	notFound := provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationNotFound}
	retryExecution, retryAttempt, retryOutcome, retryFinish, err :=
		application.InterpretObservation(execution, attempt, notFound, time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if retryOutcome != application.ObservationOutcomeRetry {
		t.Fatalf("outcome = %d, want retry", retryOutcome)
	}
	if retryFinish != nil {
		t.Fatal("retry authorization carried terminal finish evidence")
	}
	if retryExecution.CurrentAttempt != execution.CurrentAttempt+1 || retryExecution.State != application.AttemptPending {
		t.Fatalf("resubmission state = attempt %d/%s, want %d/Pending",
			retryExecution.CurrentAttempt, retryExecution.State, execution.CurrentAttempt+1)
	}
	if retryAttempt.State != application.SubmissionAttemptNotFound {
		t.Fatalf("prior attempt resolved as %s, want NotFound", retryAttempt.State)
	}

	// Found without an Execution confirms acceptance but never authorizes a
	// new attempt.
	foundNilExecution := provisioning.ExecutionObservation{Correlation: provisioning.RequestCorrelationFound}
	blockedExecution, _, blockedOutcome, _, blockErr :=
		application.InterpretObservation(retryExecution, retryAttempt, foundNilExecution, time.Now().Add(2*time.Second))
	if blockErr != nil {
		t.Fatal(blockErr)
	}
	if blockedOutcome != application.ObservationOutcomeObserve {
		t.Fatalf("outcome = %d, want continue observing", blockedOutcome)
	}
	if blockedExecution.CurrentAttempt != retryExecution.CurrentAttempt {
		t.Fatalf("Found correlation advanced the attempt counter to %d", blockedExecution.CurrentAttempt)
	}
	if !blockedExecution.AcceptanceConfirmed {
		t.Fatal("Found correlation did not confirm acceptance")
	}
}
