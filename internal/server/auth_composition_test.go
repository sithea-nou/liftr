// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/application"
	appfake "github.com/sithea-nou/liftr/internal/application/fake"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	provisioningfake "github.com/sithea-nou/liftr/internal/provisioning/fake"
	"github.com/sithea-nou/liftr/internal/server"
)

func compositionBase(t *testing.T) server.Config {
	t.Helper()
	ref := application.ProvisionerRef("auth-composition-provider")
	return server.Config{
		Transactions:          appfake.NewStore(),
		Catalog:               authCompositionCatalog(t),
		Provisioners:          map[application.ProvisionerRef]provisioning.Provisioner{ref: provisioningfake.New(provisioningfake.ModeSynchronous)},
		DefaultProvisionerRef: ref,
	}
}

// TestFullRuntimeWithoutAuthenticationConfigurationFailsStartup pins the
// secure default: missing auth configuration is a startup failure, never a
// silent fallback to open access.
func TestFullRuntimeWithoutAuthenticationConfigurationFailsStartup(t *testing.T) {
	if _, err := server.Compose(compositionBase(t)); err == nil {
		t.Fatal("full runtime composed without authentication configuration")
	}
}

// TestConflictingAuthModesAreRejected pins that insecure mode cannot be
// combined with OIDC configuration.
func TestConflictingAuthModesAreRejected(t *testing.T) {
	config := compositionBase(t)
	config.InsecureAuth = true
	config.Auth = &server.AuthConfig{Issuer: "https://idp.example", Audience: "liftr"}
	if _, err := server.Compose(config); err == nil {
		t.Fatal("insecure mode combined with OIDC configuration was accepted")
	}
}

// TestInsecureModeRequiresExplicitOptInAndWarns pins the development-only
// composition behavior end to end.
func TestInsecureModeRequiresExplicitOptInAndWarns(t *testing.T) {
	config := compositionBase(t)
	buffer := &bytes.Buffer{}
	config.Logger = slog.New(slog.NewJSONHandler(buffer, nil))
	config.InsecureAuth = true

	composed, err := server.Compose(config)
	if err != nil {
		t.Fatalf("explicit insecure composition failed: %v", err)
	}
	if !strings.Contains(buffer.String(), "INSECURE MODE") {
		t.Fatalf("insecure composition logged no prominent warning: %s", buffer.String())
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	composed.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("healthz status = %d in insecure mode", recorder.Code)
	}

	// The fixed development principal authenticates any bearer value and the
	// allow-all policy admits work; discovery stays reachable.
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/v1/resource-types", nil)
	request.Header.Set("Authorization", "Bearer anything")
	composed.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("discovery status = %d in insecure mode: %s", recorder.Code, recorder.Body.String())
	}

}

func authCompositionCatalog(t *testing.T) application.ResourceTypeCatalog {
	t.Helper()
	resourceType, err := domain.NewResourceType(provisioningfake.ResourceType(), "auth composition type",
		[]domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete})
	if err != nil {
		t.Fatal(err)
	}
	return appfake.Catalog{Types: map[domain.ResourceTypeRef]domain.ResourceType{provisioningfake.ResourceType(): resourceType}}
}
