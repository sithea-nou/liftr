// SPDX-License-Identifier: Apache-2.0

package identity_test

import (
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
)

func TestPrincipalIDIsDeterministicPerIssuerAndSubject(t *testing.T) {
	first := identity.NewPrincipalID("https://idp.example", "alice")
	second := identity.NewPrincipalID("https://idp.example", "alice")
	if first != second {
		t.Fatal("same issuer+subject produced different IDs")
	}
	if !strings.HasPrefix(string(first), "prn_v1_") || len(first) != len("prn_v1_")+64 {
		t.Fatalf("principal ID %q lacks the prn_v1_<sha256> shape", first)
	}
	if err := identity.ValidatePrincipalID(first); err != nil {
		t.Fatalf("ValidatePrincipalID rejected a derived ID: %v", err)
	}
}

func TestPrincipalIDDistinguishesIssuersForSameSubject(t *testing.T) {
	a := identity.NewPrincipalID("https://idp-a.example", "alice")
	b := identity.NewPrincipalID("https://idp-b.example", "alice")
	if a == b {
		t.Fatal("different issuers with same subject collided")
	}
}

func TestPrincipalIDDistinguishesSubjectsWithinOneIssuer(t *testing.T) {
	a := identity.NewPrincipalID("https://idp.example", "alice")
	b := identity.NewPrincipalID("https://idp.example", "bob")
	if a == b {
		t.Fatal("same issuer with different subjects collided")
	}
}

func TestPrincipalIDResistsDelimiterAmbiguity(t *testing.T) {
	// ("a", "b/c") and ("a/b", "c") must never derive equal IDs.
	first := identity.NewPrincipalID("issuer-a", "subject/b")
	second := identity.NewPrincipalID("issuer-a/subject", "b")
	if first == second {
		t.Fatal("unframed concatenation ambiguity detected in principal derivation")
	}
}

func TestValidatePrincipalIDRejectsMalformedValues(t *testing.T) {
	for _, bad := range []identity.PrincipalID{"", "control-plane", "prn_v2_deadbeef",
		"prn_v1_zzz", identity.PrincipalID("prn_v1_" + strings.Repeat("a", 63)), identity.NewPrincipalID("i", "s") + "x"} {
		if err := identity.ValidatePrincipalID(bad); err == nil {
			t.Fatalf("ValidatePrincipalID accepted %q", bad)
		}
	}
}

func TestNewPrincipalNormalizesMembershipsDeterministically(t *testing.T) {
	principal, err := identity.NewPrincipal(identity.KindUser, "https://idp.example", "alice", "oidc",
		[]domain.OwnerRef{
			{Kind: "team", ID: "payments"},
			{Kind: "team", ID: "platform"},
			{Kind: "team", ID: "payments"}, // duplicate
			{Kind: "", ID: "broken"},       // invalid, dropped
			{Kind: "team", ID: ""},         // invalid, dropped
		})
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.OwnerRef{{Kind: "team", ID: "payments"}, {Kind: "team", ID: "platform"}}
	if len(principal.Memberships) != len(want) {
		t.Fatalf("memberships = %v, want %v", principal.Memberships, want)
	}
	for index, owner := range want {
		if principal.Memberships[index] != owner {
			t.Fatalf("memberships[%d] = %v, want %v (order must be deterministic)", index, principal.Memberships[index], owner)
		}
	}
}

func TestNewPrincipalRejectsInvalidInputs(t *testing.T) {
	for _, test := range []struct {
		kind    identity.PrincipalKind
		issuer  string
		subject string
	}{
		{identity.PrincipalKind("robot"), "https://idp.example", "alice"},
		{identity.KindUser, "", "alice"},
		{identity.KindUser, "https://idp.example", ""},
	} {
		if _, err := identity.NewPrincipal(test.kind, test.issuer, test.subject, "test", nil); err == nil {
			t.Fatalf("NewPrincipal(%q,%q,%q) was accepted", test.kind, test.issuer, test.subject)
		}
	}
}

func TestIsMemberUsesStructuralComparisonOnly(t *testing.T) {
	principal, err := identity.NewPrincipal(identity.KindServiceAccount, "https://idp.example", "ci", "test",
		[]domain.OwnerRef{{Kind: "team", ID: "payments"}})
	if err != nil {
		t.Fatal(err)
	}
	if !principal.IsMember(domain.OwnerRef{Kind: "team", ID: "payments"}) {
		t.Fatal("exact structural membership not recognized")
	}
	for _, other := range []domain.OwnerRef{
		{Kind: "teams", ID: "payments"}, // kind prefix collision
		{Kind: "team", ID: "paymentss"}, // id prefix collision
		{Kind: "Team", ID: "payments"},  // case difference
		{Kind: "team", ID: "payments "}, // trailing space
	} {
		if principal.IsMember(other) {
			t.Fatalf("membership matched non-equal owner %+v; comparison must be exact", other)
		}
	}
}
