// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	appfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/provisioning/pulumi"
	"github.com/sithea-nou/liftr/internal/resourcetypes"
	"github.com/sithea-nou/liftr/internal/resourcetypes/postgresqldatabase"
	"github.com/sithea-nou/liftr/internal/server"
)

// TestEndToEndPostgreSQLDatabaseLifecycle proves the M9 goal flow against the
// real HTTP surface, application orchestration, durable outbox worker, and
// Pulumi adapter using the deterministic recording program on the file
// backend. It requires no cloud credentials and is skipped unless the pinned
// Pulumi layout is available.
func TestEndToEndPostgreSQLDatabaseLifecycle(t *testing.T) {
	pulumiRoot := os.Getenv("LIFTR_TEST_PULUMI_ROOT")
	if pulumiRoot == "" {
		t.Skip("LIFTR_TEST_PULUMI_ROOT is required for the Pulumi end-to-end test")
	}

	handler, pump := newE2ERuntime(t, pulumiRoot)

	// 1. Developer admits a PostgreSQLDatabase create.
	createBody := map[string]any{
		"id":    "e2e-db",
		"type":  map[string]string{"name": "PostgreSQLDatabase", "version": "v1"},
		"owner": map[string]string{"kind": "team", "id": "platform"},
		"spec":  map[string]any{"version": "16", "storageGB": int64(20), "highAvailability": false},
	}
	response := doJSON(t, handler, http.MethodPost, "/v1/resources", createBody, map[string]string{"Idempotency-Key": "e2e-create"})
	if response.statusCode != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", response.statusCode, string(response.body))
	}
	if location := response.header.Get("Location"); location != "/v1/resources/e2e-db" {
		t.Fatalf("create Location = %q", location)
	}

	// 2. The worker drives admission through dispatch and observation.
	pump()

	resource := getResource(t, handler, "e2e-db")
	if resource.Generation != 1 {
		t.Fatalf("generation = %d, want 1", resource.Generation)
	}
	if resource.Status.State != "Ready" {
		t.Fatalf("state = %s after create, want Ready (conditions=%+v)", resource.Status.State, resource.Status.Conditions)
	}

	// 3. A legal update grows storage; float64(30.0) must consume identically
	// to an integer literal.
	updateBody := map[string]any{
		"spec": map[string]any{"version": "16", "storageGB": float64(30.0), "highAvailability": true},
	}
	response = doJSON(t, handler, http.MethodPut, "/v1/resources/e2e-db", updateBody,
		map[string]string{"Idempotency-Key": "e2e-grow", "If-Liftr-Generation": "1"})
	if response.statusCode != http.StatusAccepted {
		t.Fatalf("update status = %d body=%s", response.statusCode, string(response.body))
	}
	pump()
	resource = getResource(t, handler, "e2e-db")
	if resource.Generation != 2 || resource.Status.State != "Ready" {
		t.Fatalf("after update generation=%d state=%s, want 2/Ready", resource.Generation, resource.Status.State)
	}

	// 4. An illegal transition — storage decrease — is rejected
	// synchronously with structured violations and no durable effect.
	response = doJSON(t, handler, http.MethodPut, "/v1/resources/e2e-db",
		map[string]any{"spec": map[string]any{"version": "16", "storageGB": int64(10), "highAvailability": true}},
		map[string]string{"Idempotency-Key": "e2e-shrink", "If-Liftr-Generation": "2"})
	assertSpecProblem(t, response, "/storageGB")

	// 5. Engine version changes are equally illegal under the v1 contract.
	response = doJSON(t, handler, http.MethodPut, "/v1/resources/e2e-db",
		map[string]any{"spec": map[string]any{"version": "17", "storageGB": float64(30.0), "highAvailability": true}},
		map[string]string{"Idempotency-Key": "e2e-version", "If-Liftr-Generation": "2"})
	assertSpecProblem(t, response, "/version")

	resource = getResource(t, handler, "e2e-db")
	if resource.Generation != 2 {
		t.Fatalf("rejected transitions advanced the generation to %d", resource.Generation)
	}

	// 6. Replaying the admitted grow request resolves from idempotent history
	// even though newer transition rejections exist in the audit trail.
	response = doJSON(t, handler, http.MethodPut, "/v1/resources/e2e-db", updateBody,
		map[string]string{"Idempotency-Key": "e2e-grow", "If-Liftr-Generation": "1"})
	if response.statusCode != http.StatusAccepted || response.header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay status=%d replayed=%q body=%s", response.statusCode,
			response.header.Get("Idempotency-Replayed"), string(response.body))
	}

	// 7. Delete destroys the managed infrastructure and lands in the
	// retained tombstone state.
	response = doJSON(t, handler, http.MethodDelete, "/v1/resources/e2e-db", nil,
		map[string]string{"Idempotency-Key": "e2e-delete", "If-Liftr-Generation": "2"})
	if response.statusCode != http.StatusAccepted {
		t.Fatalf("delete status = %d body=%s", response.statusCode, string(response.body))
	}
	pump()
	resource = getResource(t, handler, "e2e-db")
	if resource.Status.State != "Deleted" {
		t.Fatalf("state = %s after delete, want Deleted", resource.Status.State)
	}

	// 8. Discovery still publishes only developer-contract vocabulary.
	response = doJSON(t, handler, http.MethodGet, "/v1/resource-types/PostgreSQLDatabase/v1", nil, nil)
	if response.statusCode != http.StatusOK {
		t.Fatalf("discovery status = %d", response.statusCode)
	}
	document := strings.ToLower(string(response.body))
	for _, forbidden := range []string{"pulumi", "azure", "subscription"} {
		if strings.Contains(document, forbidden) {
			t.Fatalf("discovery payload leaked implementation vocabulary %q: %s", forbidden, document)
		}
	}
}

type e2EResponse struct {
	statusCode int
	header     http.Header
	body       []byte
}

func doJSON(t *testing.T, handler http.Handler, method, path string, body any, headers map[string]string) e2EResponse {
	t.Helper()
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = encoded
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return e2EResponse{statusCode: recorder.Code, header: recorder.Header(), body: recorder.Body.Bytes()}
}

type e2ECondition struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	Reason string `json:"reason"`
}

type e2EStatus struct {
	State      string         `json:"state"`
	Conditions []e2ECondition `json:"conditions"`
}

type e2EResource struct {
	ID         string         `json:"id"`
	Generation uint64         `json:"generation"`
	Spec       map[string]any `json:"spec"`
	Status     e2EStatus      `json:"status"`
}

type e2EProblem struct {
	Code       string `json:"code"`
	Violations []struct {
		Path    string `json:"path"`
		Keyword string `json:"keyword"`
	} `json:"violations"`
}

func getResource(t *testing.T, handler http.Handler, id string) e2EResource {
	t.Helper()
	response := doJSON(t, handler, http.MethodGet, "/v1/resources/"+id, nil, nil)
	if response.statusCode != http.StatusOK {
		t.Fatalf("get status = %d body=%s", response.statusCode, string(response.body))
	}
	var resource e2EResource
	if err := json.Unmarshal(response.body, &resource); err != nil {
		t.Fatal(err)
	}
	return resource
}

func assertSpecProblem(t *testing.T, response e2EResponse, wantPath string) {
	t.Helper()
	if response.statusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d body=%s, want 422 RESOURCE_SPEC_INVALID", response.statusCode, string(response.body))
	}
	var problem e2EProblem
	if err := json.Unmarshal(response.body, &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "RESOURCE_SPEC_INVALID" {
		t.Fatalf("problem code = %q", problem.Code)
	}
	found := false
	for _, violation := range problem.Violations {
		if violation.Path == wantPath && violation.Keyword == "transition" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no transition violation at %s: %+v", wantPath, problem.Violations)
	}
}

// pump drains durable work until quiescent or bounded.
func pumpUntilSettled(t *testing.T, pumpStep func(context.Context) (bool, error), maxSteps int) {
	t.Helper()
	ctx := context.Background()
	for step := 0; step < maxSteps; step++ {
		found, err := pumpStep(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("worker step failed: %v", err)
		}
		if !found {
			// One message may have been rescheduled with a short backoff
			// (for example a follow-up observation). Give delayed work one
			// grace window before declaring quiescence.
			time.Sleep(50 * time.Millisecond)
			found, err = pumpStep(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("worker step failed: %v", err)
			}
			if !found {
				return
			}
		}
	}
	t.Fatal("worker did not settle within the step bound")
}

func buildRecordingProgram(t *testing.T) (string, string) {
	t.Helper()
	source := t.TempDir()
	goExecutable, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	goExecutable, err = filepath.Abs(goExecutable)
	if err != nil {
		t.Fatal(err)
	}
	build := exec.Command(goExecutable, "build", "-o", filepath.Join(source, "recording"), "./internal/provisioning/pulumi/testdata/recording")
	build.Dir = repoRoot(t)
	build.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build recording program: %v: %s", err, output)
	}
	project := "name: liftr-recording\nruntime:\n  name: go\n  options:\n    binary: ./recording\n"
	if err := os.WriteFile(filepath.Join(source, "Pulumi.yaml"), []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := pulumi.SourceDigest(source)
	if err != nil {
		t.Fatal(err)
	}
	return source, digest
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, packageFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(packageFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot locate repository root")
		}
		dir = parent
	}
}

func newE2ERuntime(t *testing.T, pulumiRoot string) (http.Handler, func()) {
	t.Helper()
	source, digest := buildRecordingProgram(t)

	goExecutable, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	goExecutable, err = filepath.Abs(goExecutable)
	if err != nil {
		t.Fatal(err)
	}
	backend := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(backend, 0o700); err != nil {
		t.Fatal(err)
	}
	config := pulumi.Config{
		Identity: "m9-e2e", StackNamingVersion: pulumi.StackNamingVersionV1, PulumiRoot: pulumiRoot, GoExecutable: goExecutable,
		BackendURL: "file://" + backend, StackNamespace: "m9",
		WorkspaceRoot: t.TempDir(), HistoryPageSize: 10, HistoryMaximumPages: 10, StaleWorkspaceAge: time.Hour,
		Environment: func(context.Context) (map[string]string, error) {
			return map[string]string{"PULUMI_CONFIG_PASSPHRASE": "liftr-non-secret-test-passphrase"}, nil
		},
		Programs: []pulumi.Program{{ResourceType: postgresqldatabase.TypeRef(),
			Capabilities: []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete},
			ProjectName:  "liftr-recording", SourceDir: source, SourceDigest: digest, SecretInputsUnsupported: true,
			EncodeInput: postgresEncoderForTest()}},
	}
	provider, err := pulumi.New(config)
	if err != nil {
		t.Fatal(err)
	}

	contract, err := postgresqldatabase.Contract()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := resourcetypes.NewRegistry(contract)
	if err != nil {
		t.Fatal(err)
	}
	store := appfake.NewStore()
	composed, err := server.Compose(server.Config{
		Transactions:          store,
		Catalog:               catalog,
		Provisioners:          map[application.ProvisionerRef]provisioning.Provisioner{defaultRef: provider},
		DefaultProvisionerRef: defaultRef,
		// A fast retry base keeps delayed follow-up observations inside the
		// test pump's quiescence grace window; production intervals differ.
		WorkerInterval: 5 * time.Millisecond,
		InsecureAuth:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	pump := func() { pumpUntilSettled(t, composed.Worker().RunOnce, 64) }
	return composed.Handler(), pump
}

const defaultRef = application.ProvisionerRef("m9-pulumi")

func postgresEncoderForTest() pulumi.InputEncoder {
	return func(input pulumi.Input) ([]byte, error) {
		values := input.Spec.Values()
		version, _ := values["version"].(string)
		highAvailability, _ := values["highAvailability"].(bool)
		storageGB, ok := domain.IntegralValue(values["storageGB"])
		if !ok || version == "" {
			return nil, fmt.Errorf("program input mapping rejected the submitted spec")
		}
		envelope := map[string]any{
			"inputVersion": 1, "capability": string(input.Capability), "resourceId": string(input.ResourceID),
			"resourceTypeName": input.ResourceType.Name, "resourceTypeVersion": input.ResourceType.Version,
			"targetGeneration": input.TargetGeneration,
			"infraName":        pulumi.InfraName("m9-e2e", "m9", input.ResourceType, input.ResourceID),
			"spec":             map[string]any{"version": version, "storageGB": storageGB, "highAvailability": highAvailability},
		}
		return json.Marshal(envelope)
	}
}
