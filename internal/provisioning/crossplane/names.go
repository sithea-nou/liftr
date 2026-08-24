// SPDX-License-Identifier: Apache-2.0

// Package crossplane implements Liftr's provider-neutral Provisioner
// contract against a Crossplane control plane using platform-owned composite
// resources. The adapter owns every Kubernetes concept privately: nothing
// here appears in ResourceSpec, ResourceStatus, discovery, Operations, or
// the HTTP API.
package crossplane

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/sithea-nou/liftr/internal/domain"
)

// NamingVersionV1 is the only supported object-naming algorithm.
const NamingVersionV1 = "v1"

// PlatformDigest derives the stable installation marker stamped onto owned
// objects. Two installations configured with distinct identities never
// recognize each other's objects even when ResourceIDs collide.
func PlatformDigest(identity string) string {
	digest := sha256.Sum256([]byte("liftr-crossplane\x00" + identity))
	return hex.EncodeToString(digest[:])[:16]
}

// ObjectName derives the deterministic private XR name for one Liftr
// Resource under one binding. Properties pinned by tests:
//
//   - stable across restarts, updates, retries, and deletes;
//   - distinct installations derive distinct names for identical ResourceIDs;
//   - OperationID, attempt number, and generation are never inputs;
//   - the name satisfies Kubernetes DNS subdomain constraints.
func ObjectName(identity, xrNamespace string, ref domain.ResourceTypeRef, id domain.ResourceID) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		identity, xrNamespace, ref.Name, ref.Version, string(id),
	}, "\x00")))
	return "liftr-" + hex.EncodeToString(digest[:])[:20]
}

func resourceTypeAnnotation(ref domain.ResourceTypeRef) string {
	return fmt.Sprintf("%s/%s", ref.Name, ref.Version)
}
