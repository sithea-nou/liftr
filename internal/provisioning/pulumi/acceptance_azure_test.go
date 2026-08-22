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
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/provisioning/bindings"
	pulumiprovisioner "github.com/sithea-nou/liftr/internal/provisioning/pulumi"
)

// acceptanceConfig reports the operator-supplied configuration for the
// opt-in Azure acceptance test. Every value is private platform or cloud
// configuration supplied through the environment; none of it belongs to a
// developer-facing contract.
type acceptanceConfig struct {
	pulumiRoot   string
	goExecutable string
	sourceDir    string
	sourceDigest string

	identity    string
	namespace   string
	location    string
	skuName     string
	skuTier     string
	haMode      string
	adminLogin  string
	credentials []string
}

func loadAcceptanceConfig(t *testing.T) acceptanceConfig {
	t.Helper()
	if os.Getenv("LIFTR_ACCEPTANCE_AZURE") != "1" {
		t.Skip("LIFTR_ACCEPTANCE_AZURE=1 is required; this test provisions real, cost-bearing Azure infrastructure")
	}
	required := []string{
		"LIFTR_ACCEPTANCE_LOCATION",
		"LIFTR_ACCEPTANCE_SKU_NAME",
		"LIFTR_ACCEPTANCE_SKU_TIER",
		"LIFTR_ACCEPTANCE_HA_MODE",
	}
	for _, name := range required {
		if os.Getenv(name) == "" {
			t.Fatalf("%s is required for the Azure acceptance test", name)
		}
	}
	if os.Getenv("LIFTR_TEST_PULUMI_ROOT") == "" {
		t.Fatal("LIFTR_TEST_PULUMI_ROOT is required for the Azure acceptance test")
	}
	credentials := []string{"ARM_SUBSCRIPTION_ID", "ARM_TENANT_ID", "ARM_CLIENT_ID", "ARM_CLIENT_SECRET"}
	for _, name := range credentials {
		if os.Getenv(name) == "" {
			t.Fatalf("%s must be exported for provider authentication", name)
		}
	}
	goExecutable, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	goExecutable, err = filepath.Abs(goExecutable)
	if err != nil {
		t.Fatal(err)
	}
	sourceDir := buildAzureProgram(t, goExecutable)
	digest, err := pulumiprovisioner.SourceDigest(sourceDir)
	if err != nil {
		t.Fatal(err)
	}
	return acceptanceConfig{
		pulumiRoot: os.Getenv("LIFTR_TEST_PULUMI_ROOT"), goExecutable: goExecutable,
		sourceDir: sourceDir, sourceDigest: digest,
		identity: fmt.Sprintf("liftr-acceptance-%d", time.Now().UnixNano()), namespace: "acceptance",
		location: os.Getenv("LIFTR_ACCEPTANCE_LOCATION"), skuName: os.Getenv("LIFTR_ACCEPTANCE_SKU_NAME"),
		skuTier: os.Getenv("LIFTR_ACCEPTANCE_SKU_TIER"), haMode: os.Getenv("LIFTR_ACCEPTANCE_HA_MODE"),
		adminLogin: "liftradmin", credentials: credentials,
	}
}

// buildAzureProgram compiles the nested-module reference program. The outer
// module never depends on the Azure SDK; this build happens only under the
// opt-in acceptance gate.
func buildAzureProgram(t *testing.T, goExecutable string) string {
	t.Helper()
	_, packageFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Dir(filepath.Dir(packageFile))
	for {
		if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(repoRoot)
		if parent == repoRoot {
			t.Fatal("cannot locate repository root")
		}
		repoRoot = parent
	}
	sourceDir := filepath.Join(repoRoot, "internal", "provisioning", "pulumi", "programs", "azureflexiblepostgresql")
	output := filepath.Join(t.TempDir(), "program")
	build := exec.Command(goExecutable, "build", "-o", output, ".")
	build.Dir = sourceDir
	build.Env = append(os.Environ(), "GOTOOLCHAIN=local")
	if outputBytes, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build azure reference program: %v: %s", err, outputBytes)
	}
	return sourceDir
}

// TestAzureFlexibleServerLifecycle is the opt-in reference implementation
// acceptance run: create, grow storage while enabling high availability,
// then delete a real Flexible Server. It is excluded from `go test ./...`
// and `make verify`, and it costs real money when enabled.
func TestAzureFlexibleServerLifecycle(t *testing.T) {
	config := loadAcceptanceConfig(t)

	backend := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(backend, 0o700); err != nil {
		t.Fatal(err)
	}
	platform := bindings.PostgresPlatform{
		Location: config.location, SkuName: config.skuName, SkuTier: config.skuTier,
		HighAvailabilityMode: config.haMode, AdministratorLogin: config.adminLogin,
	}
	provider, err := pulumiprovisioner.New(pulumiprovisioner.Config{
		Identity: config.identity, StackNamingVersion: pulumiprovisioner.StackNamingVersionV1,
		PulumiRoot: config.pulumiRoot, GoExecutable: config.goExecutable,
		BackendURL: (&url.URL{Scheme: "file", Path: backend}).String(), StackNamespace: config.namespace,
		WorkspaceRoot: t.TempDir(), HistoryPageSize: 50, HistoryMaximumPages: 20, StaleWorkspaceAge: time.Hour,
		Environment: func(_ context.Context) (map[string]string, error) {
			values := map[string]string{"PULUMI_CONFIG_PASSPHRASE": os.Getenv("PULUMI_CONFIG_PASSPHRASE")}
			for _, name := range config.credentials {
				values[name] = os.Getenv(name)
			}
			return values, nil
		},
		Programs: []pulumiprovisioner.Program{{
			ResourceType: domain.ResourceTypeRef{Name: "PostgreSQLDatabase", Version: "v1"},
			Capabilities: []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete},
			ProjectName:  "liftr-postgresqldatabase", SourceDir: config.sourceDir, SourceDigest: config.sourceDigest,
			RequiredEnvironment:     config.credentials,
			EncodeInput:             bindings.PostgresEncoder(config.identity, config.namespace, platform),
			SecretInputsUnsupported: true,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	resourceID := domain.ResourceID(fmt.Sprintf("acceptance-%d", time.Now().UnixNano()))
	resourceType := domain.ResourceTypeRef{Name: "PostgreSQLDatabase", Version: "v1"}
	newSpec := func(storage any, ha bool) domain.ResourceSpec {
		spec, specErr := domain.NewResourceSpec(map[string]any{"version": "16", "storageGB": storage, "highAvailability": ha})
		if specErr != nil {
			t.Fatal(specErr)
		}
		return spec
	}
	request := func(capability domain.Capability, generation uint64, spec domain.ResourceSpec) provisioning.ExecutionRequest {
		return provisioning.ExecutionRequest{
			OperationID:   domain.OperationID(fmt.Sprintf("%s-%s-%d", resourceID, capability, generation)),
			AttemptNumber: 1, ResourceID: resourceID, ResourceType: resourceType,
			Spec: spec, Capability: capability, TargetGeneration: generation,
		}
	}
	ctx := context.Background()

	createResult, createErr := provider.Submit(ctx, request(domain.CapabilityCreate, 1, newSpec(32, false)))
	assertAcceptanceSucceeded(t, "create", createResult.Observation, createErr)

	updateResult, updateErr := provider.Submit(ctx, request(domain.CapabilityUpdate, 2, newSpec(float64(64.0), true)))
	assertAcceptanceSucceeded(t, "update", updateResult.Observation, updateErr)

	deleteRequest := request(domain.CapabilityDelete, 3, newSpec(float64(64.0), true))
	deleteResult, deleteErr := provider.Submit(ctx, deleteRequest)
	assertAcceptanceSucceeded(t, "delete", deleteResult.Observation, deleteErr)

	observation, observeErr := provider.Observe(ctx, provisioning.ObservationRequest{
		OperationID: deleteRequest.OperationID, AttemptNumber: deleteRequest.AttemptNumber,
		ResourceID: deleteRequest.ResourceID, ResourceType: deleteRequest.ResourceType,
		Spec: deleteRequest.Spec, Capability: deleteRequest.Capability, TargetGeneration: deleteRequest.TargetGeneration,
	})
	if observeErr != nil {
		t.Fatal(observeErr)
	}
	if observation.Correlation != provisioning.RequestCorrelationFound ||
		observation.Resource.Presence != provisioning.ResourcePresenceNotFound {
		t.Fatalf("post-delete observation = %+v", observation)
	}
}

func assertAcceptanceSucceeded(t *testing.T, phase string, observation provisioning.ExecutionObservation, err error) {
	t.Helper()
	if err == nil && observation.Correlation == provisioning.RequestCorrelationFound &&
		observation.Execution != nil && observation.Execution.State == provisioning.ExecutionStateSucceeded {
		return
	}
	var failure error
	if observation.Execution != nil && observation.Execution.Failure != nil {
		failure = observation.Execution.Failure
	}
	t.Fatalf("%s did not succeed: correlation=%s state=%+v failure=%v err=%v", phase,
		observation.Correlation, observation.Execution, failure, err)
}
