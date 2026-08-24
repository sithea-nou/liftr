// SPDX-License-Identifier: Apache-2.0

package identity

import (
	"fmt"

	"github.com/sithea-nou/liftr/internal/domain"
)

// Action is one authorization decision Liftr can be asked to make. The set is
// deliberately small; future actions (secret:resolve, admin:*) join without
// breaking the port.
type Action string

const (
	ActionResourceCreate   Action = "resource:create"
	ActionResourceRead     Action = "resource:read"
	ActionResourceUpdate   Action = "resource:update"
	ActionResourceDelete   Action = "resource:delete"
	ActionResourceRetry    Action = "resource:retry"
	ActionResourceList     Action = "resource:list"
	ActionResourceTypeRead Action = "resourceType:read"
)

// ActionResourceList is the enumeration permission. It is decided only
// through the collection authorization path (application.Authorizer's list
// method), never through single-target Authorize calls, because a collection
// has no ResourceTarget. The default owner-membership policy grants it
// together with resource:read from the same memberships; a future policy may
// grant read-without-list or list-without-read, but inventory visibility must
// never exceed what resource:read would disclose (ADR-0016).

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
