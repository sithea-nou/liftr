// SPDX-License-Identifier: Apache-2.0

package crossplane

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube"
)

// TestL2RealAPIServerIdentityPreconditionDiscovery is the opt-in Layer-2
// suite: it exercises deterministic object identity, UID delete
// preconditions, resourceVersion-preconditioned applies, and served-GVR
// discovery disambiguation against a real Kubernetes API server. It requires
// no Crossplane installation and no cloud provider — only a throwaway CRD.
//
//	LIFTR_TEST_CROSSPLANE_KUBECONFIG=/path/to/kubeconfig go test ./internal/provisioning/crossplane -run TestL2 -v
//
// It is never part of `make verify`.
func TestL2RealAPIServerIdentityPreconditionDiscovery(t *testing.T) {
	kubeconfigPath := testEnvOrSkip(t, "LIFTR_TEST_CROSSPLANE_KUBECONFIG")
	restConfig, err := kube.RestConfigFromKubeconfig(kubeconfigPath, testEnvOr("LIFTR_TEST_CROSSPLANE_CONTEXT"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := kube.NewClient(restConfig, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	const (
		group     = "platform.liftr.io"
		version   = "v1alpha1"
		plural    = "xtestresources"
		namespace = "liftr-l2"
	)
	gvr := kube.GVR{Group: group, Version: version, Resource: plural}
	ctx := context.Background()

	createNamespace(t, ctx, client, namespace)
	defer func() {
		_ = teardownNamespace(t, ctx, client, namespace)
	}()
	crdGVR := kube.GVR{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	crd := kube.NewObject(map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": plural + "." + group},
		"spec": map[string]any{
			"group": group,
			"names": map[string]any{"plural": plural, "singular": "xtestresource", "kind": "XTestResource"},
			"scope": "Namespaced",
			"versions": []any{map[string]any{
				"name": version, "served": true, "storage": true,
				"schema": map[string]any{"openAPIV3Schema": map[string]any{
					"type": "object", "x-kubernetes-preserve-unknown-fields": true,
				}},
			}},
		},
	})
	if _, err := client.Create(ctx, crdGVR, "", crd); err != nil && !kube.IsAlreadyExists(err) {
		t.Fatalf("create CRD: %v", err)
	}
	t.Cleanup(func() {
		_ = client.Delete(context.Background(), crdGVR, "", plural+"."+group, "")
	})

	// Wait for the API resource to actually appear in live discovery.
	waitForServed(t, ctx, client, gvr, true)

	l2OwnershipAndPreconditions(t, ctx, client, gvr, namespace)
	l2ResourceVersionPrecondition(t, ctx, client, gvr, namespace)
	l2DiscoveryDisambiguation(t, ctx, client, gvr, namespace)
}

func l2OwnershipAndPreconditions(t *testing.T, ctx context.Context, client kube.Client, gvr kube.GVR, namespace string) {
	t.Helper()
	owner := identityMetadata{
		platformDigest: PlatformDigest("l2-install"),
		resourceID:     "l2-resource",
		resourceType:   domain.ResourceTypeRef{Name: "PostgreSQLDatabase", Version: "v2"},
	}
	document := map[string]any{
		"apiVersion": gvr.Group + "/" + gvr.Version,
		"kind":       "XTestResource",
		"metadata":   map[string]any{"name": "liftr-l2-target"},
		"spec":       map[string]any{"desired": true},
	}
	owner.stamp(document)
	stampOperationCorrelation(document, "op-l2", 3)

	created, err := client.Create(ctx, gvr, namespace, kube.NewObject(document))
	if err != nil {
		t.Fatalf("L2 create failed: %v", err)
	}
	if !owner.verify(created) {
		t.Fatal("real API server did not round-trip ownership metadata")
	}
	if !operationCorrelated(created, "op-l2", 3) {
		t.Fatal("real API server did not round-trip operation correlation")
	}

	// A UID precondition against the wrong physical identity must conflict.
	wrongUIDDeleteErr := client.Delete(ctx, gvr, namespace, created.Name(), "uid-that-does-not-exist")
	if !kube.IsConflict(wrongUIDDeleteErr) {
		t.Fatalf("UID precondition mismatch produced %v, want 409 Conflict", wrongUIDDeleteErr)
	}

	// The correct UID deletes exactly the verified object.
	if err := client.Delete(ctx, gvr, namespace, created.Name(), created.UID()); err != nil {
		t.Fatalf("UID-preconditioned delete failed: %v", err)
	}
	if _, getErr := client.Get(ctx, gvr, namespace, created.Name()); !kube.IsNotFound(getErr) {
		t.Fatalf("post-delete GET = %v, want NotFound", getErr)
	}
}

func l2ResourceVersionPrecondition(t *testing.T, ctx context.Context, client kube.Client, gvr kube.GVR, namespace string) {
	t.Helper()
	document := map[string]any{
		"apiVersion": gvr.Group + "/" + gvr.Version,
		"kind":       "XTestResource",
		"metadata":   map[string]any{"name": "liftr-l2-rv"},
		"spec":       map[string]any{"generation": float64(1)},
	}
	owner := identityMetadata{platformDigest: PlatformDigest("l2-install"), resourceID: "l2-rv", resourceType: domain.ResourceTypeRef{Name: "X", Version: "v1"}}
	owner.stamp(document)
	if _, err := client.Create(ctx, gvr, namespace, kube.NewObject(document)); err != nil {
		t.Fatalf("rv fixture create failed: %v", err)
	}
	fresh, err := client.Get(ctx, gvr, namespace, "liftr-l2-rv")
	if err != nil {
		t.Fatal(err)
	}

	updatedDocument := fresh.Clone().Raw()
	updatedDocument["spec"] = map[string]any{"generation": float64(2)}
	if _, updateErr := client.Update(ctx, gvr, namespace, "liftr-l2-rv", kube.NewObject(updatedDocument), fresh.ResourceVersion()); updateErr != nil {
		t.Fatalf("same-identity conditional update failed: %v", updateErr)
	}

	stale := fresh.ResourceVersion()
	conflictDocument := fresh.Clone().Raw()
	conflictDocument["spec"] = map[string]any{"generation": float64(3)}
	_, updateErr := client.Update(ctx, gvr, namespace, "liftr-l2-rv", kube.NewObject(conflictDocument), stale)
	if !kube.IsConflict(updateErr) {
		t.Fatalf("stale-resourceVersion update produced %v, want 409 Conflict", updateErr)
	}
	current, err := client.Get(ctx, gvr, namespace, "liftr-l2-rv")
	if err != nil {
		t.Fatal(err)
	}
	if current.Spec()["generation"] != float64(2) {
		t.Fatalf("rejected write mutated the object anyway: %+v", current.Raw())
	}
}

func l2DiscoveryDisambiguation(t *testing.T, ctx context.Context, client kube.Client, gvr kube.GVR, namespace string) {
	t.Helper()
	if verdict := client.ServedResource(ctx, gvr); verdict != kube.ServedConfirmed {
		t.Fatalf("live discovery verdict = %d, want Confirmed while the CRD exists", verdict)
	}

	document := map[string]any{
		"apiVersion": gvr.Group + "/" + gvr.Version,
		"kind":       "XTestResource",
		"metadata":   map[string]any{"name": "liftr-l2-absent-after-crd"},
		"spec":       map[string]any{},
	}
	identity := identityMetadata{platformDigest: PlatformDigest("l2-install"), resourceID: "l2-disc", resourceType: domain.ResourceTypeRef{Name: "X", Version: "v1"}}
	identity.stamp(document)
	if _, err := client.Create(ctx, gvr, namespace, kube.NewObject(document)); err != nil {
		t.Fatal(err)
	}
	name := "liftr-l2-absent-after-crd"

	crdGVR := kube.GVR{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	if err := client.Delete(ctx, crdGVR, "", gvr.Resource+"."+gvr.Group, ""); err != nil && !kube.IsNotFound(err) {
		t.Fatalf("retire CRD: %v", err)
	}
	waitForServed(t, ctx, client, gvr, false)

	if _, getErr := client.Get(ctx, gvr, namespace, name); !kube.IsNotFound(getErr) {
		t.Fatalf("object GET after CRD removal = %v, want NotFound", getErr)
	}
	// The decisive invariant: with the kind unserved, the object 404 must
	// NOT establish managed absence. The adapter resolves this exact
	// combination through resolveAbsence, which requires a Confirmed
	// discovery answer; here the live verdict is Refuted, so any conclusion
	// drawn would be kindNotServed, never absenceProven.
	if verdict := client.ServedResource(ctx, gvr); verdict == kube.ServedConfirmed {
		t.Fatal("discovery still confirms a retired resource")
	}
}

func waitForServed(t *testing.T, ctx context.Context, client kube.Client, gvr kube.GVR, served bool) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		if client.ServedResource(ctx, gvr) == servedVerdict(served) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("discovery did not reach served=%v within the deadline", served)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func servedVerdict(served bool) kube.ServedVerdict {
	if served {
		return kube.ServedConfirmed
	}
	return kube.ServedRefuted
}

func createNamespace(t *testing.T, ctx context.Context, client kube.Client, name string) {
	t.Helper()
	object := kube.NewObject(map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]any{"name": name},
	})
	if _, err := client.Create(ctx, kube.GVR{Version: "v1", Resource: "namespaces"}, "", object); err != nil && !kube.IsAlreadyExists(err) {
		t.Fatalf("create namespace: %v", err)
	}
}

func teardownNamespace(t *testing.T, ctx context.Context, client kube.Client, name string) error {
	t.Helper()
	if err := client.Delete(ctx, kube.GVR{Version: "v1", Resource: "namespaces"}, "", name, ""); err != nil && !kube.IsNotFound(err) {
		return fmt.Errorf("delete namespace: %w", err)
	}
	return nil
}

func testEnvOr(key string) string { return os.Getenv(key) }

func testEnvOrSkip(t *testing.T, key string) string {
	t.Helper()
	value := os.Getenv(key)
	if value == "" {
		t.Skip("LIFTR_TEST_CROSSPLANE_KUBECONFIG is required for the real-API-server L2 suite")
	}
	return value
}
