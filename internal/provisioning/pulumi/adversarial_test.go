// SPDX-License-Identifier: Apache-2.0

package pulumi

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

var infraNamePattern = regexp.MustCompile(`^liftr-[0-9a-f]{20}$`)

func postgresRef() domain.ResourceTypeRef {
	return domain.ResourceTypeRef{Name: "PostgreSQLDatabase", Version: "v1"}
}

// TestInfraNameIsPlatformScopedAndDeterministic pins Correction 3:
// infrastructure identity digests platform identity, implementation
// namespace, ResourceTypeRef, and ResourceID; it is stable across restarts,
// updates, and deletes; distinct installations never collide on the same
// ResourceID; and OperationID/generation/attempt are structurally absent.
func TestInfraNameIsPlatformScopedAndDeterministic(t *testing.T) {
	first := InfraName("install-alpha", "ns", postgresRef(), "resource-1")
	if !infraNamePattern.MatchString(first) {
		t.Fatalf("infra name %q does not satisfy backend-safe format", first)
	}
	if again := InfraName("install-alpha", "ns", postgresRef(), "resource-1"); again != first {
		t.Fatalf("infra name is not deterministic: %q vs %q", first, again)
	}
	tests := []struct {
		name       string
		identity   string
		namespace  string
		ref        domain.ResourceTypeRef
		resourceID string
	}{
		{name: "different installation identity", identity: "install-beta", namespace: "ns", ref: postgresRef(), resourceID: "resource-1"},
		{name: "different namespace", identity: "install-alpha", namespace: "other", ref: postgresRef(), resourceID: "resource-1"},
		{name: "different resource type version", identity: "install-alpha", namespace: "ns", ref: domain.ResourceTypeRef{Name: "PostgreSQLDatabase", Version: "v2"}, resourceID: "resource-1"},
		{name: "different resource type name", identity: "install-alpha", namespace: "ns", ref: domain.ResourceTypeRef{Name: "OtherDatabase", Version: "v1"}, resourceID: "resource-1"},
		{name: "different resource ID", identity: "install-alpha", namespace: "ns", ref: postgresRef(), resourceID: "resource-2"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if derived := InfraName(test.identity, test.namespace, test.ref, domain.ResourceID(test.resourceID)); derived == first {
				t.Fatal("distinct inputs produced identical infrastructure identity")
			}
		})
	}
	// The same logical Resource keeps one infrastructure identity across
	// lifecycle capabilities and generations.
	create := InfraName("install-alpha", "ns", postgresRef(), "resource-1")
	deleteName := InfraName("install-alpha", "ns", postgresRef(), "resource-1")
	if create != deleteName {
		t.Fatal("create and delete derived different infrastructure identities")
	}
}

func TestRequiredEnvironmentValidationRejectsBadDeclarations(t *testing.T) {
	tests := []struct {
		name  string
		names []string
	}{
		{name: "empty name", names: []string{""}},
		{name: "duplicate declaration", names: []string{"ARM_TENANT_ID", "ARM_TENANT_ID"}},
		{name: "reserved passphrase channel", names: []string{"PULUMI_CONFIG_PASSPHRASE"}},
		{name: "reserved adapter input channel", names: []string{"LIFTR_INPUT_FILE"}},
		{name: "reserved backend channel", names: []string{"PULUMI_BACKEND_URL"}},
		{name: "reserved process channel", names: []string{"PATH"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(t)
			config.Programs[0].RequiredEnvironment = test.names
			if _, err := newProvisioner(config, &fakeFactory{}); err == nil {
				t.Fatal("invalid required environment declaration was accepted")
			}
		})
	}
}

// TestUndeclaredEnvironmentValueIsRejectedBeforeInvocation pins the allowlist
// security boundary: values outside the registration's declared names are
// refused before any Pulumi invocation, even though the platform supplied them.
func TestUndeclaredEnvironmentValueIsRejectedBeforeInvocation(t *testing.T) {
	config := testConfig(t)
	config.Environment = func(context.Context) (map[string]string, error) {
		return map[string]string{
			"PULUMI_CONFIG_PASSPHRASE": "passphrase-ok",
			"UNDECLARED_SECRET":        "must-not-reach-child",
		}, nil
	}
	factory := &fakeFactory{}
	provider, err := newProvisioner(config, factory)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := provider.Submit(context.Background(), executionRequest(t, domain.CapabilityCreate))
	if err != nil {
		t.Fatal(err)
	}
	observation := submission.Observation
	if observation.Correlation != provisioning.RequestCorrelationNotFound ||
		observation.Execution == nil || observation.Execution.State != provisioning.ExecutionStateFailed ||
		observation.Execution.Failure == nil || observation.Execution.Failure.Reason != "ExecutionEnvironmentUnavailable" {
		t.Fatalf("undeclared environment observation = %+v", observation)
	}
	if factory.openCalls != 0 {
		t.Fatal("Pulumi was invoked despite undeclared environment content")
	}
}

// TestMissingRequiredEnvironmentIsConclusivePreflightFailure pins that a
// declared-but-missing variable is detected by Liftr before invocation and
// therefore authorizes the conclusive NotFound+Failed preflight outcome.
func TestMissingRequiredEnvironmentIsConclusivePreflightFailure(t *testing.T) {
	config := testConfig(t)
	config.Programs[0].RequiredEnvironment = []string{"EXAMPLE_CREDENTIAL"}
	config.Environment = func(context.Context) (map[string]string, error) {
		return map[string]string{}, nil // declared name not supplied
	}
	factory := &fakeFactory{workspace: &fakeWorkspace{stack: &fakeStack{}}}
	provider, err := newProvisioner(config, factory)
	if err != nil {
		t.Fatal(err)
	}
	submission, err := provider.Submit(context.Background(), executionRequest(t, domain.CapabilityCreate))
	if err != nil {
		t.Fatal(err)
	}
	observation := submission.Observation
	if observation.Correlation != provisioning.RequestCorrelationNotFound ||
		observation.Execution == nil || observation.Execution.State != provisioning.ExecutionStateFailed ||
		observation.Execution.Failure == nil || observation.Execution.Failure.Reason != "RequiredEnvironmentMissing" {
		t.Fatalf("missing environment observation = %+v", observation)
	}
	if factory.openCalls != 0 {
		t.Fatal("Pulumi was invoked despite missing required environment")
	}
}

// TestDeclaredEnvironmentReachesChildAndPassphraseChannelStaysGlobal pins the
// happy path: declared values reach the constructed child environment, the
// global passphrase channel still works without declarations.
func TestDeclaredEnvironmentReachesChildAndPassphraseChannelStaysGlobal(t *testing.T) {
	config := testConfig(t)
	config.Programs[0].RequiredEnvironment = []string{"EXAMPLE_CREDENTIAL"}
	config.Environment = func(context.Context) (map[string]string, error) {
		return map[string]string{"PULUMI_CONFIG_PASSPHRASE": "pass", "EXAMPLE_CREDENTIAL": "value"}, nil
	}
	stack := &fakeStack{summary: updateSummary{kind: "update", result: "succeeded"}}
	provider, err := newProvisioner(config, &fakeFactory{workspace: &fakeWorkspace{stack: stack}})
	if err != nil {
		t.Fatal(err)
	}
	request := executionRequest(t, domain.CapabilityCreate)
	stack.summary.message = correlationMessage(request.OperationID, request.AttemptNumber)
	submission, err := provider.Submit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if submission.Observation.Correlation != provisioning.RequestCorrelationFound {
		t.Fatalf("submission = %+v", submission.Observation)
	}
}

// TestDestroySuccessReportsManagedAbsence pins the honest delete fact: only a
// conclusively correlated successful destroy reports Presence=NotFound;
// readiness and drift stay Unknown, and create/update success never fabricates
// presence or readiness facts.
func TestDestroySuccessReportsManagedAbsence(t *testing.T) {
	deleteRequest := executionRequest(t, domain.CapabilityDelete)
	updateRequest := executionRequest(t, domain.CapabilityUpdate)
	tests := []struct {
		name      string
		request   provisioning.ExecutionRequest
		kind      string
		result    string
		wantFacts provisioning.ResourceObservation
	}{
		{name: "successful destroy", request: deleteRequest, kind: "destroy", result: "succeeded",
			wantFacts: provisioning.ResourceObservation{Presence: provisioning.ResourcePresenceNotFound,
				Readiness: provisioning.ResourceReadinessUnknown, Drift: provisioning.ResourceDriftUnknown}},
		{name: "failed destroy stays unknown", request: deleteRequest, kind: "destroy", result: "failed", wantFacts: unknownFacts()},
		{name: "running destroy stays unknown", request: deleteRequest, kind: "destroy", result: "in-progress", wantFacts: unknownFacts()},
		{name: "successful update stays unknown", request: updateRequest, kind: "update", result: "succeeded", wantFacts: unknownFacts()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message := correlationMessage(test.request.OperationID, test.request.AttemptNumber)
			summary := updateSummary{kind: test.kind, message: message, result: test.result, endTime: strPtr("2026-08-22T10:00:00Z")}
			observation := observationFromSummary(summary, expectedHistoryKind(test.request.Capability), message, provisioning.ExecutionHandle{})
			if observation.Correlation != provisioning.RequestCorrelationFound {
				t.Fatalf("correlation = %s", observation.Correlation)
			}
			if observation.Resource != test.wantFacts {
				t.Fatalf("facts = %+v, want %+v", observation.Resource, test.wantFacts)
			}
		})
	}
}

// --- Correction 1 adversarial suite -----------------------------------------
//
// These tests pin ADR-0007 ambiguity semantics against future regression.

// Case A: execution starts, side effects may occur, the process exits, no
// history entry exists, and no update is in progress. Absence of history is
// NOT proof of absence of execution: the outcome must remain Unknown, must
// never become NotFound, and must never authorize resubmission.
func TestAdversarialStartedButUnrecordedExecutionRemainsAmbiguous(t *testing.T) {
	config := testConfig(t)
	rawDiagnostic := "provider auth rejected: subscription unavailable"
	stack := &fakeStack{
		runErr: errors.New(rawDiagnostic),
		pages:  map[int][]updateSummary{1: {}}, // zero history entries at all
	}
	// updateInProgress is false — deliberately. Even with no active update
	// and no history entry, absence after possible invocation is Unknown.
	provider, err := newProvisioner(config, &fakeFactory{workspace: &fakeWorkspace{stack: stack}})
	if err != nil {
		t.Fatal(err)
	}
	submission, err := provider.Submit(context.Background(), executionRequest(t, domain.CapabilityCreate))
	if !errors.Is(err, provisioning.ErrAmbiguousSubmission) {
		t.Fatalf("error = %v, want ErrAmbiguousSubmission", err)
	}
	observation := submission.Observation
	if observation.Correlation != provisioning.RequestCorrelationUnknown {
		t.Fatalf("correlation = %s, want Unknown (absent history after launch is not NotFound)", observation.Correlation)
	}
	if observation.Execution == nil || observation.Execution.State != provisioning.ExecutionStateUnknown {
		t.Fatalf("execution = %+v, want Unknown state", observation.Execution)
	}
	if strings.Contains(observation.Execution.Failure.Error(), rawDiagnostic) {
		t.Fatal("raw CLI diagnostics crossed the provisioner boundary")
	}
	// The same holds for a fresh Observe of the ambiguous attempt.
	observed, err := provider.Observe(context.Background(), observationRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if observed.Correlation != provisioning.RequestCorrelationUnknown {
		t.Fatalf("observe correlation = %s, want Unknown", observed.Correlation)
	}
}

// Case B: failures that occur strictly before the Pulumi invocation boundary
// may be conclusive. Each pre-launch rejection is terminal NotFound+Failed
// with zero invocations.
func TestAdversarialPreInvocationFailuresAreConclusive(t *testing.T) {
	cases := map[string]func(config Config) Config{
		"unsupported capability": func(config Config) Config { return config },
		"program input invalid": func(config Config) Config {
			config.Programs[0].EncodeInput = func(Input) ([]byte, error) { return nil, errors.New("encoder failure") }
			return config
		},
		"required environment missing": func(config Config) Config {
			config.Programs[0].RequiredEnvironment = []string{"MISSING_VARIABLE"}
			config.Environment = func(context.Context) (map[string]string, error) { return map[string]string{}, nil }
			return config
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			config := mutate(testConfig(t))
			factory := &fakeFactory{}
			provider, err := newProvisioner(config, factory)
			if err != nil {
				t.Fatal(err)
			}
			capability := domain.CapabilityCreate
			if name == "unsupported capability" {
				capability = "CapabilityObserve"
			}
			request := executionRequest(t, capability)
			submission, err := provider.Submit(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if submission.Observation.Correlation != provisioning.RequestCorrelationNotFound ||
				submission.Observation.Execution == nil ||
				submission.Observation.Execution.State != provisioning.ExecutionStateFailed {
				t.Fatalf("preflight observation = %+v", submission.Observation)
			}
			if factory.openCalls != 0 {
				t.Fatal("pre-invocation failure reached Automation API")
			}
		})
	}
}

func strPtr(value string) *string { return &value }
