// SPDX-License-Identifier: Apache-2.0

package fake

import (
	"context"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
)

// TestIssuer is the deterministic issuer used by test principals.
const TestIssuer = "https://test.liftr.dev"

// Principal returns a valid deterministic test principal whose stable ID
// derives from name alone, optionally carrying typed owner memberships.
func Principal(name string, owners ...domain.OwnerRef) identity.Principal {
	principal, err := identity.NewPrincipal(identity.KindUser, TestIssuer, name, "test", owners)
	if err != nil {
		panic(err)
	}
	return principal
}

// Owner returns one typed owner reference.
func Owner(kind, id string) domain.OwnerRef {
	return domain.OwnerRef{Kind: kind, ID: id}
}

// AllowAll grants every action to every principal. Tests compose it where
// authorization itself is not under examination.
type AllowAll struct{}

// Authorize allows everything.
func (AllowAll) Authorize(context.Context, identity.Principal, identity.Action, identity.ResourceTarget) error {
	return nil
}

// DenyAll denies every action, exercising fail-closed paths.
type DenyAll struct{}

// Authorize denies everything.
func (DenyAll) Authorize(context.Context, identity.Principal, identity.Action, identity.ResourceTarget) error {
	return identity.ErrInvalidPrincipal
}

// RecordingAuthorizer counts invocations and applies configured denials, so
// tests can assert that admission consulted policy exactly once and that
// worker execution never does.
type RecordingAuthorizer struct {
	AllowAll AllowAll
	Denied   map[identity.Action]error

	Invocations int
	LastAction  identity.Action
	LastTarget  identity.ResourceTarget
	LastActor   identity.Principal
}

// Authorize records the decision request and answers from configuration.
func (r *RecordingAuthorizer) Authorize(_ context.Context, principal identity.Principal, action identity.Action, target identity.ResourceTarget) error {
	r.Invocations++
	r.LastAction = action
	r.LastTarget = target
	r.LastActor = principal
	if denial, denied := r.Denied[action]; denied {
		return denial
	}
	return nil
}
