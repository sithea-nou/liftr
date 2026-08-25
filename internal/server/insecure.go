// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	apihttp "github.com/sithea-nou/liftr/internal/api/http"
	"github.com/sithea-nou/liftr/internal/identity"
)

// insecureAuthenticator composes the single explicit development mode
// (LIFTR_AUTH_MODE=insecure, ADR-0012). It authenticates every bearer
// credential — including none — as one fixed development principal. This type
// is never constructed by secured composition and never inferred from missing
// configuration; Compose requires an explicit opt-in flag and logs a loud
// warning when it is selected.
type insecureAuthenticator struct {
	principal identity.Principal
}

// newInsecureAuthenticator builds the fixed development principal. Its
// identity is deterministic so idempotency namespaces stay stable within one
// development instance.
func newInsecureAuthenticator() apihttp.Authenticator {
	principal := identity.Principal{
		ID:      identity.NewPrincipalID("liftr://insecure", "development"),
		Kind:    identity.KindUser,
		Issuer:  "liftr://insecure",
		Subject: "development",
		Method:  "insecure",
	}
	return insecureAuthenticator{principal: principal}
}

// Authenticate accepts any credential, including an empty one.
func (a insecureAuthenticator) Authenticate(context.Context, string) (identity.Principal, error) {
	return a.principal, nil
}

// AllowsAnonymous reports that requests without credentials authenticate as
// the development principal. Only explicit insecure composition exposes this
// behavior (ADR-0012).
func (insecureAuthenticator) AllowsAnonymous() bool { return true }

// allowAllAuthorizer grants every action to the development principal. It
// satisfies application.Authorizer structurally and exists only inside
// explicit insecure composition (ADR-0012).
type allowAllAuthorizer struct{}

type allowAllOperatorAuthorizer struct{}

func (allowAllOperatorAuthorizer) AuthorizeOperator(context.Context, identity.Principal, identity.Action, identity.OperatorTarget) error {
	return nil
}

// Authorize allows everything; it must never be reachable from secured runs.
func (allowAllAuthorizer) Authorize(context.Context, identity.Principal, identity.Action, identity.ResourceTarget) error {
	return nil
}

// AuthorizeResourceList mirrors the single-target decision: development
// visibility is unrestricted, because the fixed development principal holds
// no memberships while its creates are authorized under any owner. This is
// the one shipped producer of the unrestricted visibility marker; the secured
// owner-membership policy never emits it (ADR-0016).
func (allowAllAuthorizer) AuthorizeResourceList(context.Context, identity.Principal) (identity.ResourceVisibility, error) {
	return identity.ResourceVisibility{AllOwners: true}, nil
}
