// SPDX-License-Identifier: Apache-2.0

package application

import (
	"context"
	"errors"
	"fmt"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
)

// ErrNotAuthorized reports that an authenticated principal was denied an
// action. Transports choose the externally visible form (403 FORBIDDEN, or a
// Resource-absent 404 under the approved existence-hiding policy); this
// sentinel never carries policy internals.
var ErrNotAuthorized = errors.New("not authorized")

// Authorizer decides whether a principal may perform an action against a
// target. It is an application-owned consumer port (ADR-0012): the neutral
// vocabulary lives in internal/identity, concrete policy implementations
// satisfy this interface structurally, and composition supplies the
// implementation at startup.
//
// Authorization is admission-time policy. Implementations are consulted only
// by exported business use cases — never by durable worker execution, which
// continues already-admitted Operations regardless of later membership,
// token, or authorizer availability changes.
//
// M11 implementations are pure in-memory functions of configuration. An
// implementation that performs network I/O should cache decisions or probe at
// the transport edge; use cases invoke the authorizer inside their admission
// transactions where the decision must be atomic with persisted effects.
type Authorizer interface {
	Authorize(ctx context.Context, principal identity.Principal, action identity.Action, target identity.ResourceTarget) error
}

// retryAction maps a retried capability onto its admission action. Retrying a
// failed Operation re-executes the same lifecycle intent, so it requires the
// same permission as the original submission.
func retryAction(capability domain.Capability) identity.Action {
	switch capability {
	case domain.CapabilityCreate:
		return identity.ActionResourceCreate
	case domain.CapabilityDelete:
		return identity.ActionResourceDelete
	default:
		return identity.ActionResourceUpdate
	}
}

// authorize enforces the authentication precondition and delegates the
// decision to the configured authorizer. It fails closed: a missing principal
// or a missing authorizer denies every action.
func (s *Service) authorize(ctx context.Context, principal identity.Principal, action identity.Action, target identity.ResourceTarget) error {
	if principal.ID == "" || principal.Kind == "" {
		return ErrNotAuthorized
	}
	if s.Authorizer == nil {
		return ErrNotAuthorized
	}
	if err := s.Authorizer.Authorize(ctx, principal, action, target); err != nil {
		// Every denial normalizes into one sentinel so policy internals stay
		// behind the port and transports branch on a single error value.
		return fmt.Errorf("%w", ErrNotAuthorized)
	}
	return nil
}

// AuthorizeCreateAction reports whether the principal may admit a create for
// the requested ResourceType and OwnerRef. Transports call it after
// structural request parsing and before admission so denial semantics stay
// uniform; the admission transaction re-checks the decision.
func (s *Service) AuthorizeCreateAction(ctx context.Context, principal identity.Principal, ref domain.ResourceTypeRef, owner domain.OwnerRef) error {
	return s.authorize(ctx, principal, identity.ActionResourceCreate, identity.ResourceTarget{Type: ref, Owner: owner})
}

// CheckResourceAccess reports whether the principal may perform action on the
// retained Resource. It distinguishes absence (ErrResourceNotFound) from
// denial (ErrNotAuthorized) so transports can render the approved
// indistinguishable 404 for both while application callers can tell them
// apart.
func (s *Service) CheckResourceAccess(ctx context.Context, principal identity.Principal, id domain.ResourceID, action identity.Action) error {
	record, err := s.loadResource(ctx, id)
	if err != nil {
		return err
	}
	target := resourceTargetOf(record)
	return s.authorize(ctx, principal, action, target)
}

// resourceTargetOf builds the authorization context of a stored Resource.
func resourceTargetOf(record ResourceRecord) identity.ResourceTarget {
	resource := record.Resource
	return identity.ResourceTarget{Type: resource.Type(), Owner: resource.Owner(), ResourceID: resource.ID()}
}

// idempotencyScope derives the per-principal idempotency namespace. Keys are
// unique within one principal's namespace and never collide across principals
// (ADR-0012); possession of another principal's key grants nothing.
func idempotencyScope(principal identity.Principal) string {
	return string(principal.ID)
}
