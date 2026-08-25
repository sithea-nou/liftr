// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/identity"
	"gopkg.in/yaml.v3"
)

const openAPIPath = "../../../docs/openapi/admin/v1/openapi-admin.yaml"

func TestOpenAPIMatchesAdminRoutes(t *testing.T) {
	body, err := os.ReadFile(openAPIPath)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(body, &document); err != nil {
		t.Fatalf("parse OpenAPI: %v", err)
	}
	paths, ok := document["paths"].(map[string]any)
	if !ok {
		t.Fatal("contract has no paths")
	}
	documented := map[string]bool{}
	for path, raw := range paths {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("path %q is not an object", path)
		}
		for method := range item {
			if method == "get" || method == "post" {
				documented[strings.ToUpper(method)+" "+path] = true
			}
		}
	}
	runtime := map[string]bool{}
	for _, route := range apiRoutes(&handler{}) {
		runtime[route.Method+" "+route.Pattern] = true
	}
	for key := range documented {
		if !runtime[key] {
			t.Errorf("documented operation %q is not served", key)
		}
	}
	for key := range runtime {
		if !documented[key] {
			t.Errorf("runtime route %q is undocumented", key)
		}
	}
}

func TestOpenAPIStateIdentityMatchesWireNames(t *testing.T) {
	body, err := os.ReadFile(openAPIPath)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	components := document["components"].(map[string]any)
	schemas := components["schemas"].(map[string]any)
	stateIdentity := schemas["StateIdentity"].(map[string]any)
	properties := stateIdentity["properties"].(map[string]any)
	want := []string{"provisionerRef", "engine", "program", "backend", "stateKey", "lineagePresent", "serial", "digestPrefix", "version"}
	for _, name := range want {
		if _, ok := properties[name]; !ok {
			t.Errorf("StateIdentity schema is missing %q", name)
		}
	}
	for name := range properties {
		if name != "" && name[0] >= 'A' && name[0] <= 'Z' {
			t.Errorf("StateIdentity schema exposes non-wire property %q", name)
		}
	}
}

func TestAdminRouterDoesNotServePublicAPI(t *testing.T) {
	handler := NewHandler(Deps{Auth: anonymousTestAuth{}})
	request := httptest.NewRequest(http.MethodGet, "/v1/resources", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
}

type anonymousTestAuth struct{}

func (anonymousTestAuth) Authenticate(context.Context, string) (identity.Principal, error) {
	return identity.Principal{ID: "test", Kind: identity.KindUser}, nil
}
func (anonymousTestAuth) AllowsAnonymous() bool { return true }
