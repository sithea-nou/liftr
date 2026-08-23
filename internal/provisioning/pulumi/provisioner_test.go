// SPDX-License-Identifier: Apache-2.0

package pulumi

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

var testResourceType = domain.ResourceTypeRef{Name: "Noop", Version: "v1"}

func TestSubmitMapsCapabilitiesAndCleansIsolatedWorkspace(t *testing.T) {
	tests := []struct {
		name        string
		capability  domain.Capability
		selectErr   error
		wantCreate  int
		wantUp      int
		wantDestroy int
		kind        string
	}{
		{name: "create", capability: domain.CapabilityCreate, selectErr: errStackNotFound, wantCreate: 1, wantUp: 1, kind: "update"},
		{name: "update", capability: domain.CapabilityUpdate, wantUp: 1, kind: "update"},
		{name: "delete", capability: domain.CapabilityDelete, wantDestroy: 1, kind: "destroy"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(t)
			stack := &fakeStack{summary: updateSummary{kind: test.kind, result: "succeeded"}}
			workspace := &fakeWorkspace{stack: stack, selectErr: test.selectErr}
			factory := &fakeFactory{workspace: workspace}
			provider, err := newProvisioner(config, factory)
			if err != nil {
				t.Fatal(err)
			}
			request := executionRequest(t, test.capability)
			stack.summary.message = correlationMessage(request.OperationID, request.AttemptNumber)
			submission, err := provider.Submit(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if submission.Observation.Correlation != provisioning.RequestCorrelationFound || submission.Observation.Execution.State != provisioning.ExecutionStateSucceeded {
				t.Fatalf("observation = %+v", submission.Observation)
			}
			if workspace.createCalls != test.wantCreate || stack.upCalls != test.wantUp || stack.destroyCalls != test.wantDestroy {
				t.Fatalf("create=%d up=%d destroy=%d", workspace.createCalls, stack.upCalls, stack.destroyCalls)
			}
			if factory.workDir == config.Programs[0].SourceDir {
				t.Fatal("shared source directory was used as a workspace")
			}
			if _, err := os.Stat(filepath.Dir(factory.workDir)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("workspace was not removed: %v", err)
			}
			if factory.environment["PULUMI_BACKEND_URL"] != config.BackendURL || factory.environment[inputEnvironment] == "" {
				t.Fatal("private workspace environment was not provided")
			}
			if !factory.secureWorkspace {
				t.Fatal("workspace or input permissions were not restrictive")
			}
		})
	}
}

func TestTerminalFailureIsCorrelatedWithoutRawDiagnostics(t *testing.T) {
	config := testConfig(t)
	rawSecret := "raw-cli-secret-value"
	request := executionRequest(t, domain.CapabilityCreate)
	summary := updateSummary{kind: "update", message: correlationMessage(request.OperationID, request.AttemptNumber), result: "failed"}
	stack := &fakeStack{summary: summary, pages: map[int][]updateSummary{1: {summary}}, runErr: errors.New(rawSecret)}
	provider, err := newProvisioner(config, &fakeFactory{workspace: &fakeWorkspace{stack: stack}})
	if err != nil {
		t.Fatal(err)
	}
	submission, err := provider.Submit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	observation := submission.Observation
	if observation.Correlation != provisioning.RequestCorrelationFound || observation.Execution == nil || observation.Execution.State != provisioning.ExecutionStateFailed {
		t.Fatalf("observation = %+v", observation)
	}
	if strings.Contains(observation.Execution.Failure.Error(), rawSecret) {
		t.Fatal("raw CLI diagnostics crossed the provisioner boundary")
	}
}

func TestExactRunningHistoryConfirmsAcceptedSubmission(t *testing.T) {
	config := testConfig(t)
	request := executionRequest(t, domain.CapabilityCreate)
	summary := updateSummary{kind: "update", message: correlationMessage(request.OperationID, request.AttemptNumber), result: "in-progress"}
	stack := &fakeStack{pages: map[int][]updateSummary{1: {summary}}, runErr: errors.New("connection interrupted")}
	provider, err := newProvisioner(config, &fakeFactory{workspace: &fakeWorkspace{stack: stack}})
	if err != nil {
		t.Fatal(err)
	}
	submission, err := provider.Submit(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if submission.Observation.Correlation != provisioning.RequestCorrelationFound || submission.Observation.Execution == nil || submission.Observation.Execution.State != provisioning.ExecutionStateRunning {
		t.Fatalf("submission = %+v", submission)
	}
}

func TestAmbiguousSubmitDoesNotLeakRawCLIOutput(t *testing.T) {
	config := testConfig(t)
	rawSecret := "raw-pulumi-output-secret"
	stack := &fakeStack{runErr: errors.New(rawSecret), pages: map[int][]updateSummary{1: {}}}
	provider, err := newProvisioner(config, &fakeFactory{workspace: &fakeWorkspace{stack: stack}})
	if err != nil {
		t.Fatal(err)
	}
	submission, err := provider.Submit(context.Background(), executionRequest(t, domain.CapabilityCreate))
	if !errors.Is(err, provisioning.ErrAmbiguousSubmission) || submission.Observation.Correlation != provisioning.RequestCorrelationUnknown {
		t.Fatalf("submission=%+v error=%v", submission, err)
	}
	if submission.Observation.Execution == nil || submission.Observation.Execution.Failure == nil || strings.Contains(submission.Observation.Execution.Failure.Error(), rawSecret) {
		t.Fatal("ambiguous submission leaked raw CLI output")
	}
}

func TestPreflightFailureIsTerminalNotFound(t *testing.T) {
	config := testConfig(t)
	config.Programs[0].EncodeInput = func(Input) ([]byte, error) { return nil, errors.New("sensitive value") }
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
	if observation.Correlation != provisioning.RequestCorrelationNotFound || observation.Execution == nil || observation.Execution.State != provisioning.ExecutionStateFailed {
		t.Fatalf("preflight observation = %+v", observation)
	}
	if factory.openCalls != 0 {
		t.Fatal("preflight failure invoked Pulumi")
	}
}

func TestObserveUsesExactBoundedHistoryCorrelation(t *testing.T) {
	config := testConfig(t)
	config.HistoryPageSize = 2
	config.HistoryMaximumPages = 2
	request := observationRequest(t)
	message := correlationMessage(request.OperationID, request.AttemptNumber)
	tests := []struct {
		name      string
		pages     map[int][]updateSummary
		want      provisioning.RequestCorrelation
		wantState provisioning.ExecutionState
		wantCalls int
	}{
		{name: "second page exact match", pages: map[int][]updateSummary{1: {{message: "other"}, {message: "other-2"}}, 2: {{kind: "update", message: message, result: "succeeded"}}}, want: provisioning.RequestCorrelationFound, wantState: provisioning.ExecutionStateSucceeded, wantCalls: 2},
		{name: "bounded absence", pages: map[int][]updateSummary{1: {{message: "other"}, {message: "other-2"}}, 2: {{message: "other-3"}, {message: "other-4"}}}, want: provisioning.RequestCorrelationUnknown, wantCalls: 2},
		{name: "wrong kind", pages: map[int][]updateSummary{1: {{kind: "destroy", message: message, result: "succeeded"}}}, want: provisioning.RequestCorrelationUnknown, wantCalls: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stack := &fakeStack{pages: test.pages}
			provider, err := newProvisioner(config, &fakeFactory{workspace: &fakeWorkspace{stack: stack}})
			if err != nil {
				t.Fatal(err)
			}
			observation, err := provider.Observe(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if observation.Correlation != test.want || stack.historyCalls != test.wantCalls {
				t.Fatalf("correlation=%s history calls=%d", observation.Correlation, stack.historyCalls)
			}
			if test.wantState != "" && (observation.Execution == nil || observation.Execution.State != test.wantState) {
				t.Fatalf("execution = %+v", observation.Execution)
			}
		})
	}
}

func TestMissingStackAfterPossibleSubmissionIsUnknown(t *testing.T) {
	config := testConfig(t)
	provider, err := newProvisioner(config, &fakeFactory{workspace: &fakeWorkspace{selectErr: errStackNotFound}})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := provider.Observe(context.Background(), observationRequest(t))
	if err != nil {
		t.Fatal(err)
	}
	if observation.Correlation != provisioning.RequestCorrelationUnknown || observation.Execution != nil {
		t.Fatalf("observation = %+v", observation)
	}
}

func TestConfigurationRequiresNoSecretInputAndStableSource(t *testing.T) {
	config := testConfig(t)
	config.Programs[0].SecretInputsUnsupported = false
	if _, err := newProvisioner(config, &fakeFactory{}); err == nil {
		t.Fatal("secret-bearing registration was accepted")
	}
	config = testConfig(t)
	config.Programs[0].SourceDigest = strings.Repeat("0", 64)
	if _, err := newProvisioner(config, &fakeFactory{}); err == nil {
		t.Fatal("mutable source registration was accepted")
	}
}

func TestSourceMutationFailsBeforeAutomationInvocation(t *testing.T) {
	config := testConfig(t)
	factory := &fakeFactory{workspace: &fakeWorkspace{stack: &fakeStack{}}}
	provider, err := newProvisioner(config, factory)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.Programs[0].SourceDir, "mutation"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	submission, err := provider.Submit(context.Background(), executionRequest(t, domain.CapabilityCreate))
	if err != nil {
		t.Fatal(err)
	}
	if submission.Observation.Correlation != provisioning.RequestCorrelationNotFound || submission.Observation.Execution == nil || submission.Observation.Execution.State != provisioning.ExecutionStateFailed {
		t.Fatalf("submission = %+v", submission)
	}
	if factory.openCalls != 0 {
		t.Fatal("mutated source reached Automation API")
	}
}

func TestStaleWorkspaceCleanupIsPrefixAndAgeBounded(t *testing.T) {
	root := t.TempDir()
	stale := filepath.Join(root, "liftr-pulumi-stale")
	recent := filepath.Join(root, "liftr-pulumi-recent")
	unrelated := filepath.Join(root, "unrelated")
	for _, path := range []string{stale, recent, unrelated} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	active := filepath.Join(root, "liftr-pulumi-active")
	if err := os.Mkdir(active, 0o700); err != nil {
		t.Fatal(err)
	}
	activeLock, err := acquireWorkspaceLock(active)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseWorkspaceLock(activeLock)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(active, old, old); err != nil {
		t.Fatal(err)
	}
	if err := cleanupStaleWorkspaces(root, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale Liftr workspace was retained")
	}
	for _, path := range []string{recent, unrelated, active} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("safe directory was removed: %v", err)
		}
	}
}

func TestOpaqueIdentityDoesNotExposePulumiLocator(t *testing.T) {
	config := testConfig(t)
	provider, err := newProvisioner(config, &fakeFactory{})
	if err != nil {
		t.Fatal(err)
	}
	handle := provider.handle("operation-1", 1, "resource-1").String()
	stack := provider.stackName("noop", "resource-1")
	for _, value := range []string{config.BackendURL, config.WorkspaceRoot, "resource-1", stack} {
		if strings.Contains(handle, value) {
			t.Fatalf("opaque handle exposed %q", value)
		}
	}
	secondStack := provider.stackName("noop", "resource-1")
	if stack != secondStack {
		t.Fatal("stack identity is not deterministic")
	}
}

func testConfig(t *testing.T) Config {
	t.Helper()
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "noop"), []byte("test executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	project := "name: noop\nruntime:\n  name: go\n  options:\n    binary: ./noop\n"
	if err := os.WriteFile(filepath.Join(source, "Pulumi.yaml"), []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := SourceDigest(source)
	if err != nil {
		t.Fatal(err)
	}
	goExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Config{Identity: "test-v1", StackNamingVersion: StackNamingVersionV1, PulumiRoot: filepath.Join(t.TempDir(), "pulumi"), GoExecutable: goExecutable, BackendURL: "file://" + filepath.Join(t.TempDir(), "state"),
		StackNamespace: "test", WorkspaceRoot: t.TempDir(), HistoryPageSize: 10, HistoryMaximumPages: 3, StaleWorkspaceAge: time.Hour,
		Programs: []Program{{ResourceType: testResourceType, Capabilities: []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete},
			ProjectName: "noop", SourceDir: source, SourceDigest: digest, SecretInputsUnsupported: true,
			EncodeInput: func(Input) ([]byte, error) { return []byte(`{"nonSecret":true}`), nil }}},
	}
}

func executionRequest(t *testing.T, capability domain.Capability) provisioning.ExecutionRequest {
	t.Helper()
	spec, err := domain.NewResourceSpec(map[string]any{"nonSecret": true})
	if err != nil {
		t.Fatal(err)
	}
	return provisioning.ExecutionRequest{OperationID: "operation-1", AttemptNumber: 1, ResourceID: "resource-1", ResourceType: testResourceType,
		Spec: spec, Capability: capability, TargetGeneration: 1}
}

func observationRequest(t *testing.T) provisioning.ObservationRequest {
	t.Helper()
	request := executionRequest(t, domain.CapabilityCreate)
	return provisioning.ObservationRequest{OperationID: request.OperationID, AttemptNumber: request.AttemptNumber, ResourceID: request.ResourceID,
		ResourceType: request.ResourceType, Spec: request.Spec, Capability: request.Capability, TargetGeneration: request.TargetGeneration}
}

type fakeFactory struct {
	workspace       automationWorkspace
	openCalls       int
	workDir         string
	environment     map[string]string
	secureWorkspace bool
}

func (f *fakeFactory) Open(_ context.Context, workDir, _ string, environment map[string]string) (automationWorkspace, error) {
	f.openCalls++
	f.workDir = workDir
	f.environment = environment
	rootInfo, rootErr := os.Stat(filepath.Dir(workDir))
	inputInfo, inputErr := os.Stat(environment[inputEnvironment])
	f.secureWorkspace = rootErr == nil && inputErr == nil && rootInfo.Mode().Perm() == 0o700 && inputInfo.Mode().Perm() == 0o600
	if f.workspace == nil {
		return nil, errors.New("workspace unavailable")
	}
	return f.workspace, nil
}

type fakeWorkspace struct {
	stack       automationStack
	selectErr   error
	createCalls int
	stackNames  []string
}

func (w *fakeWorkspace) SelectStack(_ context.Context, name string) (automationStack, error) {
	w.stackNames = append(w.stackNames, name)
	return w.stack, w.selectErr
}

func (w *fakeWorkspace) CreateStack(_ context.Context, name string) (automationStack, error) {
	w.createCalls++
	w.stackNames = append(w.stackNames, name)
	return w.stack, nil
}

type fakeStack struct {
	summary        updateSummary
	pages          map[int][]updateSummary
	upCalls        int
	destroyCalls   int
	historyCalls   int
	runErr         error
	selectedOutput func(string) []byte
}

func (s *fakeStack) Up(_ context.Context, message string) (updateSummary, error) {
	s.upCalls++
	s.summary.message = message
	return s.summary, s.runErr
}

func (s *fakeStack) Destroy(_ context.Context, message string) (updateSummary, error) {
	s.destroyCalls++
	s.summary.message = message
	return s.summary, s.runErr
}

func (s *fakeStack) History(_ context.Context, _, page int) ([]updateSummary, error) {
	s.historyCalls++
	return s.pages[page], nil
}

func (*fakeStack) Info(context.Context) (stackInfo, error) { return stackInfo{}, nil }

// SelectedOutput returns the configured export payload; nil by default.
func (s *fakeStack) SelectedOutput(_ context.Context, name string) ([]byte, error) {
	if s.selectedOutput == nil {
		return nil, fmt.Errorf("selected output %q could not be read", name)
	}
	return s.selectedOutput(name), nil
}
