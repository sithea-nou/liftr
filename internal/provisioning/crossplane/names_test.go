// SPDX-License-Identifier: Apache-2.0

package crossplane

import (
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube"
)

func TestObjectNameIsStableAcrossOperationsAttemptsAndGenerations(t *testing.T) {
	base := ObjectName("install-a", "liftr-test", testResourceType, "resource-1")
	for range 10 {
		if again := ObjectName("install-a", "liftr-test", testResourceType, "resource-1"); again != base {
			t.Fatalf("object identity changed across derivations: %q vs %q", base, again)
		}
	}
}

func TestObjectNameExcludesOperationAttemptAndGeneration(t *testing.T) {
	// The derivation is a pure function of platform identity, namespace,
	// resource type ref, and resource ID; there is no input channel for
	// execution dimensions. This pins the contract structurally.
	first := ObjectName("i", "ns", testResourceType, "r")
	second := ObjectName("i", "ns", testResourceType, "r")
	if first != second {
		t.Fatal("identity derivation is nondeterministic")
	}
	if !strings.HasPrefix(first, "liftr-") || len(first) != 26 {
		t.Fatalf("derived name %q violates DNS-label shape", first)
	}
	for _, character := range strings.TrimPrefix(first, "liftr-") {
		isLower := character >= '0' && character <= '9' || character >= 'a' && character <= 'f'
		if !isLower {
			t.Fatalf("derived name %q contains non-hex character %q", first, character)
		}
	}
}

func TestObjectNamesDivergeByPlatformIdentityAndResourceID(t *testing.T) {
	sameInstallDifferentResource := ObjectName("install-a", "ns", testResourceType, "resource-1") != ObjectName("install-a", "ns", testResourceType, "resource-2")
	differentInstallSameResource := ObjectName("install-a", "ns", testResourceType, "resource-1") != ObjectName("install-b", "ns", testResourceType, "resource-1")
	differentNamespace := ObjectName("install-a", "ns-one", testResourceType, "resource-1") != ObjectName("install-a", "ns-two", testResourceType, "resource-1")
	differentTypeVersion := ObjectName("install-a", "ns", domain.ResourceTypeRef{Name: "TestResource", Version: "v2"}, "resource-1") != ObjectName("install-a", "ns", testResourceType, "resource-1")
	if !sameInstallDifferentResource || !differentInstallSameResource || !differentNamespace || !differentTypeVersion {
		t.Fatal("identity collisions across installation dimensions")
	}
}

func TestPlatformDigestSeparatesInstallations(t *testing.T) {
	if PlatformDigest("one") == PlatformDigest("two") {
		t.Fatal("distinct installations share a platform digest")
	}
	if PlatformDigest("one") != PlatformDigest("one") {
		t.Fatal("platform digest is unstable")
	}
}

func TestExecutionHandleRoundTrip(t *testing.T) {
	binding := resolvedBinding{
		gvr:       kube.GVR{Group: "platform.liftr.io", Version: "v1alpha1", Resource: "xtestresources"},
		kind:      "XTestResource",
		namespace: "liftr-test",
	}
	handle := encodeHandle(&binding, "liftr-abc", "")
	payload, ok := decodeHandle(&handle)
	if !ok {
		t.Fatal("issued handle could not be decoded")
	}
	if payload.Name != "liftr-abc" || payload.Namespace != "liftr-test" || payload.Kind != "XTestResource" || payload.UID != "" {
		t.Fatalf("decoded payload = %+v", payload)
	}

	withUID := encodeHandle(&binding, "liftr-abc", "uid-42")
	payload, ok = decodeHandle(&withUID)
	if !ok || payload.UID != "uid-42" {
		t.Fatalf("UID-bearing handle decoded to %+v ok=%v", payload, ok)
	}
}

func TestForeignHandlesAreRejected(t *testing.T) {
	foreign, err := provisioning.NewExecutionHandle("totally-different-scheme")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decodeHandle(&foreign); ok {
		t.Fatal("a non-crossplane handle token was accepted")
	}
	if _, ok := decodeHandle(nil); ok {
		t.Fatal("a nil handle was accepted")
	}
}
