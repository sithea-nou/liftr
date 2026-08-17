// SPDX-License-Identifier: Apache-2.0

package pulumi

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

func TestLocalWorkspaceFilesystemBackendLifecycle(t *testing.T) {
	pulumiRoot := os.Getenv("LIFTR_TEST_PULUMI_ROOT")
	if pulumiRoot == "" {
		t.Skip("LIFTR_TEST_PULUMI_ROOT is required for Pulumi integration tests")
	}
	t.Setenv("LIFTR_FORBIDDEN_AMBIENT_VALUE", "must-not-reach-program")
	_, packageFile, _, _ := runtime.Caller(0)
	packageDir := filepath.Dir(packageFile)
	source := t.TempDir()
	programBinary := filepath.Join(source, "noop")
	goExecutable, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	goExecutable, err = filepath.Abs(goExecutable)
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command(goExecutable, "build", "-o", programBinary, "./testdata/noop")
	build.Dir = packageDir
	build.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build harmless Pulumi program: %v: %s", err, output)
	}
	project := []byte("name: liftr-noop\nruntime:\n  name: go\n  options:\n    binary: ./noop\n")
	if err := os.WriteFile(filepath.Join(source, "Pulumi.yaml"), project, 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := SourceDigest(source)
	if err != nil {
		t.Fatal(err)
	}
	backend := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(backend, 0o700); err != nil {
		t.Fatal(err)
	}
	workspaceRoot := t.TempDir()
	backendURL := (&url.URL{Scheme: "file", Path: backend}).String()
	config := Config{Identity: "integration-v1", PulumiRoot: pulumiRoot, GoExecutable: goExecutable, BackendURL: backendURL, StackNamespace: "integration",
		WorkspaceRoot: workspaceRoot, HistoryPageSize: 10, HistoryMaximumPages: 10, StaleWorkspaceAge: time.Hour,
		Environment: func(context.Context) (map[string]string, error) {
			return map[string]string{"PULUMI_CONFIG_PASSPHRASE": "liftr-non-secret-test-passphrase"}, nil
		},
		Programs: []Program{{ResourceType: testResourceType, Capabilities: []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete},
			ProjectName: "liftr-noop", SourceDir: source, SourceDigest: digest, SecretInputsUnsupported: true,
			EncodeInput: func(input Input) ([]byte, error) {
				return json.Marshal(map[string]any{"nonSecret": input.Spec.Values()["nonSecret"]})
			}}},
	}
	provider, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := domain.NewResourceSpec(map[string]any{"nonSecret": true})
	create := provisioning.ExecutionRequest{OperationID: "pulumi-create", AttemptNumber: 1, ResourceID: "pulumi-resource", ResourceType: testResourceType,
		Spec: spec, Capability: domain.CapabilityCreate, TargetGeneration: 1}
	createResult, err := provider.Submit(context.Background(), create)
	assertTerminalSuccess(t, createResult, err)
	update := create
	update.OperationID = "pulumi-update"
	update.Capability = domain.CapabilityUpdate
	update.TargetGeneration = 2
	updateResult, err := provider.Submit(context.Background(), update)
	assertTerminalSuccess(t, updateResult, err)
	deleteRequest := update
	deleteRequest.OperationID = "pulumi-delete"
	deleteRequest.Capability = domain.CapabilityDelete
	deleteResult, err := provider.Submit(context.Background(), deleteRequest)
	assertTerminalSuccess(t, deleteResult, err)

	observation, err := provider.Observe(context.Background(), provisioning.ObservationRequest{OperationID: deleteRequest.OperationID,
		AttemptNumber: 1, ResourceID: deleteRequest.ResourceID, ResourceType: deleteRequest.ResourceType, Spec: deleteRequest.Spec,
		Capability: deleteRequest.Capability, TargetGeneration: deleteRequest.TargetGeneration})
	if err != nil || observation.Correlation != provisioning.RequestCorrelationFound || observation.Execution == nil || observation.Execution.State != provisioning.ExecutionStateSucceeded {
		t.Fatalf("destroy observation=%+v error=%v", observation, err)
	}
	inspection, err := createWorkspace(workspaceRoot, config.Programs[0], []byte(`{"nonSecret":true}`))
	if err != nil {
		t.Fatal(err)
	}
	environment, err := provider.environment(context.Background(), inspection)
	if err != nil {
		inspection.cleanup()
		t.Fatal(err)
	}
	automationWorkspace, err := provider.factory.Open(context.Background(), inspection.workDir, inspection.homeDir, environment)
	if err != nil {
		inspection.cleanup()
		t.Fatal(err)
	}
	stack, err := automationWorkspace.SelectStack(context.Background(), provider.stackName("liftr-noop", create.ResourceID))
	if err != nil {
		inspection.cleanup()
		t.Fatalf("destroy removed the retained stack: %v", err)
	}
	history, err := stack.History(context.Background(), 10, 1)
	inspection.cleanup()
	if err != nil {
		t.Fatal(err)
	}
	for _, summary := range history {
		if summary.kind == "refresh" {
			t.Fatal("adapter ran Refresh automatically")
		}
	}
	entries, err := os.ReadDir(workspaceRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("isolated workspaces remain: %v error=%v", entries, err)
	}
	stackName := provider.stackName("liftr-noop", create.ResourceID)
	if stackName == "" {
		t.Fatal("deterministic retained stack identity is empty")
	}
}

func assertTerminalSuccess(t *testing.T, submission provisioning.Submission, err error) {
	t.Helper()
	if err != nil || submission.Observation.Correlation != provisioning.RequestCorrelationFound || submission.Observation.Execution == nil || submission.Observation.Execution.State != provisioning.ExecutionStateSucceeded {
		var failure *provisioning.ExecutionFailure
		if submission.Observation.Execution != nil {
			failure = submission.Observation.Execution.Failure
		}
		t.Fatalf("correlation=%s state=%v failure=%v error=%v", submission.Observation.Correlation, submission.Observation.Execution, failure, err)
	}
}
