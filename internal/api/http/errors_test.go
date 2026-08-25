// SPDX-License-Identifier: Apache-2.0

package httpapi_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	apihttp "github.com/sithea-nou/liftr/internal/api/http"
	"github.com/sithea-nou/liftr/internal/application"
	applicationfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/policy"
	"github.com/sithea-nou/liftr/internal/provisioning"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
)

// faultRunner simulates a storage layer that fails with sensitive internals.
type faultRunner struct{}

func (faultRunner) Within(context.Context, func(application.UnitOfWork) error) error {
	return errors.New("pq: replication slot secret=alpha-tok host=10.0.0.9 failed")
}

func TestPolicyProblemsUseStableOpaqueCodes(t *testing.T) {
	deniedFixture := newFixture(t)
	deniedPolicy, err := policy.Parse([]byte(`{
		"apiVersion":"liftr.dev/admission-policy/v1",
		"rules":[{"id":"private-deny-rule","kind":"capability-deny","resourceType":{"name":"FakeResource","version":"v1"},"capabilities":["create"]}]
	}`), []domain.ResourceTypeRef{provisioningfake.ResourceType()})
	if err != nil {
		t.Fatal(err)
	}
	deniedFixture.service.AdmissionPolicy = deniedPolicy
	denied := deniedFixture.request(t, http.MethodPost, "/v1/resources", map[string]string{"Idempotency-Key": "policy-denied-key"}, policyCreateBody("policy-denied-resource"))
	document := expectProblem(t, denied, http.StatusForbidden, "POLICY_DENIED")
	encoded := fmt.Sprint(document)
	if strings.Contains(encoded, "private-deny-rule") || strings.Contains(encoded, string(deniedPolicy.Revision())) {
		t.Fatalf("policy problem leaked private provenance: %s", encoded)
	}

	quotaFixture := newFixture(t)
	quotaPolicy, err := policy.Parse([]byte(`{
		"apiVersion":"liftr.dev/admission-policy/v1",
		"rules":[{"id":"private-quota-rule","kind":"resource-count-quota","limit":1}]
	}`), []domain.ResourceTypeRef{provisioningfake.ResourceType()})
	if err != nil {
		t.Fatal(err)
	}
	quotaFixture.service.AdmissionPolicy = quotaPolicy
	first := quotaFixture.request(t, http.MethodPost, "/v1/resources", map[string]string{"Idempotency-Key": "quota-first-key"}, policyCreateBody("quota-first-resource"))
	if first.StatusCode != http.StatusCreated {
		t.Fatalf("first quota response status=%d", first.StatusCode)
	}
	quota := quotaFixture.request(t, http.MethodPost, "/v1/resources", map[string]string{"Idempotency-Key": "quota-second-key"}, policyCreateBody("quota-second-resource"))
	document = expectProblem(t, quota, http.StatusConflict, "QUOTA_EXCEEDED")
	encoded = fmt.Sprint(document)
	if strings.Contains(encoded, "private-quota-rule") || strings.Contains(encoded, string(quotaPolicy.Revision())) {
		t.Fatalf("quota problem leaked private provenance: %s", encoded)
	}
}

func policyCreateBody(id string) map[string]any {
	return map[string]any{
		"id":    id,
		"type":  map[string]string{"name": testResourceType, "version": testResourceVersion},
		"owner": map[string]string{"kind": "team", "id": "platform"},
		"spec":  map[string]any{},
	}
}

// TestRawPersistenceFaultsStayOpaque pins adversarial review item: raw
// provider, Go, or persistence errors must never reach Problem.detail.
func TestRawPersistenceFaultsStayOpaque(t *testing.T) {
	typeValue, err := domain.NewResourceType(provisioningfake.ResourceType(), "fake resource",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		t.Fatal(err)
	}
	catalog := applicationfake.Catalog{Types: map[domain.ResourceTypeRef]domain.ResourceType{provisioningfake.ResourceType(): typeValue}}
	ref, err := application.NewProvisionerRef("fault-provider")
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewService(catalog,
		&applicationfake.Selector{Ref: ref},
		&applicationfake.Resolver{Providers: map[application.ProvisionerRef]provisioning.Provisioner{
			ref: provisioningfake.New(provisioningfake.ModeSynchronous),
		}},
		faultRunner{},
		applicationfake.AllowAll{},
	)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{handler: apihttp.NewHandler(apihttp.Deps{Service: service, Auth: newHeaderAuthenticator()}), auth: newHeaderAuthenticator()}

	create := f.request(t, http.MethodPost, "/v1/resources", map[string]string{"Idempotency-Key": "k"}, map[string]any{
		"id":   "r-fault",
		"type": map[string]string{"name": testResourceType, "version": testResourceVersion},
		"owner": map[string]string{
			"kind": "team",
			"id":   "platform",
		},
		"spec": map[string]any{},
	})
	document := expectProblem(t, create, http.StatusInternalServerError, "INTERNAL")
	detail, _ := document["detail"].(string)
	if detail != "an unexpected internal error occurred" {
		t.Fatalf("internal problem detail = %q, want the generic sentence", detail)
	}
	for _, leaked := range []string{"secret", "replication", "10.0.0.9", "pq:"} {
		if strings.Contains(detail, leaked) {
			t.Fatalf("problem detail leaks persistence internals: %q", detail)
		}
	}

	read := f.request(t, http.MethodGet, "/v1/resources/r-fault", nil, nil)
	expectProblem(t, read, http.StatusInternalServerError, "INTERNAL")
}
