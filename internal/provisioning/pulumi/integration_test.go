// SPDX-License-Identifier: Apache-2.0

package pulumi

import (
	"context"
	"encoding/json"
	"errors"
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
	config := Config{Identity: "integration-v1", StackNamingVersion: StackNamingVersionV1, PulumiRoot: pulumiRoot, GoExecutable: goExecutable, BackendURL: backendURL, StackNamespace: "integration",
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
	environment, err := provider.environment(context.Background(), inspection, nil)
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

// TestPostgreSQLBindingLifecycleThroughRecordingProgram drives the M9
// reference binding end-to-end against the deterministic CI program: real
// Pulumi invocations on the file backend, the shared envelope encoder,
// int64/float64 storage equivalence, history correlation, honest delete
// absence facts, and retained stack identity.
func TestPostgreSQLBindingLifecycleThroughRecordingProgram(t *testing.T) {
	pulumiRoot := os.Getenv("LIFTR_TEST_PULUMI_ROOT")
	if pulumiRoot == "" {
		t.Skip("LIFTR_TEST_PULUMI_ROOT is required for Pulumi integration tests")
	}
	_, packageFile, _, _ := runtime.Caller(0)
	packageDir := filepath.Dir(packageFile)
	source := t.TempDir()
	programBinary := filepath.Join(source, "recording")
	goExecutable, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	goExecutable, err = filepath.Abs(goExecutable)
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command(goExecutable, "build", "-o", programBinary, "./testdata/recording")
	build.Dir = packageDir
	build.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build recording Pulumi program: %v: %s", err, output)
	}
	project := []byte("name: liftr-recording\nruntime:\n  name: go\n  options:\n    binary: ./recording\n")
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
	resourceType := domain.ResourceTypeRef{Name: "PostgreSQLDatabase", Version: "v1"}
	config := Config{
		Identity: "m9-integration", StackNamingVersion: StackNamingVersionV1, PulumiRoot: pulumiRoot, GoExecutable: goExecutable,
		BackendURL: (&url.URL{Scheme: "file", Path: backend}).String(), StackNamespace: "m9",
		WorkspaceRoot: t.TempDir(), HistoryPageSize: 10, HistoryMaximumPages: 10, StaleWorkspaceAge: time.Hour,
		Environment: func(context.Context) (map[string]string, error) {
			return map[string]string{"PULUMI_CONFIG_PASSPHRASE": "liftr-non-secret-test-passphrase"}, nil
		},
		Programs: []Program{{ResourceType: resourceType, Capabilities: []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete},
			ProjectName: "liftr-recording", SourceDir: source, SourceDigest: digest, SecretInputsUnsupported: true,
			EncodeInput: func(input Input) ([]byte, error) {
				// Mirrors the production binding encoder shape (see
				// internal/provisioning/bindings); inlined here because this
				// package cannot import its own consumer without a cycle.
				version, _ := input.Spec.Values()["version"].(string)
				storageRaw := input.Spec.Values()["storageGB"]
				highAvailability, _ := input.Spec.Values()["highAvailability"].(bool)
				storageGB, ok := domain.IntegralValue(storageRaw)
				if !ok || version == "" {
					return nil, errors.New("program input mapping rejected the submitted spec")
				}
				envelope := map[string]any{
					"inputVersion": 1, "capability": string(input.Capability), "resourceId": string(input.ResourceID),
					"resourceTypeName": input.ResourceType.Name, "resourceTypeVersion": input.ResourceType.Version,
					"targetGeneration": input.TargetGeneration,
					"infraName":        InfraName("m9-integration", "m9", input.ResourceType, input.ResourceID),
					"spec":             map[string]any{"version": version, "storageGB": storageGB, "highAvailability": highAvailability},
				}
				return json.Marshal(envelope)
			}}},
	}
	provider, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	newSpec := func(storageGB any) domain.ResourceSpec {
		spec, specErr := domain.NewResourceSpec(map[string]any{"version": "16", "storageGB": storageGB, "highAvailability": false})
		if specErr != nil {
			t.Fatal(specErr)
		}
		return spec
	}
	create := provisioning.ExecutionRequest{OperationID: "m9-create", AttemptNumber: 1, ResourceID: "m9-resource",
		ResourceType: resourceType, Spec: newSpec(int64(20)), Capability: domain.CapabilityCreate, TargetGeneration: 1}
	createResult, err := provider.Submit(context.Background(), create)
	assertTerminalSuccess(t, createResult, err)

	update := create
	update.OperationID = "m9-update"
	update.Capability = domain.CapabilityUpdate
	update.TargetGeneration = 2
	update.Spec = newSpec(float64(30.0)) // decimal representation of an integral value must map identically
	updateResult, err := provider.Submit(context.Background(), update)
	assertTerminalSuccess(t, updateResult, err)

	deleteRequest := update
	deleteRequest.OperationID = "m9-delete"
	deleteRequest.Capability = domain.CapabilityDelete
	deleteRequest.Spec = newSpec(float64(30.0))
	deleteResult, err := provider.Submit(context.Background(), deleteRequest)
	assertTerminalSuccess(t, deleteResult, err)

	observation, err := provider.Observe(context.Background(), provisioning.ObservationRequest{OperationID: deleteRequest.OperationID,
		AttemptNumber: 1, ResourceID: deleteRequest.ResourceID, ResourceType: deleteRequest.ResourceType, Spec: deleteRequest.Spec,
		Capability: deleteRequest.Capability, TargetGeneration: deleteRequest.TargetGeneration})
	if err != nil {
		t.Fatal(err)
	}
	if observation.Correlation != provisioning.RequestCorrelationFound || observation.Execution == nil ||
		observation.Execution.State != provisioning.ExecutionStateSucceeded {
		t.Fatalf("delete observation = %+v error=%v", observation, err)
	}
	if observation.Resource.Presence != provisioning.ResourcePresenceNotFound ||
		observation.Resource.Readiness != provisioning.ResourceReadinessUnknown ||
		observation.Resource.Drift != provisioning.ResourceDriftUnknown {
		t.Fatalf("destroy success facts = %+v, want managed absence only", observation.Resource)
	}
}
