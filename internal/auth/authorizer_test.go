// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/sithea-nou/liftr/internal/auth"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
)

func TestOwnerAuthorizerAppliesMembershipRuleToRetry(t *testing.T) {
	owner := domain.OwnerRef{Kind: "team", ID: "payments"}
	principal, err := identity.NewPrincipal(identity.KindUser, "https://issuer.example", "alice", "test", []domain.OwnerRef{owner})
	if err != nil {
		t.Fatal(err)
	}
	target := identity.ResourceTarget{Owner: owner}
	authorizer := auth.OwnerAuthorizer{}

	if err := authorizer.Authorize(context.Background(), principal, identity.ActionResourceRetry, target); err != nil {
		t.Fatalf("member retry denied: %v", err)
	}
	target.Owner = domain.OwnerRef{Kind: "team", ID: "platform"}
	if err := authorizer.Authorize(context.Background(), principal, identity.ActionResourceRetry, target); !errors.Is(err, auth.ErrDenied) {
		t.Fatalf("non-member retry error = %v, want ErrDenied", err)
	}
}
