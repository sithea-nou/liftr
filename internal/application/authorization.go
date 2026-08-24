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
// target, and whether a principal may enumerate the Resource inventory at
// all. It is an application-owned consumer port (ADR-0012): the neutral
// vocabulary lives in internal/identity, concrete policy implementations
// satisfy this interface structurally, and composition supplies the
// implementation at startup.
//
// Authorization is admission-time policy. Implementations are consulted only
// by exported business use cases — never by durable worker execution, which
// continues already-admitted Operations regardless of later membership,
// token, or authorizer availability changes.
//
// The single-target decision (Authorize) and the collection decision
// (AuthorizeResourceList) are deliberately separate: a collection has no
// ResourceTarget to authorize against. resource:list is the enumeration
// permission; the returned visibility is what inventory may disclose and must
// never exceed the principal's resource:read scope (ADR-0016). Denying the
// collection while granting reads remains supported; exposing summaries of
// otherwise unreadable Resources through listing does not.
//
// M11 implementations are pure in-memory functions of configuration. An
// implementation that performs network I/O should cache decisions or probe at
// the transport edge; use cases invoke the authorizer inside their page or
// admission transactions where the decision must be atomic with the effects
// it governs.
type Authorizer interface {
	Authorize(ctx context.Context, principal identity.Principal, action identity.Action, target identity.ResourceTarget) error

	// AuthorizeResourceList decides whether the principal may enumerate
	// Resources and returns the closed visibility scope that inventory may
	// disclose. Denial is an error; success with an empty owner set and no
	// unrestricted marker is a valid "authorized but sees nothing" answer,
	// not a denial.
	AuthorizeResourceList(ctx context.Context, principal identity.Principal) (identity.ResourceVisibility, error)
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

// CheckRetryAccess resolves an Operation through its owning Resource and
// authorizes resource:retry against the stored owner. Missing Operations,
// missing owning Resources, and denials intentionally collapse to
// ErrOperationNotFound so transports cannot disclose operation activity.
func (s *Service) CheckRetryAccess(ctx context.Context, principal identity.Principal, id domain.OperationID) (ResourceRecord, error) {
	var resource ResourceRecord
	err := s.Transactions.Within(ctx, func(tx UnitOfWork) error {
		operation, err := tx.Operations().LookupOperation(ctx, id)
		if err != nil {
			return err
		}
		resource, err = tx.Resources().GetResource(ctx, operation.Operation.ResourceID())
		if err != nil {
			if errors.Is(err, ErrResourceNotFound) {
				return ErrOperationNotFound
			}
			return err
		}
		if err := s.authorize(ctx, principal, identity.ActionResourceRetry, resourceTargetOf(resource)); err != nil {
			if errors.Is(err, ErrNotAuthorized) {
				return ErrOperationNotFound
			}
			return err
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrOperationNotFound) {
			return ResourceRecord{}, ErrOperationNotFound
		}
		return ResourceRecord{}, err
	}
	return resource, nil
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
