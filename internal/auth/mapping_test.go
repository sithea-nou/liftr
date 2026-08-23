// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/auth"
	"github.com/sithea-nou/liftr/internal/domain"
)

func membershipsFrom(t *testing.T, mapper auth.ClaimMapper, groupsJSON string, subject string) []domain.OwnerRef {
	t.Helper()
	var claims map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{"groups":`+groupsJSON+`}`), &claims); err != nil {
		t.Fatal(err)
	}
	return mapper.MembershipsOf(claims, subject)
}

// TestMembershipEncodingCollisionsGrantNothing pins correction 2: malformed,
// ambiguous, or differently spelled entries never collapse into another
// owner's identity; authorization compares typed values exactly.
func TestMembershipEncodingCollisionsGrantNothing(t *testing.T) {
	mapper := auth.ClaimMapper{GroupClaim: "groups", Prefix: "liftr:"}

	memberships := membershipsFrom(t, mapper,
		`["team:payments","te am:payments","Team:payments","team:paymentss","team:payments ","","::",":","x",
		  "liftr:team:platform","xliftr:team:secret","liftr:liftr:team:payments"]`, "alice")

	granted := map[domain.OwnerRef]bool{}
	for _, membership := range memberships {
		granted[membership] = true
	}
	// A configured prefix makes it part of the convention: unprefixed
	// entries grant nothing, prefixed entries map once, and double prefixes
	// degrade to a distinct owner instead of colliding.
	wantGranted := map[domain.OwnerRef]bool{
		{Kind: "team", ID: "platform"}:       true,
		{Kind: "liftr", ID: "team:payments"}: true,
	}
	for owner := range granted {
		if !wantGranted[owner] {
			t.Fatalf("unexpected membership %v granted from hostile claim set; full set: %v", owner, memberships)
		}
	}
	for _, forbidden := range []domain.OwnerRef{
		{Kind: "te am", ID: "payments"},
		{Kind: "Team", ID: "payments"},
		{Kind: "team", ID: "paymentss"},
		{Kind: "team", ID: "payments "},
		{Kind: "team", ID: "secret"},
		{Kind: "", ID: ""},
	} {
		if granted[forbidden] {
			t.Fatalf("hostile entry %v was granted as a membership", forbidden)
		}
	}
	if len(memberships) != len(wantGranted) {
		t.Fatalf("granted %d memberships (%v), want exactly the canonical set", len(memberships), memberships)
	}
}

// TestGroupClaimBoundsRejectHostileInput pins bounded claim processing:
// oversized totals, oversized entries, and excessive counts grant nothing
// beyond the deterministic caps.
func TestGroupClaimBoundsRejectHostileInput(t *testing.T) {
	mapper := auth.ClaimMapper{
		GroupClaim:     "groups",
		MaxEntries:     3,
		MaxEntryLength: 16,
		MaxTotalBytes:  64,
	}

	// Deterministic truncation: sorted input, first three canonical entries.
	memberships := membershipsFrom(t, mapper, `["team:d","team:c","team:b","team:a"]`, "alice")
	if len(memberships) != 3 || memberships[0].ID != "a" || memberships[1].ID != "b" || memberships[2].ID != "c" {
		t.Fatalf("entry-count cap not deterministic: %v", memberships)
	}

	// An entry longer than the cap terminates scanning deterministically:
	// entries sorting before it survive, nothing beyond it is considered.
	long := `["` + strings.Repeat("x", 17) + `","team:a"]`
	if got := membershipsFrom(t, mapper, long, "alice"); len(got) != 1 ||
		got[0] != (domain.OwnerRef{Kind: "team", ID: "a"}) {
		t.Fatalf("oversized-entry truncation not deterministic: %v", got)
	}

	// A non-array or non-string claim grants nothing without failing auth.
	if got := membershipsFrom(t, mapper, `"team:a"`, "alice"); len(got) != 0 {
		t.Fatalf("scalar group claim granted %v", got)
	}
	if got := membershipsFrom(t, mapper, `[1,2]`, "alice"); len(got) != 0 {
		t.Fatalf("non-string array granted %v", got)
	}
}

// TestStaticGrantsMapToTypedOwners pins that static grants produce typed
// memberships directly, independent of any group claim.
func TestStaticGrantsMapToTypedOwners(t *testing.T) {
	mapper := auth.ClaimMapper{
		Grants: auth.StaticGrants{
			Subjects: map[string][]domain.OwnerRef{
				"ci-bot": {{Kind: "team", ID: "release"}},
			},
			Groups: map[string][]domain.OwnerRef{
				"entra-object-987": {{Kind: "team", ID: "platform"}},
			},
		},
	}
	// Subject grant applies even with an empty groups claim.
	var emptyClaims map[string]json.RawMessage
	if got := mapper.MembershipsOf(emptyClaims, "ci-bot"); len(got) != 1 ||
		got[0] != (domain.OwnerRef{Kind: "team", ID: "release"}) {
		t.Fatalf("subject grant = %v", got)
	}
	// Group grant maps the raw IdP identifier onto the typed owner.
	groups := membershipsFrom(t, mapper, `["entra-object-987"]`, "someone")
	if len(groups) != 1 || groups[0] != (domain.OwnerRef{Kind: "team", ID: "platform"}) {
		t.Fatalf("group grant = %v", groups)
	}
	// Unrelated subjects receive nothing from either table.
	if got := mapper.MembershipsOf(emptyClaims, "nobody"); len(got) != 0 {
		t.Fatalf("unrelated subject received %v", got)
	}
}
