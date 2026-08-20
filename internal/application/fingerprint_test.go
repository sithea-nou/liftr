// SPDX-License-Identifier: Apache-2.0

package application

import (
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
)

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
	first, err := createCommandFingerprint(CreateResourceCommand{
		ID: "r\x00web", Type: domain.ResourceTypeRef{Name: "app", Version: "v1"},
		Owner: domain.OwnerRef{Kind: "team", ID: "platform"}, Spec: spec,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := createCommandFingerprint(CreateResourceCommand{
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
