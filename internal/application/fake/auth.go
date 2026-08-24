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

// AuthorizeResourceList allows enumeration with unrestricted visibility,
// mirroring the insecure development composition's allow-everything policy.
func (AllowAll) AuthorizeResourceList(context.Context, identity.Principal) (identity.ResourceVisibility, error) {
	return identity.ResourceVisibility{AllOwners: true}, nil
}

// DenyAll denies every action, exercising fail-closed paths.
type DenyAll struct{}

// Authorize denies everything.
func (DenyAll) Authorize(context.Context, identity.Principal, identity.Action, identity.ResourceTarget) error {
	return identity.ErrInvalidPrincipal
}

// AuthorizeResourceList denies enumeration entirely.
func (DenyAll) AuthorizeResourceList(context.Context, identity.Principal) (identity.ResourceVisibility, error) {
	return identity.ResourceVisibility{}, identity.ErrInvalidPrincipal
}

// RecordingAuthorizer counts invocations and applies configured denials, so
// tests can assert that admission consulted policy exactly once and that
// worker execution never does. Collection decisions are counted separately:
// ListInvocations must be exactly one per list request (ADR-0016).
type RecordingAuthorizer struct {
	AllowAll AllowAll
	Denied   map[identity.Action]error

	Invocations int
	LastAction  identity.Action
	LastTarget  identity.ResourceTarget
	LastActor   identity.Principal

	// ListErr denies every collection decision when set.
	ListErr error
	// ListScope overrides the returned visibility; nil means unrestricted,
	// matching AllowAll semantics.
	ListScope       *identity.ResourceVisibility
	ListInvocations int
	LastListActor   identity.Principal
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

// AuthorizeResourceList records the collection request and answers from
// configuration.
func (r *RecordingAuthorizer) AuthorizeResourceList(_ context.Context, principal identity.Principal) (identity.ResourceVisibility, error) {
	r.ListInvocations++
	r.LastListActor = principal
	if r.ListErr != nil {
		return identity.ResourceVisibility{}, r.ListErr
	}
	if r.ListScope != nil {
		return *r.ListScope, nil
	}
	return r.AllowAll.AuthorizeResourceList(context.Background(), principal)
}
