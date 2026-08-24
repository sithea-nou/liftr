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

// TestRecordingProgramOutputsThroughRealCLI drives selected-output extraction
// end to end against the deterministic CI program: a real Pulumi invocation on
// the file backend publishes the allowlisted envelope, the strict decoder
// validates provenance, and delete never extracts.
func TestRecordingProgramOutputsThroughRealCLI(t *testing.T) {
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
	resourceType := domain.ResourceTypeRef{Name: "PostgreSQLDatabase", Version: "v2"}
	config := Config{
		Identity: "m10-integration", StackNamingVersion: StackNamingVersionV1,
		PulumiRoot: pulumiRoot, GoExecutable: goExecutable,
		BackendURL:     (&url.URL{Scheme: "file", Path: backend}).String(),
		StackNamespace: "m10",
		WorkspaceRoot:  t.TempDir(), HistoryPageSize: 10, HistoryMaximumPages: 10, StaleWorkspaceAge: time.Hour,
		Environment: func(context.Context) (map[string]string, error) {
			return map[string]string{"PULUMI_CONFIG_PASSPHRASE": "liftr-non-secret-test-passphrase"}, nil
		},
		Programs: []Program{{ResourceType: resourceType, Capabilities: []domain.Capability{
			domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete},
			ProjectName: "liftr-recording", SourceDir: source, SourceDigest: digest, SecretInputsUnsupported: true,
			EncodeInput: func(input Input) ([]byte, error) {
				envelope := map[string]any{
					"inputVersion": 1, "capability": string(input.Capability), "resourceId": string(input.ResourceID),
					"resourceTypeName": input.ResourceType.Name, "resourceTypeVersion": input.ResourceType.Version,
					"targetGeneration": input.TargetGeneration,
					"infraName":        InfraName("m10-integration", "m10", input.ResourceType, input.ResourceID),
					"spec": map[string]any{"version": "16", "storageGB": int64(20),
						"highAvailability": false},
				}
				return json.Marshal(envelope)
			}},
		},
	}
	config.Programs[0].OutputMappings = []OutputMapping{{Ref: "liftr-recording-outputs-v1", ExportName: "liftrOutputs"}}
	config.Programs[0].CurrentOutputMappingRef = "liftr-recording-outputs-v1"

	provider, err := New(config)
	if err != nil {
		t.Fatal(err)
	}

	create := provisioning.ExecutionRequest{OperationID: "m10-create", AttemptNumber: 1, ResourceID: "m10-resource",
		ResourceType: resourceType, Spec: mustIntegrationSpec(t),
		Capability: domain.CapabilityCreate, TargetGeneration: 4, OutputMappingRef: "liftr-recording-outputs-v1"}
	result, err := provider.Submit(context.Background(), create)
	assertTerminalSuccess(t, result, err)
	if result.Observation.Outputs == nil || result.Observation.Outputs.State != provisioning.OutputsAvailable {
		t.Fatalf("create outputs = %+v", result.Observation.Outputs)
	}
	values := result.Observation.Outputs.Values
	hostname, _ := values["hostname"].(string)
	port, _ := values["port"].(int64)
	if hostname == "" || port != 5432 {
		t.Fatalf("values = %v", values)
	}
	for banned := range map[string]struct{}{"password": {}, "connectionString": {}} {
		if _, present := values[banned]; present {
			t.Fatalf("forbidden value %q crossed the boundary", banned)
		}
	}

	deleteRequest := create
	deleteRequest.OperationID = "m10-delete"
	deleteRequest.Capability = domain.CapabilityDelete
	deleteRequest.TargetGeneration = 5
	deleteResult, err := provider.Submit(context.Background(), deleteRequest)
	assertTerminalSuccess(t, deleteResult, err)
	if deleteResult.Observation.Outputs != nil {
		t.Fatalf("delete carried an output claim: %+v", deleteResult.Observation.Outputs)
	}

	// A registration whose declared mapping ref contradicts the program's
	// persisted envelope identity fails loudly instead of falling back.
	mismatched := provisioning.ExecutionRequest{OperationID: "m10-mismatch", AttemptNumber: 1, ResourceID: "m10-resource-b",
		ResourceType: resourceType, Spec: mustIntegrationSpec(t),
		Capability: domain.CapabilityCreate, TargetGeneration: 1, OutputMappingRef: "liftr-wrong-registration-ref"}
	mismatchProvider := *provider
	badProgram := config.Programs[0]
	badProgram.OutputMappings = []OutputMapping{{Ref: "liftr-wrong-registration-ref", ExportName: "liftrOutputs"}}
	badProgram.CurrentOutputMappingRef = "liftr-wrong-registration-ref"
	mismatchProvider.programs = map[domain.ResourceTypeRef]Program{resourceType: badProgram}
	submission, err := mismatchProvider.Submit(context.Background(), mismatched)
	if err != nil && submission.Observation.Execution == nil {
		// Loud pre-extraction failure paths are also acceptable.
	} else if submission.Observation.Outputs != nil && submission.Observation.Outputs.State == provisioning.OutputsAvailable {
		t.Fatal("contradicting registration produced available evidence")
	}
}

func mustIntegrationSpec(t *testing.T) domain.ResourceSpec {
	t.Helper()
	spec, err := domain.NewResourceSpec(map[string]any{"version": "16", "storageGB": int64(20), "highAvailability": false})
	if err != nil {
		t.Fatal(err)
	}
	return spec
}
