// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
)

// ErrDenied is the authorizer's deny marker. The application normalizes every
// non-nil denial into its own not-authorized sentinel, so this error never
// crosses the port boundary with policy internals attached.
var ErrDenied = errors.New("denied by authorization policy")

// OwnerAuthorizer is the M11 owner-membership policy:
//
//   - resourceType:read is authenticated-global: every authenticated
//     principal may read discovery, because schemas reveal developer
//     capabilities but never secrets or provider configuration.
//   - resource actions require an exact structural membership match between
//     the target's OwnerRef and one of the principal's typed memberships.
//   - unknown actions — including the reserved secret:resolve — are denied
//     until a milestone explicitly implements them.
//
// It satisfies application.Authorizer structurally; composition proves the
// satisfaction without creating an import from auth to application.
type OwnerAuthorizer struct{}

// Authorize decides one action. Denial returns ErrDenied; the caller is
// responsible for choosing the externally visible form.
func (OwnerAuthorizer) Authorize(_ context.Context, principal identity.Principal, action identity.Action, target identity.ResourceTarget) error {
	if principal.ID == "" {
		return fmt.Errorf("%w: no principal", ErrDenied)
	}
	switch action {
	case identity.ActionResourceTypeRead:
		return nil
	case identity.ActionResourceCreate, identity.ActionResourceRead,
		identity.ActionResourceUpdate, identity.ActionResourceDelete:
		if !principal.IsMember(target.Owner) {
			return fmt.Errorf("%w: no membership under %s/%s", ErrDenied, target.Owner.Kind, target.Owner.ID)
		}
		return nil
	default:
		return fmt.Errorf("%w: action %q is not granted", ErrDenied, string(action))
	}
}

// LoadStaticGrants reads and validates the optional deployment grants file:
// subject-keyed and group-value-keyed maps onto typed owner references. All
// referenced owners must be canonical; parsing fails loudly at startup rather
// than silently granting nothing.
func LoadStaticGrants(path string) (StaticGrants, error) {
	if strings.TrimSpace(path) == "" {
		return StaticGrants{}, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return StaticGrants{}, fmt.Errorf("read auth grants file: %w", err)
	}
	var grants StaticGrants
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&grants); err != nil {
		return StaticGrants{}, fmt.Errorf("parse auth grants file %s: %w", path, err)
	}
	if len(grants.Subjects) == 0 && len(grants.Groups) == 0 {
		return StaticGrants{}, fmt.Errorf("auth grants file %s declares no grants", path)
	}
	for subject, owners := range grants.Subjects {
		if strings.TrimSpace(subject) == "" {
			return StaticGrants{}, fmt.Errorf("auth grants file %s has an empty subject key", path)
		}
		if err := validateGrantedOwners(owners); err != nil {
			return StaticGrants{}, fmt.Errorf("auth grants file %s subject %q: %w", path, subject, err)
		}
	}
	for group, owners := range grants.Groups {
		if strings.TrimSpace(group) == "" {
			return StaticGrants{}, fmt.Errorf("auth grants file %s has an empty group key", path)
		}
		if err := validateGrantedOwners(owners); err != nil {
			return StaticGrants{}, fmt.Errorf("auth grants file %s group %q: %w", path, group, err)
		}
	}
	return grants, nil
}

func validateGrantedOwners(owners []domain.OwnerRef) error {
	if len(owners) == 0 {
		return errors.New("grant declares no owner")
	}
	for _, owner := range owners {
		if strings.TrimSpace(owner.Kind) == "" || strings.TrimSpace(owner.ID) == "" ||
			owner.Kind != strings.TrimSpace(owner.Kind) || owner.ID != strings.TrimSpace(owner.ID) {
			return errors.New("owner kind/id must be canonical non-empty strings")
		}
	}
	return nil
}
