// SPDX-License-Identifier: Apache-2.0

package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	apihttp "github.com/sithea-nou/liftr/internal/api/http"
	"github.com/sithea-nou/liftr/internal/application"
	applicationfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
)

// faultRunner simulates a storage layer that fails with sensitive internals.
type faultRunner struct{}

func (faultRunner) Within(context.Context, func(application.UnitOfWork) error) error {
	return errors.New("pq: replication slot secret=alpha-tok host=10.0.0.9 failed")
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
