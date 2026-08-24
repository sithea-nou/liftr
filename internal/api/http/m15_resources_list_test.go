// SPDX-License-Identifier: Apache-2.0

package httpapi_test

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	applicationfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/auth"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
)

// seedInventory installs one retained Resource directly in the store so
// inventory semantics are observable without driving the worker.
func seedInventory(t *testing.T, f *fixture, id, ownerKind, ownerID string, state domain.ResourceState, createdAt time.Time) {
	t.Helper()
	resource, err := domain.NewResource(domain.ResourceID(id),
		domain.ResourceTypeRef{Name: testResourceType, Version: testResourceVersion},
		domain.OwnerRef{Kind: ownerKind, ID: ownerID},
		mustSpec(t, map[string]any{"size": int64(5)}), createdAt)
	if err != nil {
		t.Fatal(err)
	}
	status, err := domain.NewResourceStatus(resource.ID(), 1, state, nil, createdAt.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	err = f.store.Within(context.Background(), func(tx application.UnitOfWork) error {
		return tx.Resources().CreateResource(context.Background(), application.ResourceRecord{
			Resource: resource, Status: status, ProvisionerRef: mustProvisionerRef(t), Version: 1,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func listResources(t *testing.T, f *fixture, query string) (*http.Response, map[string]any) {
	t.Helper()
	response := f.request(t, http.MethodGet, "/v1/resources"+query, nil, nil)
	return response, decodeBody(t, response)
}

func listedIDsFrom(document map[string]any) []string {
	items, _ := document["items"].([]any)
	ids := make([]string, 0, len(items))
	for _, item := range items {
		entry, _ := item.(map[string]any)
		id, _ := entry["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

func sortedKeys(document map[string]any) []string {
	keys := make([]string, 0, len(document))
	for key := range document {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestResourceListHappyPathShapeAndHeaders(t *testing.T) {
	f := newFixture(t)
	base := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	seedInventory(t, f, "orders-db", "team", "platform", domain.ResourceStateReady, base)

	response := f.request(t, http.MethodGet, "/v1/resources", nil, nil)
	expectStatus(t, response, http.StatusOK)
	if got := header(response, "Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := header(response, "Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q", got)
	}
	if header(response, "X-Request-ID") == "" {
		t.Fatal("missing X-Request-ID")
	}
	if header(response, "Liftr-Generation") != "" {
		t.Fatal("a collection represents no single Resource and must not carry Liftr-Generation")
	}
	document := decodeBody(t, response)
	if _, hasTotal := document["total"]; hasTotal {
		t.Fatal("inventory must not report a total count")
	}
	items, ok := document["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("items = %#v", document["items"])
	}
	summary := items[0].(map[string]any)

	// Correction J pin: the summary key set is exactly the approved surface;
	// spec, conditions, outputs, and execution metadata cannot appear.
	want := []string{"createdAt", "generation", "id", "owner", "status", "type", "updatedAt"}
	if strings.Join(sortedKeys(summary), ",") != strings.Join(want, ",") {
		t.Fatalf("summary keys = %v, want %v", sortedKeys(summary), want)
	}
	summaryStatus := summary["status"].(map[string]any)
	wantStatus := []string{"observedGeneration", "state", "updatedAt"}
	if strings.Join(sortedKeys(summaryStatus), ",") != strings.Join(wantStatus, ",") {
		t.Fatalf("summary status keys = %v, want %v", sortedKeys(summaryStatus), wantStatus)
	}
	for _, banned := range []string{"spec", "outputs", "conditions", "provisionerRef", "sequence", "resourceSeq"} {
		if _, present := summary[banned]; present {
			t.Fatalf("summary leaked %q", banned)
		}
	}
	if summary["latestOperation"] == nil {
		t.Log("directly seeded Resource has no Operation yet; latestOperation correctly omitted")
	}
}

func TestResourceListRequiresAuthenticationAndAuthorizesOncePerRequest(t *testing.T) {
	recorder := &applicationfake.RecordingAuthorizer{}
	f, _ := newFixtureWithPolicy(t, recorder)
	seedInventory(t, f, "counted", "team", "platform", domain.ResourceStateReady, time.Now())

	unauthenticated := f.requestWithoutAuth(t, http.MethodGet, "/v1/resources", nil, nil)
	expectProblem(t, unauthenticated, http.StatusUnauthorized, "UNAUTHENTICATED")
	if recorder.ListInvocations != 0 {
		t.Fatalf("unauthenticated request reached the authorizer %d times", recorder.ListInvocations)
	}

	first, body := listResources(t, f, "")
	if first.StatusCode != http.StatusOK || len(listedIDsFrom(body)) != 1 {
		t.Fatalf("authorized list = %d %v", first.StatusCode, body)
	}
	if recorder.ListInvocations != 1 {
		t.Fatalf("list authorizations after first page = %d, want exactly 1", recorder.ListInvocations)
	}
	second, _ := listResources(t, f, "?limit=50")
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second request = %d", second.StatusCode)
	}
	if recorder.ListInvocations != 2 {
		t.Fatalf("list authorizations after second request = %d, want exactly one per request", recorder.ListInvocations)
	}
}

func TestResourceListDenialIsForbiddenAndEmptyVisibilityIsEmptyCollection(t *testing.T) {
	f, _ := newFixtureWithPolicy(t, applicationfake.DenyAll{})
	denied := f.request(t, http.MethodGet, "/v1/resources", nil, nil)
	document := expectProblem(t, denied, http.StatusForbidden, "FORBIDDEN")
	for _, banned := range []string{"owners", "count", "items"} {
		if _, present := document[banned]; present {
			t.Fatalf("forbidden listing leaked %q", banned)
		}
	}

	f2, _ := newFixtureWithPolicy(t, scopedPolicy{scope: identity.ResourceVisibility{}})
	seedInventory(t, f2, "invisible", "team", "other", domain.ResourceStateReady, time.Now())
	response, body := listResources(t, f2, "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("empty visibility answered %d, want an authorized empty collection", response.StatusCode)
	}
	items, ok := body["items"].([]any)
	if !ok || len(items) != 0 {
		t.Fatalf("empty visibility must render an empty array, got %#v", body["items"])
	}
}

// scopedPolicy authorizes every action but returns one fixed listing scope,
// letting transport tests exercise membership-derived visibility without
// importing policy packages.
type scopedPolicy struct{ scope identity.ResourceVisibility }

func (p scopedPolicy) Authorize(context.Context, identity.Principal, identity.Action, identity.ResourceTarget) error {
	return nil
}

func (p scopedPolicy) AuthorizeResourceList(context.Context, identity.Principal) (identity.ResourceVisibility, error) {
	return p.scope, nil
}

func TestResourceListMembershipScopeThroughOwnerPolicy(t *testing.T) {
	f, authn := newFixtureWithPolicy(t, auth.OwnerAuthorizer{})
	authn.registerMembership("member", domain.OwnerRef{Kind: "team", ID: "security"})
	base := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	seedInventory(t, f, "mine", "team", "platform", domain.ResourceStateReady, base)
	seedInventory(t, f, "theirs", "team", "security", domain.ResourceStateReady, base.Add(time.Second))

	response := f.send(t, http.MethodGet, "/v1/resources", map[string]string{"Authorization": "Bearer member"}, nil)
	document := decodeBody(t, response)
	ids := listedIDsFrom(document)
	if len(ids) != 1 || ids[0] != "theirs" {
		t.Fatalf("membership scope returned %v, want only the owned Resource", ids)
	}

	// The default fixture principal holds team/platform: same rows, other side.
	defaulted := f.request(t, http.MethodGet, "/v1/resources", nil, nil)
	ids = listedIDsFrom(decodeBody(t, defaulted))
	if len(ids) != 1 || ids[0] != "mine" {
		t.Fatalf("owner-membership scope returned %v, want only team/platform rows", ids)
	}
}

func TestResourceListOwnerFilterNarrowsAndNeverGrants(t *testing.T) {
	f, _ := newFixtureWithPolicy(t, scopedPolicy{scope: identity.ResourceVisibility{
		Owners: []domain.OwnerRef{{Kind: "team", ID: "platform"}},
	}})
	base := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	seedInventory(t, f, "visible-a", "team", "platform", domain.ResourceStateReady, base)
	seedInventory(t, f, "hidden-b", "team", "payments", domain.ResourceStateReady, base.Add(time.Second))

	_, document := listResources(t, f, "?ownerKind=team&ownerId=platform")
	ids := listedIDsFrom(document)
	if len(ids) != 1 || ids[0] != "visible-a" {
		t.Fatalf("in-scope owner filter returned unexpected rows: %v", ids)
	}

	response, document := listResources(t, f, "?ownerKind=team&ownerId=security")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("out-of-scope owner filter answered %d, want an empty 200 collection", response.StatusCode)
	}
	if len(listedIDsFrom(document)) != 0 {
		t.Fatalf("out-of-scope owner filter disclosed %v", listedIDsFrom(document))
	}
	half := f.request(t, http.MethodGet, "/v1/resources?ownerKind=team", nil, nil)
	expectProblem(t, half, http.StatusBadRequest, "INVALID_ARGUMENT")
}

func TestResourceListDeletedSemanticsAndTypeStateFilters(t *testing.T) {
	f, _ := newFixtureWithPolicy(t, scopedPolicy{scope: identity.ResourceVisibility{AllOwners: true}})
	base := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	f.seedDeletedRecord(t, "gone-tombstone")
	seedInventory(t, f, "live-one", "team", "platform", domain.ResourceStateReady, base)

	_, body := listResources(t, f, "")
	ids := listedIDsFrom(body)
	if len(ids) != 1 || ids[0] != "live-one" {
		t.Fatalf("default view must exclude Deleted tombstones: %v", ids)
	}

	_, body = listResources(t, f, "?includeDeleted=true")
	if len(listedIDsFrom(body)) != 2 {
		t.Fatalf("includeDeleted must expose tombstones under the same authorization: %v", body)
	}

	stateReady := "?state=Ready"
	_, body = listResources(t, f, stateReady)
	if len(listedIDsFrom(body)) != 1 {
		t.Fatalf("state=Ready returned %v", listedIDsFrom(body))
	}
	_, body = listResources(t, f, "?state=Deleted&includeDeleted=true")
	ids = listedIDsFrom(body)
	if len(ids) != 1 || ids[0] != "gone-tombstone" {
		t.Fatalf("state=Deleted with includeDeleted=true returned %v", ids)
	}
	_, body = listResources(t, f, "?type=NoSuchType&version=v9")
	if len(listedIDsFrom(body)) != 0 {
		t.Fatalf("unknown well-formed type must yield an empty collection: %v", listedIDsFrom(body))
	}
	_, body = listResources(t, f, "?type="+testResourceType)
	if len(listedIDsFrom(body)) != 1 {
		t.Fatalf("known type filter returned %v", listedIDsFrom(body))
	}
}

func TestResourceListCursorPossessionGrantsNoAuthorization(t *testing.T) {
	// A well-formed-looking cursor plus a policy that denies enumeration:
	// denial must win before any cursor semantics are evaluated.
	f, _ := newFixtureWithPolicy(t, applicationfake.DenyAll{})
	denied := f.request(t, http.MethodGet, "/v1/resources?cursor=r1_AAAAAQ", nil, nil)
	expectProblem(t, denied, http.StatusForbidden, "FORBIDDEN")

	// An unauthenticated caller with any cursor is 401, never validated.
	unauthenticated := f.requestWithoutAuth(t, http.MethodGet, "/v1/resources?cursor=r1_AAAAAQ", nil, nil)
	expectProblem(t, unauthenticated, http.StatusUnauthorized, "UNAUTHENTICATED")
}

func TestResourceListStrictQueryValidation(t *testing.T) {
	f := newFixture(t)
	cases := []struct{ query, reason string }{
		{"?nonsense=1", "unknown parameter"},
		{"?limit=1&limit=2", "duplicate parameter"},
		{"?limit=0", "limit below minimum"},
		{"?limit=101", "limit above maximum"},
		{"?limit=abc", "non-numeric limit"},
		{"?state=Exploded", "unknown state"},
		{"?state=Deleted", "deleted without includeDeleted"},
		{"?includeDeleted=yes", "non-boolean includeDeleted"},
		{"?ownerKind=", "empty ownerKind"},
		{"?version=v1", "version without type"},
		{"?cursor=", "empty cursor"},
	}
	for _, testCase := range cases {
		response := f.request(t, http.MethodGet, "/v1/resources"+testCase.query, nil, nil)
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s (%s): status = %d, want 400", testCase.query, testCase.reason, response.StatusCode)
		}
		document := decodeBody(t, response)
		if document["code"] != "INVALID_ARGUMENT" {
			t.Fatalf("%s (%s): code = %v", testCase.query, testCase.reason, document["code"])
		}
	}
	for _, query := range []string{"?limit=1", "?limit=100"} {
		response := f.request(t, http.MethodGet, "/v1/resources"+query, nil, nil)
		expectStatus(t, response, http.StatusOK)
	}
}

func urlQueryEscape(value string) string {
	replacer := strings.NewReplacer("+", "%2B", "=", "%3D", "/", "%2F")
	return replacer.Replace(value)
}

func TestResourceListPaginationAcrossPagesAndCursorBinding(t *testing.T) {
	f, _ := newFixtureWithPolicy(t, scopedPolicy{scope: identity.ResourceVisibility{AllOwners: true}})
	base := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	for i := range 3 {
		seedInventory(t, f, "page-"+string(rune('a'+i)), "team", "platform", domain.ResourceStateReady, base.Add(time.Duration(i)*time.Second))
	}

	_, body := listResources(t, f, "?limit=2")
	ids := listedIDsFrom(body)
	if len(ids) != 2 || ids[0] != "page-c" {
		t.Fatalf("first page = %v", ids)
	}
	cursor, _ := body["nextCursor"].(string)
	if cursor == "" {
		t.Fatal("first page omitted nextCursor despite more results")
	}

	_, body = listResources(t, f, "?limit=2&cursor="+urlQueryEscape(cursor))
	all := append(ids, listedIDsFrom(body)...)
	if strings.Join(all, ",") != "page-c,page-b,page-a" {
		t.Fatalf("full traversal = %v", all)
	}
	if next, present := body["nextCursor"]; present && next != "" {
		t.Fatalf("final page advertises another cursor: %q", next)
	}

	// A cursor is bound to its filter tuple: replaying it under different
	// filters is invalid rather than a silently different traversal.
	mismatched := f.request(t, http.MethodGet, "/v1/resources?limit=2&type=Other&cursor="+urlQueryEscape(cursor), nil, nil)
	expectProblem(t, mismatched, http.StatusBadRequest, "INVALID_ARGUMENT")

	// Operation-history cursors are a different namespace; garbage stays
	// garbage. Both normalize to one INVALID_ARGUMENT form.
	for _, broken := range []string{"c1_AAAAAQ", "r1_nonsense", strings.Repeat("r1_x", 64)} {
		response := f.request(t, http.MethodGet, "/v1/resources?limit=2&cursor="+urlQueryEscape(broken), nil, nil)
		expectProblem(t, response, http.StatusBadRequest, "INVALID_ARGUMENT")
	}
}
