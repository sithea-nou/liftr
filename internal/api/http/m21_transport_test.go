// SPDX-License-Identifier: Apache-2.0

package httpapi_test

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sithea-nou/liftr/internal/application"
	applicationfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
)

var createKeyCounter atomic.Int64

// m21ReferenceCatalog returns a catalog whose single fake type declares one
// optional self-targeting reference slot.
func m21ReferenceCatalog(t *testing.T) application.ResourceTypeCatalog {
	t.Helper()
	typeValue, err := domain.NewResourceType(provisioningfake.ResourceType(), "Fake resource for transport tests",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		t.Fatal(err)
	}
	slots, err := resourcecontract.NewReferenceContract([]resourcecontract.ReferenceSlot{{
		Name:               "dependency",
		AllowedTargetTypes: []domain.ResourceTypeRef{provisioningfake.ResourceType()},
		MinItems:           0,
		MaxItems:           1,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return applicationfake.Catalog{
		Types:      map[domain.ResourceTypeRef]domain.ResourceType{provisioningfake.ResourceType(): typeValue},
		References: map[domain.ResourceTypeRef]*resourcecontract.ReferenceContract{provisioningfake.ResourceType(): &slots},
	}
}

func m21CreateBody(id, target string) map[string]any {
	body := map[string]any{
		"id":    id,
		"type":  map[string]string{"name": provisioningfake.ResourceType().Name, "version": provisioningfake.ResourceType().Version},
		"owner": map[string]string{"kind": "team", "id": "platform"},
		"spec":  map[string]any{"size": 3},
	}
	if target != "" {
		body["references"] = map[string]any{"dependency": []string{target}}
	}
	return body
}

func m21CreateHeaders() map[string]string {
	return map[string]string{"Idempotency-Key": fmt.Sprintf("key-%d", createKeyCounter.Add(1))}
}

func TestM21TransportCreateCarriesAndReturnsReferences(t *testing.T) {
	fixture := newFixtureWithCatalog(t, m21ReferenceCatalog(t))
	fixture.request(t, http.MethodPost, "/v1/resources", m21CreateHeaders(), m21CreateBody("m21-t1", ""))
	response := fixture.request(t, http.MethodPost, "/v1/resources", m21CreateHeaders(), m21CreateBody("m21-s1", "m21-t1"))
	expectStatus(t, response, http.StatusCreated)
	body := decodeBody(t, response)
	references, ok := body["references"].(map[string]any)
	if !ok {
		t.Fatalf("create response missing references: %v", body)
	}
	targets, ok := references["dependency"].([]any)
	if !ok || len(targets) != 1 || targets[0] != "m21-t1" {
		t.Fatalf("references = %v", references)
	}

	read := fixture.request(t, http.MethodGet, "/v1/resources/m21-s1", nil, nil)
	expectStatus(t, read, http.StatusOK)
	readBody := decodeBody(t, read)
	if _, ok := readBody["references"]; !ok {
		t.Fatalf("GET dropped references: %v", readBody)
	}
}

func TestM21TransportUnknownSlotIsReferenceInvalid(t *testing.T) {
	fixture := newFixtureWithCatalog(t, m21ReferenceCatalog(t))
	fixture.request(t, http.MethodPost, "/v1/resources", m21CreateHeaders(), m21CreateBody("m21-t2", ""))
	body := m21CreateBody("m21-bad", "m21-t2")
	body["references"] = map[string]any{"nosuch": []string{"m21-t2"}}
	response := fixture.request(t, http.MethodPost, "/v1/resources", m21CreateHeaders(), body)
	problem := expectProblem(t, response, http.StatusUnprocessableEntity, "REFERENCE_INVALID")
	violations, ok := problem["violations"].([]any)
	if !ok || len(violations) == 0 {
		t.Fatalf("REFERENCE_INVALID without violations: %v", problem)
	}
	detail := fmt.Sprintf("%v", violations[0])
	if !strings.Contains(detail, "unknown-slot") {
		t.Fatalf("violation keyword missing: %v", violations)
	}
}

func TestM21TransportHiddenTargetRendersGenericRefusal(t *testing.T) {
	fixture := newFixtureWithCatalog(t, m21ReferenceCatalog(t))
	for _, id := range []string{"missing-x", "present-x"} {
		_ = id
	}
	// A nonexistent and a cross-owner target must render the SAME code.
	first := expectProblem(t,
		fixture.request(t, http.MethodPost, "/v1/resources", m21CreateHeaders(), m21CreateBody("m21-hidden-missing", "nope")),
		http.StatusUnprocessableEntity, "REFERENCE_INVALID")

	crossOwner := m21CreateBody("cross-owner-target", "")
	crossOwner["owner"] = map[string]string{"kind": "team", "id": "other"}
	fixture.request(t, http.MethodPost, "/v1/resources", m21CreateHeaders(), crossOwner)

	second := expectProblem(t,
		fixture.request(t, http.MethodPost, "/v1/resources", m21CreateHeaders(), m21CreateBody("m21-hidden-cross", "cross-owner-target")),
		http.StatusUnprocessableEntity, "REFERENCE_INVALID")

	firstDetail, _ := first["violations"].([]any)[0].(map[string]any)["message"].(string)
	secondDetail, _ := second["violations"].([]any)[0].(map[string]any)["message"].(string)
	if firstDetail != secondDetail {
		t.Fatalf("hidden refusals differ:\n%q\n%q", firstDetail, secondDetail)
	}
}

func TestM21TransportUpdateAbsentReferencesPreserve(t *testing.T) {
	fixture := newFixtureWithCatalog(t, m21ReferenceCatalog(t))
	fixture.request(t, http.MethodPost, "/v1/resources", m21CreateHeaders(), m21CreateBody("m21-keep", ""))
	fixture.request(t, http.MethodPost, "/v1/resources", m21CreateHeaders(), m21CreateBody("m21-holder", "m21-keep"))
	fixture.drainWorker(t)

	update := fixture.request(t, http.MethodPut, "/v1/resources/m21-holder", map[string]string{
		"Idempotency-Key":     "key-absent-update",
		"If-Liftr-Generation": "1",
	}, map[string]any{"spec": map[string]any{"size": 9}})
	expectStatus(t, update, http.StatusAccepted)
	body := decodeBody(t, update)
	references, ok := body["references"].(map[string]any)
	if !ok {
		t.Fatalf("absent-reference update did not preserve references: %v", body)
	}
	targets, _ := references["dependency"].([]any)
	if len(targets) != 1 || targets[0] != "m21-keep" {
		t.Fatalf("preserved references = %v", references)
	}
	fixture.drainWorker(t)
	clear := fixture.request(t, http.MethodPut, "/v1/resources/m21-holder", map[string]string{
		"Idempotency-Key":     "key-clear-update",
		"If-Liftr-Generation": "2",
	}, map[string]any{"spec": map[string]any{"size": 10}, "references": map[string]any{}})
	expectStatus(t, clear, http.StatusAccepted)
	cleared := decodeBody(t, clear)
	if _, present := cleared["references"]; present {
		t.Fatalf("explicit empty replacement still shows references: %v", cleared)
	}
}

func TestM21TransportResourceInUseProblem(t *testing.T) {
	fixture := newFixtureWithCatalog(t, m21ReferenceCatalog(t))
	fixture.request(t, http.MethodPost, "/v1/resources", m21CreateHeaders(), m21CreateBody("m21-used", ""))
	fixture.request(t, http.MethodPost, "/v1/resources", m21CreateHeaders(), m21CreateBody("m21-user", "m21-used"))

	response := fixture.request(t, http.MethodDelete, "/v1/resources/m21-used", map[string]string{
		"Idempotency-Key":     "key-del-inuse",
		"If-Liftr-Generation": "1",
	}, nil)
	problem := expectProblem(t, response, http.StatusConflict, "RESOURCE_IN_USE")
	if _, leaks := problem["dependents"]; leaks {
		t.Fatal("RESOURCE_IN_USE leaked dependent information")
	}
	detail := fmt.Sprintf("%v", problem["detail"])
	if strings.Contains(detail, "m21-user") {
		t.Fatal("RESOURCE_IN_USE detail disclosed the dependent ID")
	}
}
