// SPDX-License-Identifier: Apache-2.0

package application

import (
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
)

func testActor() identity.Principal {
	principal, err := identity.NewPrincipal(identity.KindUser, "https://test.liftr.dev", "tester", "test", nil)
	if err != nil {
		panic(err)
	}
	return principal
}

func TestFingerprintHashNulCollision(t *testing.T) {
	first := fingerprintHash("r\x00web", "app/v1")
	second := fingerprintHash("r", "web\x00app/v1")
	if first == second {
		t.Fatalf("fingerprints collide across NUL-joined parts: %s", first)
	}
}

func TestFingerprintHashPartBoundaries(t *testing.T) {
	joined := fingerprintHash("ab", "c")
	split := fingerprintHash("a", "bc")
	if joined == split {
		t.Fatalf("fingerprints collide across part boundaries: %s", joined)
	}
}

func TestCreateCommandFingerprintNulDistinguishesResourceAndType(t *testing.T) {
	spec, err := domain.NewResourceSpec(map[string]any{"intent": "nul"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := createCommandFingerprint(CreateResourceCommand{Actor: testActor(),
		ID: "r\x00web", Type: domain.ResourceTypeRef{Name: "app", Version: "v1"},
		Owner: domain.OwnerRef{Kind: "team", ID: "platform"}, Spec: spec,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := createCommandFingerprint(CreateResourceCommand{Actor: testActor(),
		ID: "r", Type: domain.ResourceTypeRef{Name: "web\x00app", Version: "v1"},
		Owner: domain.OwnerRef{Kind: "team", ID: "platform"}, Spec: spec,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("command fingerprints collide across NUL-joined parts: %s", first)
	}
}

func TestRetryCommandFingerprintV1CoversOnlySourceAndExpectedGeneration(t *testing.T) {
	command := RetryOperationCommand{OperationID: "operation-source", ExpectedGeneration: 42}
	want := fingerprintHash("retry-operation-v1", "operation-source", "42")
	if got := retryCommandFingerprint(command); got != want {
		t.Fatalf("retry fingerprint = %q, want %q", got, want)
	}
	changed := command
	changed.NewOperationID = "ignored-child-id"
	changed.EventID = "ignored-event-id"
	if got := retryCommandFingerprint(changed); got != want {
		t.Fatalf("non-fingerprinted retry fields changed fingerprint to %q", got)
	}
}
