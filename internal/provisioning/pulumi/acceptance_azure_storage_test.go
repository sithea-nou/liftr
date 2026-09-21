// SPDX-License-Identifier: Apache-2.0

package pulumi_test

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/provisioning/bindings"
	pulumiprovisioner "github.com/sithea-nou/liftr/internal/provisioning/pulumi"
	"github.com/sithea-nou/liftr/internal/resourcetypes/objectstorage"
)

const azureStorageOutputMapping = "liftr-azure-object-storage-outputs-v1"

// TestAzureStorageLifecyclePulumi creates a real Azure Storage account,
// changes its access frequency, verifies the provider-neutral endpoint output,
// and destroys it. It is intentionally excluded from normal verification.
func TestAzureStorageLifecyclePulumi(t *testing.T) {
	if os.Getenv("LIFTR_ACCEPTANCE_AZURE_STORAGE_PULUMI") != "1" {
		t.Skip("LIFTR_ACCEPTANCE_AZURE_STORAGE_PULUMI=1 is required; this test creates cost-bearing Azure infrastructure")
	}
	requireEnvironment(t, "LIFTR_TEST_PULUMI_ROOT", "LIFTR_ACCEPTANCE_STORAGE_LOCATION",
		"ARM_SUBSCRIPTION_ID", "ARM_TENANT_ID", "ARM_CLIENT_ID", "ARM_CLIENT_SECRET")

	goExecutable, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	goExecutable, err = filepath.Abs(goExecutable)
	if err != nil {
		t.Fatal(err)
	}
	sourceDir := azureStoragePulumiSource(t)
	compilePulumiAcceptanceProgram(t, goExecutable, sourceDir)
	programBinary := filepath.Join(sourceDir, "program")
	if info, statErr := os.Stat(programBinary); statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		t.Fatal("prebuilt Azure Storage program is required; run make build-acceptance-azure-storage-program")
	}
	digest, err := pulumiprovisioner.SourceDigest(sourceDir)
	if err != nil {
		t.Fatal(err)
	}

	backend := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(backend, 0o700); err != nil {
		t.Fatal(err)
	}
	credentials := []string{"ARM_SUBSCRIPTION_ID", "ARM_TENANT_ID", "ARM_CLIENT_ID", "ARM_CLIENT_SECRET"}
	identity := fmt.Sprintf("liftr-storage-acceptance-%d", time.Now().UnixNano())
	platform := bindings.ObjectStoragePlatform{
		Location: os.Getenv("LIFTR_ACCEPTANCE_STORAGE_LOCATION"),
		SkuName:  environmentDefault("LIFTR_ACCEPTANCE_STORAGE_SKU_NAME", "Standard_LRS"),
	}
	provider, err := pulumiprovisioner.New(pulumiprovisioner.Config{
		Identity: identity, StackNamingVersion: pulumiprovisioner.StackNamingVersionV1,
		PulumiRoot: os.Getenv("LIFTR_TEST_PULUMI_ROOT"), GoExecutable: goExecutable,
		BackendURL: (&url.URL{Scheme: "file", Path: backend}).String(), StackNamespace: "azure-storage-acceptance",
		WorkspaceRoot: t.TempDir(), HistoryPageSize: 50, HistoryMaximumPages: 20, StaleWorkspaceAge: time.Hour,
		Environment: func(_ context.Context) (map[string]string, error) {
			values := map[string]string{"PULUMI_CONFIG_PASSPHRASE": os.Getenv("PULUMI_CONFIG_PASSPHRASE")}
			for _, name := range credentials {
				values[name] = os.Getenv(name)
			}
			return values, nil
		},
		Programs: []pulumiprovisioner.Program{{
			ResourceType: objectstorage.TypeRef(),
			Capabilities: []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete},
			ProjectName:  "liftr-objectstorage", SourceDir: sourceDir, SourceDigest: digest,
			RequiredEnvironment: credentials, EncodeInput: bindings.ObjectStorageEncoder(identity, "azure-storage-acceptance", platform),
			SecretInputsUnsupported: true,
			OutputMappings:          []pulumiprovisioner.OutputMapping{{Ref: azureStorageOutputMapping, ExportName: "liftrOutputs"}},
			CurrentOutputMappingRef: azureStorageOutputMapping,
		}},
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
			result.OutputMappingRef = azureStorageOutputMapping
		}
		return result
	}

	cleanupRequired := true
	t.Cleanup(func() {
		if !cleanupRequired {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
		defer cancel()
		_, cleanupErr := provider.Submit(ctx, request(domain.CapabilityDelete, 99, "infrequent"))
		if cleanupErr != nil {
			t.Errorf("emergency Azure cleanup failed: %v", cleanupErr)
		}
	})

	createResult, createErr := provider.Submit(context.Background(), request(domain.CapabilityCreate, 1, "frequent"))
	assertAcceptanceSucceeded(t, "create storage", createResult.Observation, createErr)
	requireStorageEndpoint(t, createResult.Observation)

	updateResult, updateErr := provider.Submit(context.Background(), request(domain.CapabilityUpdate, 2, "infrequent"))
	assertAcceptanceSucceeded(t, "update storage", updateResult.Observation, updateErr)
	requireStorageEndpoint(t, updateResult.Observation)

	deleteRequest := request(domain.CapabilityDelete, 3, "infrequent")
	deleteResult, deleteErr := provider.Submit(context.Background(), deleteRequest)
	assertAcceptanceSucceeded(t, "delete storage", deleteResult.Observation, deleteErr)
	cleanupRequired = false

	observation, observeErr := provider.Observe(context.Background(), provisioning.ObservationRequest{
		OperationID: deleteRequest.OperationID, AttemptNumber: deleteRequest.AttemptNumber,
		ResourceID: deleteRequest.ResourceID, ResourceType: deleteRequest.ResourceType,
		Spec: deleteRequest.Spec, Capability: deleteRequest.Capability, TargetGeneration: deleteRequest.TargetGeneration,
	})
	if observeErr != nil {
		t.Fatal(observeErr)
	}
	if observation.Correlation != provisioning.RequestCorrelationFound || observation.Resource.Presence != provisioning.ResourcePresenceNotFound {
		t.Fatalf("post-delete observation = %+v", observation)
	}
}

func azureStoragePulumiSource(t *testing.T) string {
	t.Helper()
	_, currentFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(currentFile), "programs", "azureobjectstorage")
}

func compilePulumiAcceptanceProgram(t *testing.T, goExecutable, sourceDir string) {
	t.Helper()
	build := exec.Command(goExecutable, "build", "-o", filepath.Join(t.TempDir(), "program"), ".")
	build.Dir = sourceDir
	build.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Azure Storage Pulumi program: %v: %s", err, output)
	}
}

func requireStorageEndpoint(t *testing.T, observation provisioning.ExecutionObservation) {
	t.Helper()
	if observation.Outputs == nil || observation.Outputs.State != provisioning.OutputsAvailable {
		t.Fatalf("object-storage outputs unavailable: %+v", observation.Outputs)
	}
	endpoint, ok := observation.Outputs.Values["endpoint"].(string)
	parsed, err := url.Parse(endpoint)
	if !ok || err != nil || parsed.Scheme != "https" || parsed.Host == "" || !strings.HasSuffix(parsed.Path, "/") {
		t.Fatalf("unexpected object-storage endpoint %v", observation.Outputs.Values["endpoint"])
	}
}

func requireEnvironment(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if os.Getenv(name) == "" {
			t.Fatalf("%s is required for the Azure Storage acceptance test", name)
		}
	}
}

func environmentDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
