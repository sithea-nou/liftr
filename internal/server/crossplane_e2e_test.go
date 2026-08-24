// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	appfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube/fakeapi"
	"github.com/sithea-nou/liftr/internal/provisioning/xrbinding"
	"github.com/sithea-nou/liftr/internal/resourcetypes"
	"github.com/sithea-nou/liftr/internal/resourcetypes/postgresqldatabase"
	"github.com/sithea-nou/liftr/internal/server"
)

// TestEndToEndCrossplaneDeploymentB proves the M14 neutrality claim: the
// identical developer contract (PostgreSQLDatabase/v2, outputs included)
// runs unchanged against a different implementation technology — a
// platform-owned XR on a Crossplane control plane — through the real HTTP
// surface, application orchestration, and durable outbox worker. The fake
// control plane is deterministic in-process infrastructure: no cluster and
// no credentials.
func TestEndToEndCrossplaneDeploymentB(t *testing.T) {
	fake, baseURL := fakeapi.New(t)

	// The simulated composition reconciles at the second poll of each
	// generation: fresh conditions plus the output envelope derived from the
	// XR's own correlation annotations. A webhook-style finalizer holds
	// termination so deletion must wait for genuine physical absence.
	fake.SetController(func(poll int, object *fakeapi.Object) {
		metadata, _ := object.Raw["metadata"].(map[string]any)
		_, terminating := metadata["deletionTimestamp"]
		if terminating {
			// A webhook-installed finalizer holds termination briefly, then
			// cleanup completes and foreground garbage collection removes
			// the object for subsequent readers.
			if len(object.Finalizers) > 0 && poll >= 2 {
				object.Finalizers = nil
			}
			return
		}
		if len(object.Finalizers) == 0 {
			object.SetFinalizer("platform.liftr.io/composition")
		}
		generation := object.Generation()
		status := map[string]any{
			"conditions": []any{
				map[string]any{"type": "Synced", "status": "True", "reason": "ReconciliationSucceeded",
					"observedGeneration": float64(generation), "lastTransitionTime": timestampForXRB(poll + int(generation))},
				map[string]any{"type": "Ready", "status": "True", "reason": "Available",
					"observedGeneration": float64(generation), "lastTransitionTime": timestampForXRB(poll + int(generation)*10)},
			},
		}
		if object.AnnotationValue("liftr.io/resource-id") != "" {
			status["liftr"] = map[string]any{"outputs": xrbEnvelope(t, object)}
		}
		object.Raw["status"] = status
	})

	client, err := kube.NewClient(&kube.RestConfig{Host: baseURL}, 0)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := crossplane.NewWithClient(crossplane.Config{
		Identity:       "deployment-b",
		RequestTimeout: 5 * time.Second,
		Bindings:       []crossplane.Binding{xrbinding.Binding()},
	}, client)
	if err != nil {
		t.Fatal(err)
	}

	contractV2, err := postgresqldatabase.ContractV2()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := resourcetypes.NewRegistry(contractV2)
	if err != nil {
		t.Fatal(err)
	}
	composed, err := server.Compose(server.Config{
		Transactions:          appfake.NewStore(),
		Catalog:               catalog,
		Provisioners:          map[application.ProvisionerRef]provisioning.Provisioner{defaultRef: provider},
		DefaultProvisionerRef: defaultRef,
		WorkerInterval:        5 * time.Millisecond,
		InsecureAuth:          true,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := composed.Handler()
	pump := func() { pumpUntilSettled(t, composed.Worker().RunOnce, 96) }

	// 1. Developer admits a PostgreSQLDatabase/v2 create — zero
	//    implementation vocabulary anywhere in the request.
	createBody := map[string]any{
		"id":    "xr-db",
		"type":  map[string]string{"name": "PostgreSQLDatabase", "version": "v2"},
		"owner": map[string]string{"kind": "team", "id": "platform"},
		"spec":  map[string]any{"version": "16", "storageGB": int64(20), "highAvailability": false},
	}
	response := doJSON(t, handler, http.MethodPost, "/v1/resources", createBody, map[string]string{"Idempotency-Key": "xr-create"})
	if response.statusCode != http.StatusCreated {
		t.Fatalf("create status = %d body=%s", response.statusCode, string(response.body))
	}
	pump()

	resource := getResource(t, handler, "xr-db")
	if resource.Generation != 1 || resource.Status.State != "Ready" {
		t.Fatalf("after create generation=%d state=%s conditions=%+v", resource.Generation, resource.Status.State, resource.Status.Conditions)
	}

	// 2. Required outputs published atomically with reconciliation success.
	outputs := getXROutputs(t, handler, "xr-db")
	if outputs.ObservedGeneration != 1 || outputs.Values["hostname"] == "" {
		t.Fatalf("published outputs = %+v", outputs)
	}
	if port, ok := outputs.Values["port"].(float64); !ok || port != 5432 {
		t.Fatalf("port output = %+v", outputs.Values["port"])
	}

	// 3. Legal update grows storage; readiness must be re-proven for the new
	//    generation (condition freshness + Liftr generation annotation).
	response = doJSON(t, handler, http.MethodPut, "/v1/resources/xr-db",
		map[string]any{"spec": map[string]any{"version": "16", "storageGB": float64(30.0), "highAvailability": true}},
		map[string]string{"Idempotency-Key": "xr-grow", "If-Liftr-Generation": "1"})
	if response.statusCode != http.StatusAccepted {
		t.Fatalf("update status = %d body=%s", response.statusCode, string(response.body))
	}
	pump()
	resource = getResource(t, handler, "xr-db")
	if resource.Generation != 2 || resource.Status.State != "Ready" {
		t.Fatalf("after update generation=%d state=%s", resource.Generation, resource.Status.State)
	}
	outputs = getXROutputs(t, handler, "xr-db")
	if outputs.ObservedGeneration != 2 {
		t.Fatalf("outputs after update = %+v, want generation 2", outputs)
	}

	// 4. Exactly one XR exists for the logical resource across create and
	//    update (retry/reassertion semantics).
	name := crossplane.ObjectName("deployment-b", xrbinding.DefaultNamespace,
		domain.ResourceTypeRef{Name: "PostgreSQLDatabase", Version: "v2"}, "xr-db")
	if _, ok := fake.Get(xrbinding.DefaultNamespace, "xpostgresqldatabases", name); !ok {
		t.Fatalf("expected XR %q to exist", name)
	}
	names := fake.AllNames(xrbinding.DefaultNamespace, "xpostgresqldatabases")
	if len(names) != 1 {
		t.Fatalf("XR count = %d, want exactly one per Resource", len(names))
	}

	// 5. Delete waits for genuine absence behind a held finalizer, then lands
	//    in the Deleted tombstone.
	response = doJSON(t, handler, http.MethodDelete, "/v1/resources/xr-db", nil,
		map[string]string{"Idempotency-Key": "xr-delete", "If-Liftr-Generation": "2"})
	if response.statusCode != http.StatusAccepted {
		t.Fatalf("delete status = %d body=%s", response.statusCode, string(response.body))
	}
	pump()
	resource = getResource(t, handler, "xr-db")
	if resource.Status.State != "Deleted" {
		t.Fatalf("state after delete = %s, want Deleted", resource.Status.State)
	}
	if _, stillThere := fake.Get(xrbinding.DefaultNamespace, "xpostgresqldatabases", name); stillThere {
		t.Fatal("the XR survived a completed delete")
	}

	// 6. Public surfaces stay implementation-blind.
	for _, path := range []string{"/v1/resource-types/PostgreSQLDatabase/v2", "/v1/resources/xr-db"} {
		response = doJSON(t, handler, http.MethodGet, path, nil, nil)
		document := strings.ToLower(string(response.body))
		for _, leaked := range []string{"crossplane", "kubernetes", "composition", "xpostgresqldatabase", "namespace", "liftr.io"} {
			if strings.Contains(document, leaked) {
				t.Fatalf("%s leaked %q: %s", path, leaked, document)
			}
		}
	}

}

type xrOutputs struct {
	ObservedGeneration uint64         `json:"observedGeneration"`
	Values             map[string]any `json:"values"`
}

func getXROutputs(t *testing.T, handler http.Handler, id string) xrOutputs {
	t.Helper()
	response := doJSON(t, handler, http.MethodGet, "/v1/resources/"+id, nil, nil)
	var envelope struct {
		Outputs *xrOutputs `json:"outputs"`
	}
	if err := json.Unmarshal(response.body, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Outputs == nil {
		t.Fatal("resource carries no published outputs")
	}
	return *envelope.Outputs
}

func xrbEnvelope(t *testing.T, object *fakeapi.Object) map[string]any {
	t.Helper()
	return map[string]any{
		"version":          float64(xrbinding.EnvelopeVersion),
		"mapping":          xrbinding.OutputMappingRefV1,
		"resourceId":       object.AnnotationValue("liftr.io/resource-id"),
		"targetGeneration": jsonNumber(t, object.AnnotationValue("liftr.io/target-generation")),
		"values":           map[string]any{"hostname": "db.xr.internal", "port": float64(5432)},
	}
}

func jsonNumber(t *testing.T, decimal string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(decimal), &value); err != nil {
		t.Fatalf("generation annotation %q is not numeric", decimal)
	}
	return value
}

// timestampForXRB renders deterministic condition timestamps safely in the
// past so backend evidence never lands ahead of the test process clock.
func timestampForXRB(minutes int) string {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return base.Add(time.Duration(minutes) * time.Minute).UTC().Format("2006-01-02T15:04:05Z")
}
