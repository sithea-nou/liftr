// SPDX-License-Identifier: Apache-2.0

package opentofu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/resourcetypes/objectstorage"
)

const (
	azureRMProviderAddress = "registry.opentofu.org/hashicorp/azurerm"
	azureRMProviderVersion = "4.46.0"
	azureStorageMapping    = "liftr-azure-object-storage-outputs-v1"
)

// TestAzureStorageLifecycleOpenTofu qualifies create, update, output mapping,
// and delete against real Azure. It uses the adapter's test-only local backend
// profile; production OpenTofu registrations must still use a conformant HTTPS
// HTTP backend.
func TestAzureStorageLifecycleOpenTofu(t *testing.T) {
	if os.Getenv("LIFTR_ACCEPTANCE_AZURE_STORAGE_OPENTOFU") != "1" {
		t.Skip("LIFTR_ACCEPTANCE_AZURE_STORAGE_OPENTOFU=1 is required; this test creates cost-bearing Azure infrastructure")
	}
	requireAzureStorageEnvironment(t, "LIFTR_TEST_OPENTOFU_BIN", "LIFTR_ACCEPTANCE_OPENTOFU_PROVIDER_MIRROR",
		"LIFTR_ACCEPTANCE_STORAGE_LOCATION", "ARM_SUBSCRIPTION_ID", "ARM_TENANT_ID", "ARM_CLIENT_ID", "ARM_CLIENT_SECRET")

	executable, err := filepath.Abs(os.Getenv("LIFTR_TEST_OPENTOFU_BIN"))
	if err != nil {
		t.Fatal(err)
	}
	executableDigest, err := digestFile(executable, maxExecutableBytes)
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := filepath.Abs(os.Getenv("LIFTR_ACCEPTANCE_OPENTOFU_PROVIDER_MIRROR"))
	if err != nil {
		t.Fatal(err)
	}
	packagePath := filepath.Join(mirror, "registry.opentofu.org", "hashicorp", "azurerm",
		"terraform-provider-azurerm_"+azureRMProviderVersion+"_"+runtime.GOOS+"_"+runtime.GOARCH+".zip")
	packageDigest, err := digestFile(packagePath, maxProviderPackageBytes)
	if err != nil {
		t.Fatalf("AzureRM provider mirror package is unavailable; run make prepare-acceptance-azure-storage-opentofu: %v", err)
	}

	root := t.TempDir()
	for _, name := range []string{"work", "quarantine", "state"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	source := copyAzureStorageOpenTofuSource(t, root)
	program := Program{
		Ref:          "azure-object-storage-opentofu-v1",
		ResourceType: objectstorage.TypeRef(),
		Capabilities: []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete},
		SourceDir:    source,
		ProviderConstraints: map[string]string{
			azureRMProviderAddress: azureRMProviderVersion,
		},
		ProviderPackages: []ProviderPackage{{
			Address: azureRMProviderAddress, Version: azureRMProviderVersion, Path: packagePath, SHA256: packageDigest,
		}},
		ProviderMirror: mirror,
		EncodeInput: func(input Input) (map[string]any, error) {
			return map[string]any{
				"platform": map[string]any{
					"identity":        "liftr-azure-storage-acceptance",
					"location":        os.Getenv("LIFTR_ACCEPTANCE_STORAGE_LOCATION"),
					"replicationType": azureStorageEnvironmentDefault("LIFTR_ACCEPTANCE_STORAGE_REPLICATION_TYPE", "LRS"),
				},
				"spec": input.Spec.Values(),
			}, nil
		},
		RequiredEnvironment:      []string{"ARM_SUBSCRIPTION_ID", "ARM_TENANT_ID", "ARM_CLIENT_ID", "ARM_CLIENT_SECRET"},
		ControlMarkerAddress:     "terraform_data.liftr_control",
		ManagedWorkloadAddresses: []string{"azurerm_resource_group.storage[0]", "azurerm_storage_account.storage[0]"},
		OutputMappings: []OutputMapping{{
			Ref: azureStorageMapping, EnvelopeName: "liftr_envelope", Fields: map[string]string{"endpoint": "endpoint"},
		}},
		CurrentOutputMappingRef: azureStorageMapping,
	}
	applySourceDefaults(&program)
	program.SourceDigest, err = SourceDigest(program.SourceDir, sourceLimits(program))
	if err != nil {
		t.Fatal(err)
	}

	evidence := newL2EvidenceStore()
	provider, err := New(Config{
		Executable: executable, ExecutableSHA256: executableDigest,
		WorkRoot: filepath.Join(root, "work"), QuarantineRoot: filepath.Join(root, "quarantine"),
		Evidence: evidence, LockTimeout: 2 * time.Minute,
		Registration: Registration{
			ProvisionerRef: "azure-object-storage-opentofu-v1", Identity: "liftr-azure-storage-acceptance",
			StateKeyVersion: StateKeyVersionV1, Program: program,
			Backend: BackendProfile{Ref: "azure-storage-acceptance-local-v1", DevelopmentLocal: true, LocalStateRoot: filepath.Join(root, "state")},
			Environment: func(_ context.Context) (map[string]string, error) {
				return map[string]string{
					"ARM_SUBSCRIPTION_ID": os.Getenv("ARM_SUBSCRIPTION_ID"),
					"ARM_TENANT_ID":       os.Getenv("ARM_TENANT_ID"),
					"ARM_CLIENT_ID":       os.Getenv("ARM_CLIENT_ID"),
					"ARM_CLIENT_SECRET":   os.Getenv("ARM_CLIENT_SECRET"),
				}, nil
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	resourceID := domain.ResourceID(fmt.Sprintf("storage-acceptance-%d", time.Now().UnixNano()))
	request := func(capability domain.Capability, generation uint64, frequency string) provisioning.ExecutionRequest {
		spec, specErr := objectstorage.NewSpec(frequency, false)
		if specErr != nil {
			t.Fatal(specErr)
		}
		result := provisioning.ExecutionRequest{
			OperationID:   domain.OperationID(fmt.Sprintf("%s-%s-%d", resourceID, capability, generation)),
			AttemptNumber: 1, ResourceID: resourceID, ResourceType: objectstorage.TypeRef(),
			Spec: spec, Capability: capability, TargetGeneration: generation,
		}
		if capability != domain.CapabilityDelete {
			result.OutputMappingRef = azureStorageMapping
		}
		return result
	}
	submit := func(ctx context.Context, req provisioning.ExecutionRequest, label string) (provisioning.Submission, error) {
		fence := provisioning.ExecutionFence{MessageID: "azure-storage-message-" + label, LeaseToken: "azure-storage-token-" + label}
		evidence.allow(provider.attemptKey(req.ResourceID, req.OperationID, req.AttemptNumber), fence)
		return provider.SubmitFenced(ctx, req, fence)
	}

	cleanupRequired := true
	t.Cleanup(func() {
		if !cleanupRequired {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
		defer cancel()
		_, cleanupErr := submit(ctx, request(domain.CapabilityDelete, 99, "infrequent"), "cleanup")
		if cleanupErr != nil {
			t.Errorf("emergency Azure cleanup failed: %v", cleanupErr)
		}
	})

	createResult, createErr := submit(context.Background(), request(domain.CapabilityCreate, 1, "frequent"), "create")
	requireAzureStorageSubmission(t, "create", createResult, createErr)
	requireAzureStorageOutput(t, createResult.Observation)

	updateResult, updateErr := submit(context.Background(), request(domain.CapabilityUpdate, 2, "infrequent"), "update")
	requireAzureStorageSubmission(t, "update", updateResult, updateErr)
	requireAzureStorageOutput(t, updateResult.Observation)

	deleteResult, deleteErr := submit(context.Background(), request(domain.CapabilityDelete, 3, "infrequent"), "delete")
	requireAzureStorageSubmission(t, "delete", deleteResult, deleteErr)
	if deleteResult.Observation.Resource.Presence != provisioning.ResourcePresenceNotFound {
		t.Fatalf("delete did not converge to absence: %+v", deleteResult.Observation.Resource)
	}
	cleanupRequired = false
}

func copyAzureStorageOpenTofuSource(t *testing.T, root string) string {
	t.Helper()
	_, currentFile, _, _ := runtime.Caller(0)
	origin := filepath.Join(filepath.Dir(currentFile), "testdata", "azureobjectstorage")
	destination := filepath.Join(root, "source")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(origin)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("unexpected directory in trusted OpenTofu source: %s", entry.Name())
		}
		raw, readErr := os.ReadFile(filepath.Join(origin, entry.Name()))
		if readErr != nil {
			t.Fatal(readErr)
		}
		mode := os.FileMode(0o600)
		if entry.Name() == ".terraform.lock.hcl" {
			mode = 0o400
		}
		if writeErr := os.WriteFile(filepath.Join(destination, entry.Name()), raw, mode); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	return destination
}

func requireAzureStorageSubmission(t *testing.T, phase string, result provisioning.Submission, err error) {
	t.Helper()
	observation := result.Observation
	if err == nil && observation.Correlation == provisioning.RequestCorrelationFound &&
		observation.Execution != nil && observation.Execution.State == provisioning.ExecutionStateSucceeded {
		return
	}
	t.Fatalf("%s did not succeed: observation=%+v err=%v", phase, observation, err)
}

func requireAzureStorageOutput(t *testing.T, observation provisioning.ExecutionObservation) {
	t.Helper()
	if observation.Outputs == nil || observation.Outputs.State != provisioning.OutputsAvailable {
		t.Fatalf("object-storage outputs unavailable: %+v", observation.Outputs)
	}
	endpoint, ok := observation.Outputs.Values["endpoint"].(string)
	if !ok || !strings.HasPrefix(endpoint, "https://") {
		t.Fatalf("unexpected object-storage endpoint %v", observation.Outputs.Values["endpoint"])
	}
}

func requireAzureStorageEnvironment(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if os.Getenv(name) == "" {
			t.Fatalf("%s is required for the Azure Storage acceptance test", name)
		}
	}
}

func azureStorageEnvironmentDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
