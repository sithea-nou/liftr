// SPDX-License-Identifier: Apache-2.0

package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	apihttp "github.com/sithea-nou/liftr/internal/api/http"
	applicationfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/auth"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
)

// failingAuthenticator rejects every credential, pinning the presented-token
// challenge shape.
type failingAuthenticator struct{}

func (failingAuthenticator) Authenticate(_ context.Context, _ string) (identity.Principal, error) {
	return identity.Principal{}, errors.New("invalid credentials")
}

// TestMissingCredentialAnswersUnauthenticated pins the 401 contract and the
// health-endpoint exemption.
func TestMissingCredentialAnswersUnauthenticated(t *testing.T) {
	f := newFixture(t)

	response := f.requestWithoutAuth(t, http.MethodGet, "/v1/resources/anything", nil, nil)
	expectProblem(t, response, http.StatusUnauthorized, "UNAUTHENTICATED")
	if challenge := header(response, "WWW-Authenticate"); challenge != `Bearer realm="liftr"` {
		t.Fatalf("WWW-Authenticate = %q", challenge)
	}

	healthz := f.requestWithoutAuth(t, http.MethodGet, "/healthz", nil, nil)
	expectStatus(t, healthz, http.StatusOK)
	readyz := f.requestWithoutAuth(t, http.MethodGet, "/readyz", nil, nil)
	expectStatus(t, readyz, http.StatusOK)
}

func parseProblemDocument(t *testing.T, response *http.Response) map[string]any {
	t.Helper()
	document := decodeBody(t, response)
	if document["code"] == nil {
		t.Fatalf("not a problem document: %v", document)
	}
	return document
}

// TestPresentedInvalidCredentialUsesInvalidTokenChallenge pins RFC 6750
// challenge behavior and body identity across failure reasons.
func TestPresentedInvalidCredentialUsesInvalidTokenChallenge(t *testing.T) {
	store := applicationfake.NewStore()
	typeValue, err := domain.NewResourceType(provisioningfake.ResourceType(), "fake",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		t.Fatal(err)
	}
	catalog := applicationfake.Catalog{Types: map[domain.ResourceTypeRef]domain.ResourceType{provisioningfake.ResourceType(): typeValue}}
	base := newFixtureWithParts(t, store, catalog)
	failing := &fixture{
		handler:  apihttp.NewHandler(apihttp.Deps{Service: base.service, Auth: failingAuthenticator{}}),
		service:  base.service,
		auth:     newHeaderAuthenticator(),
		catalog:  catalog,
		resolver: base.resolver,
		ref:      base.ref,
		store:    store,
	}

	presented := failing.requestWithoutAuth(t, http.MethodGet, "/v1/resources/anything",
		map[string]string{"Authorization": "Bearer unparseable-jwt"}, nil)
	if presented.StatusCode != http.StatusUnauthorized {
		t.Fatalf("presented invalid credential status = %d", presented.StatusCode)
	}
	if challenge := header(presented, "WWW-Authenticate"); challenge != `Bearer realm="liftr", error="invalid_token"` {
		t.Fatalf("presented credential WWW-Authenticate = %q", challenge)
	}
	missing := failing.requestWithoutAuth(t, http.MethodGet, "/v1/resources/anything", map[string]string{}, nil)

	presentedBody := parseProblemDocument(t, presented)
	missingBody := parseProblemDocument(t, missing)
	for _, document := range []map[string]any{presentedBody, missingBody} {
		if document["code"] != "UNAUTHENTICATED" {
			t.Fatalf("problem code = %v, want UNAUTHENTICATED", document["code"])
		}
	}
	delete(presentedBody, "requestId")
	delete(missingBody, "requestId")
	for key, value := range presentedBody {
		if missingBody[key] != value {
			t.Fatalf("credential failure bodies differ at %q: %v vs %v (failure reasons must stay hidden)", key, value, missingBody[key])
		}
	}
}

// TestForbiddenReadIsIndistinguishableFromAbsence pins the hidden-404 policy:
// every externally visible field of the problem matches a truly unknown ID,
// and no precondition state leaks before authorization.
func TestForbiddenReadIsIndistinguishableFromAbsence(t *testing.T) {
	fixture, authz := newFixtureWithPolicy(t, auth.OwnerAuthorizer{})
	authz.registerMembership("payments-user", domain.OwnerRef{Kind: "team", ID: "payments"})
	create := fixture.request(t, http.MethodPost, "/v1/resources", map[string]string{"Idempotency-Key": "hidden-create"},
		map[string]any{
			"id":    "resource-secretive",
			"type":  map[string]string{"name": testResourceType, "version": testResourceVersion},
			"owner": map[string]string{"kind": "team", "id": "platform"},
			"spec":  map[string]any{"size": int64(11)},
		})
	expectStatus(t, create, http.StatusCreated)

	assertSameHidden404(t, fixture,
		"/v1/resources/resource-secretive", "/v1/resources/no-such-record", "payments-user")

	// A forbidden update must not disclose generation or precondition state,
	// whatever headers accompany it.
	staleGeneration := fixture.requestWithoutAuth(t, http.MethodPut, "/v1/resources/resource-secretive",
		map[string]string{"Authorization": "Bearer payments-user", "If-Liftr-Generation": "99", "Idempotency-Key": "k1"},
		map[string]any{"spec": map[string]any{"size": int64(12)}})
	noHeaders := fixture.requestWithoutAuth(t, http.MethodPut, "/v1/resources/resource-secretive",
		map[string]string{"Authorization": "Bearer payments-user"}, map[string]any{"spec": map[string]any{"size": int64(12)}})
	if staleGeneration.StatusCode != http.StatusNotFound || noHeaders.StatusCode != http.StatusNotFound {
		t.Fatalf("forbidden updates answered %d/%d, want uniform 404", staleGeneration.StatusCode, noHeaders.StatusCode)
	}
	assertSameNotFoundPayload(t, staleGeneration, noHeaders, "RESOURCE_NOT_FOUND")
	if stale := header(staleGeneration, "Liftr-Generation"); stale != "" {
		t.Fatal("forbidden update leaked generation header")
	}

	deleteForbidden := fixture.requestWithoutAuth(t, http.MethodDelete, "/v1/resources/resource-secretive",
		map[string]string{"Authorization": "Bearer payments-user", "If-Liftr-Generation": "1"}, nil)
	document := documentFrom(t, deleteForbidden)
	if deleteForbidden.StatusCode != http.StatusForbidden && deleteForbidden.StatusCode != http.StatusNotFound {
		t.Fatalf("forbidden delete status = %d", deleteForbidden.StatusCode)
	}
	if document["code"] != "RESOURCE_NOT_FOUND" {
		t.Fatalf("forbidden delete code = %v, want hidden RESOURCE_NOT_FOUND", document["code"])
	}

	// Operation reads authorize through the owning Resource with the same
	// hidden shape as a missing Operation.
	view := decodeBody(t, create)
	operationHref, _ := view["latestOperation"].(map[string]any)["href"].(string)
	assertSameHidden404Operation(t, fixture, operationHref, "payments-user")
}

// TestForbiddenCreateAndAuthorizedMemberCreate pins the honest 403 for
// capability denial on creates.
func TestForbiddenCreateAndAuthorizedMemberCreate(t *testing.T) {
	fixture, authz := newFixtureWithPolicy(t, auth.OwnerAuthorizer{})
	authz.registerMembership("payments-user", domain.OwnerRef{Kind: "team", ID: "payments"})

	denied := fixture.requestWithoutAuth(t, http.MethodPost, "/v1/resources",
		map[string]string{"Authorization": "Bearer tester", "Idempotency-Key": "forbidden-owner"},
		map[string]any{
			"id":    "resource-forbidden-owner",
			"type":  map[string]string{"name": testResourceType, "version": testResourceVersion},
			"owner": map[string]string{"kind": "team", "id": "payments"},
			"spec":  map[string]any{"size": int64(13)},
		})
	expectProblem(t, denied, http.StatusForbidden, "FORBIDDEN")

	allowed := fixture.requestWithoutAuth(t, http.MethodPost, "/v1/resources",
		map[string]string{"Authorization": "Bearer payments-user", "Idempotency-Key": "member-owner"},
		map[string]any{
			"id":    "resource-payments",
			"type":  map[string]string{"name": testResourceType, "version": testResourceVersion},
			"owner": map[string]string{"kind": "team", "id": "payments"},
			"spec":  map[string]any{"size": int64(14)},
		})
	expectStatus(t, allowed, http.StatusCreated)
}

// TestOutputsFollowResourceReadAuthorization pins that non-secret outputs
// ride resource:read and are never reachable by non-members.
func TestOutputsFollowResourceReadAuthorization(t *testing.T) {
	catalog := newOutputContractCatalog(t)
	fixture, authz := newFixtureWithCatalogAndPolicy(t, auth.OwnerAuthorizer{}, catalog)
	authz.registerMembership("payments-user", domain.OwnerRef{Kind: "team", ID: "payments"})
	fixture.resolver.Providers[fixture.ref] = &evidenceProvider{}

	create := fixture.request(t, http.MethodPost, "/v1/resources", map[string]string{"Idempotency-Key": "outputs-authz"},
		map[string]any{
			"id":    "resource-with-outputs",
			"type":  map[string]string{"name": testResourceType, "version": testResourceVersion},
			"owner": map[string]string{"kind": "team", "id": "platform"},
			"spec":  map[string]any{"size": int64(15)},
		})
	expectStatus(t, create, http.StatusCreated)
	fixture.drainWorker(t)

	memberView := decodeBody(t, fixture.request(t, http.MethodGet, "/v1/resources/resource-with-outputs", nil, nil))
	if _, present := memberView["outputs"]; !present {
		t.Fatalf("authorized reader sees no outputs: %v", memberView)
	}
	nonMember := fixture.requestWithoutAuth(t, http.MethodGet, "/v1/resources/resource-with-outputs",
		map[string]string{"Authorization": "Bearer payments-user"}, nil)
	expectProblem(t, nonMember, http.StatusNotFound, "RESOURCE_NOT_FOUND")
}

// TestDiscoveryRequiresAuthentication pins that discovery is authenticated.
func TestDiscoveryRequiresAuthentication(t *testing.T) {
	f := newFixture(t)
	unauthenticated := f.requestWithoutAuth(t, http.MethodGet, "/v1/resource-types", nil, nil)
	expectProblem(t, unauthenticated, http.StatusUnauthorized, "UNAUTHENTICATED")
	authenticated := f.request(t, http.MethodGet, "/v1/resource-types", nil, nil)
	expectStatus(t, authenticated, http.StatusOK)
}

// TestCrossPrincipalIdempotencyThroughTransport pins that principal B cannot
// replay principal A's admission by possessing its key.
func TestCrossPrincipalIdempotencyThroughTransport(t *testing.T) {
	fixture, authz := newFixtureWithPolicy(t, auth.OwnerAuthorizer{})
	authz.registerMembership("payments-user", domain.OwnerRef{Kind: "team", ID: "payments"})

	body := func(id string) map[string]any {
		return map[string]any{
			"id":    id,
			"type":  map[string]string{"name": testResourceType, "version": testResourceVersion},
			"owner": map[string]string{"kind": "team", "id": "platform"},
			"spec":  map[string]any{"size": int64(16)},
		}
	}
	first := fixture.request(t, http.MethodPost, "/v1/resources", map[string]string{"Idempotency-Key": "transport-shared-key"}, body("resource-by-tester"))
	expectStatus(t, first, http.StatusCreated)
	firstOperation := decodeBody(t, first)["latestOperation"].(map[string]any)["id"]

	// First: B WITHOUT any membership replays A's key with byte-equivalent
	// content. Authorization precedes replay, so B is denied outright and
	// learns nothing about A's admission.
	unauthorizedReplay := fixture.requestWithoutAuth(t, http.MethodPost, "/v1/resources",
		map[string]string{"Authorization": "Bearer stranger-principal", "Idempotency-Key": "transport-shared-key"},
		body("resource-by-stranger"))
	expectProblem(t, unauthorizedReplay, http.StatusForbidden, "FORBIDDEN")

	// Then: B WITH membership under the owner uses the same key against a
	// fresh resource ID. Scoping yields B's own independent admission.
	authz.registerMembership("other-principal", domain.OwnerRef{Kind: "team", ID: "platform"})
	replayByOther := fixture.requestWithoutAuth(t, http.MethodPost, "/v1/resources",
		map[string]string{"Authorization": "Bearer other-principal", "Idempotency-Key": "transport-shared-key"},
		body("resource-by-other"))
	expectStatus(t, replayByOther, http.StatusCreated)
	otherOperation := decodeBody(t, replayByOther)["latestOperation"].(map[string]any)["id"]
	if header(replayByOther, "Idempotency-Replayed") == "true" {
		t.Fatal("principal B resolved a record from principal A's namespace")
	}
	if firstOperation == otherOperation {
		t.Fatal("two principals shared one idempotency namespace through the transport")
	}

	// A still replays its own original admission unchanged.
	replayByOwner := fixture.request(t, http.MethodPost, "/v1/resources",
		map[string]string{"Idempotency-Key": "transport-shared-key"}, body("resource-by-tester"))
	expectStatus(t, replayByOwner, http.StatusCreated)
	if header(replayByOwner, "Idempotency-Replayed") != "true" {
		t.Fatal("principal A lost its own replay")
	}
}

// assertSameHidden404 pins that two resource paths answer with identical
// problem documents apart from requestId.
func assertSameHidden404(t *testing.T, fixture *fixture, forbiddenPath, unknownPath, token string) {
	t.Helper()
	forbidden := fixture.requestWithoutAuth(t, http.MethodGet, forbiddenPath,
		map[string]string{"Authorization": "Bearer " + token}, nil)
	unknown := fixture.requestWithoutAuth(t, http.MethodGet, unknownPath,
		map[string]string{"Authorization": "Bearer " + token}, nil)
	assertSameNotFoundPayload(t, forbidden, unknown, "RESOURCE_NOT_FOUND")
}

// assertSameHidden404Operation pins operation-read hiding.
func assertSameHidden404Operation(t *testing.T, fixture *fixture, forbiddenPath, token string) {
	t.Helper()
	forbidden := fixture.requestWithoutAuth(t, http.MethodGet, forbiddenPath,
		map[string]string{"Authorization": "Bearer " + token}, nil)
	unknown := fixture.requestWithoutAuth(t, http.MethodGet, "/v1/operations/op-does-not-exist",
		map[string]string{"Authorization": "Bearer " + token}, nil)
	assertSameNotFoundPayload(t, forbidden, unknown, "OPERATION_NOT_FOUND")
}

// assertSameNotFoundPayload pins that two responses carry byte-equivalent
// problem documents apart from requestId.
func assertSameNotFoundPayload(t *testing.T, first, second *http.Response, code string) {
	t.Helper()
	left := documentFrom(t, first)
	right := documentFrom(t, second)
	if left["code"] != code || right["code"] != code {
		t.Fatalf("codes = %v / %v, want %s", left["code"], right["code"], code)
	}
	// requestId and instance are caller- or correlation-specific; every
	// server-state field must be identical for the two responses.
	for _, field := range []string{"requestId", "instance"} {
		delete(left, field)
		delete(right, field)
	}
	if len(left) != len(right) {
		t.Fatalf("problems differ in field count: %v vs %v", left, right)
	}
	for key, value := range left {
		if right[key] != value {
			t.Fatalf("problem field %q differs: %v vs %v (existence must stay hidden)", key, value, right[key])
		}
	}
}

func documentFrom(t *testing.T, response *http.Response) map[string]any {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(rawBody(t, response), &document); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	return document
}

// countingAuthenticator records every Authenticate call so tests can prove
// that oversized credentials never reach token processing.
type countingAuthenticator struct {
	inner apihttp.Authenticator
	calls int
}

func (c *countingAuthenticator) Authenticate(ctx context.Context, credential string) (identity.Principal, error) {
	c.calls++
	return c.inner.Authenticate(ctx, credential)
}

// TestOversizedBearerCredentialIsRefusedBeforeProcessing pins the explicit
// Liftr credential limit: an oversized bearer is answered 401 without any
// authenticator (and therefore JWT or JWKS) work, and the credential itself
// never appears in the response.
func TestOversizedBearerCredentialIsRefusedBeforeProcessing(t *testing.T) {
	base := newFixture(t)
	counter := &countingAuthenticator{inner: base.auth}
	f := &fixture{
		handler:  apihttp.NewHandler(apihttp.Deps{Service: base.service, Auth: counter}),
		service:  base.service,
		store:    base.store,
		resolver: base.resolver,
		catalog:  base.catalog,
		ref:      base.ref,
		auth:     base.auth,
	}

	oversized := "Bearer " + strings.Repeat("A", apihttp.MaxBearerCredentialBytes+1)
	response := f.requestWithoutAuth(t, http.MethodGet, "/v1/resource-types",
		map[string]string{"Authorization": oversized}, nil)
	expectProblem(t, response, http.StatusUnauthorized, "UNAUTHENTICATED")
	if challenge := header(response, "WWW-Authenticate"); challenge != `Bearer realm="liftr", error="invalid_token"` {
		t.Fatalf("oversized credential WWW-Authenticate = %q", challenge)
	}
	if strings.Contains(string(rawBody(t, response)), strings.Repeat("A", 64)) {
		t.Fatal("problem body echoed credential material")
	}
	if counter.calls != 0 {
		t.Fatalf("authenticator invoked %d times for an oversized credential; must be refused before processing", counter.calls)
	}

	// The exact boundary accepts credentials at the limit.
	atLimit := f.requestWithoutAuth(t, http.MethodGet, "/v1/resource-types",
		map[string]string{"Authorization": "Bearer " + strings.Repeat("A", apihttp.MaxBearerCredentialBytes)}, nil)
	if atLimit.StatusCode == http.StatusUnauthorized && header(atLimit, "WWW-Authenticate") != "" &&
		strings.Contains(header(atLimit, "WWW-Authenticate"), "invalid_token") {
		// The fake authenticator accepts any non-empty credential, so an
		// at-limit value must pass the size gate and authenticate.
		t.Fatalf("credential exactly at the size limit was refused: %d", atLimit.StatusCode)
	}
	if counter.calls < 1 {
		t.Fatal("at-limit credential never reached the authenticator")
	}
}
