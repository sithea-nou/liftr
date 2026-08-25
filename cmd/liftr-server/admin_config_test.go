// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/server"
)

func TestComposeAdminAuthConfig(t *testing.T) {
	api := server.AuthConfig{Issuer: "https://issuer.example", Audience: "liftr-api", Algorithms: []string{"RS256"}, KindClaim: "kind"}

	t.Run("disabled", func(t *testing.T) {
		config, err := composeAdminAuthConfig(api, false, false)
		if err != nil || config != nil {
			t.Fatalf("config = %#v, error = %v", config, err)
		}
	})

	t.Run("secured defaults", func(t *testing.T) {
		t.Setenv("LIFTR_ADMIN_AUTH_AUDIENCE", "liftr-operator")
		t.Setenv("LIFTR_ADMIN_AUTH_GRANTS_FILE", "/grants.json")
		config, err := composeAdminAuthConfig(api, false, true)
		if err != nil {
			t.Fatal(err)
		}
		if config.Issuer != api.Issuer || config.Audience != "liftr-operator" || config.KindClaim != api.KindClaim || config.GrantsFile != "/grants.json" {
			t.Fatalf("unexpected config: %#v", config)
		}
	})

	t.Run("same audience rejected", func(t *testing.T) {
		t.Setenv("LIFTR_ADMIN_AUTH_AUDIENCE", api.Audience)
		_, err := composeAdminAuthConfig(api, false, true)
		if err == nil || !strings.Contains(err.Error(), "must differ") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("insecure", func(t *testing.T) {
		config, err := composeAdminAuthConfig(server.AuthConfig{}, true, true)
		if err != nil || config == nil {
			t.Fatalf("config = %#v, error = %v", config, err)
		}
	})
}
