// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"fmt"

	"github.com/sithea-nou/liftr/internal/domain"
)

// Action is one authorization decision Liftr can be asked to make. The set is
// deliberately small; future actions (secret:resolve, resource:retry,
// admin:*) join without breaking the port.
type Action string

const (
	ActionResourceCreate   Action = "resource:create"
	ActionResourceRead     Action = "resource:read"
	ActionResourceUpdate   Action = "resource:update"
	ActionResourceDelete   Action = "resource:delete"
	ActionResourceTypeRead Action = "resourceType:read"
)

// ActionSecretResolve is reserved for the future SecretReference resolver
// defined by ADR-0011. It is intentionally never produced by M11 code paths;
// its presence pins that secret authorization stays independent of
// resource:read.
const ActionSecretResolve Action = "secret:resolve"

// ResourceTarget is the context of one authorization decision. For create
// admissions it carries the requested type and owner with an empty ID; for
// reads and mutations it carries the stored values. Operation reads authorize
// through their owning Resource and never receive an independent target.
type ResourceTarget struct {
	Type       domain.ResourceTypeRef
	Owner      domain.OwnerRef
	ResourceID domain.ResourceID
}

// String renders a stable, log-safe description of a decision request. It
// never includes credentials or raw claim data.
func (t ResourceTarget) String() string {
	return fmt.Sprintf("type=%s/%s owner=%s/%s resource=%s", t.Type.Name, t.Type.Version, t.Owner.Kind, t.Owner.ID, t.ResourceID)
}
