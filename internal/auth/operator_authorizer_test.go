// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sithea-nou/liftr/internal/identity"
)

func TestOperatorGrantsAreStrictAndDenyByDefault(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "grants.json")
	if err := os.WriteFile(path, []byte(`{"subjects":{"operator":["operator:diagnostics:read"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	grants, err := LoadOperatorGrants(path)
	if err != nil {
		t.Fatal(err)
	}
	authorizer := StaticOperatorAuthorizer{Grants: grants}
	principal := identity.Principal{ID: "principal", Kind: identity.KindUser, Subject: "operator"}
	target := identity.OperatorTarget{Kind: identity.OperatorTargetResource, ID: "resource"}
	if err := authorizer.AuthorizeOperator(context.Background(), principal, identity.ActionOperatorDiagnosticsRead, target); err != nil {
		t.Fatalf("granted action denied: %v", err)
	}
	if err := authorizer.AuthorizeOperator(context.Background(), principal, identity.ActionOperatorWorkRecover, target); err == nil {
		t.Fatal("ungranted recovery action was allowed")
	}

	if err := os.WriteFile(path, []byte(`{"subjects":{"operator":["operator:diagnostics:read"]}} {}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOperatorGrants(path); err == nil {
		t.Fatal("trailing JSON content was accepted")
	}
	if err := os.WriteFile(path, []byte(`{"subjects":{"operator":["unknown"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOperatorGrants(path); err == nil {
		t.Fatal("unknown operator action was accepted")
	}
}
