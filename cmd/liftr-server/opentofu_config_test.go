// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning/opentofu"
	"github.com/sithea-nou/liftr/internal/resourcetypes"
	"github.com/sithea-nou/liftr/internal/resourcetypes/postgresqldatabase"
)

func TestLoadOpenTofuConfigFileIsStrictAndBounded(t *testing.T) {
	catalog := openTofuTestCatalog(t)
	valid := marshalOpenTofuConfig(t, validOpenTofuFileConfig())
	tests := []struct {
		name string
		raw  []byte
	}{
		{name: "valid", raw: valid},
		{name: "unknown field", raw: append([]byte(`{"terraformVersion":"1.12.6",`), valid[1:]...)},
		{name: "duplicate field", raw: append([]byte(`{"registrations":[],`), valid[1:]...)},
		{name: "trailing document", raw: append(append([]byte(nil), valid...), []byte(` {}`)...)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeOpenTofuConfig(t, test.raw)
			_, err := loadOpenTofuConfigFile(context.Background(), path, catalog)
			if test.name == "valid" && err != nil {
				t.Fatalf("valid config rejected: %v", err)
			}
			if test.name != "valid" && err == nil {
				t.Fatal("invalid config was accepted")
			}
		})
	}

	oversized := make([]byte, maxOpenTofuConfigBytes+1)
	for i := range oversized {
		oversized[i] = ' '
	}
	if _, err := loadOpenTofuConfigFile(context.Background(), writeOpenTofuConfig(t, oversized), catalog); err == nil {
		t.Fatal("oversized config was accepted")
	}
}

func TestLoadOpenTofuConfigFileRejectsUnsafeOrUnsupportedRegistration(t *testing.T) {
	catalog := openTofuTestCatalog(t)
	tests := []struct {
		name   string
		mutate func(*openTofuFileConfig)
	}{
		{name: "local backend", mutate: func(config *openTofuFileConfig) { config.Backend.Type = "local" }},
		{name: "insecure backend", mutate: func(config *openTofuFileConfig) { config.Backend.StateURL = "http://state.example.test/state" }},
		{name: "unsupported resource type", mutate: func(config *openTofuFileConfig) {
			config.Program.ResourceType = domain.ResourceTypeRef{Name: "Unknown", Version: "v1"}
		}},
		{name: "unsupported capability", mutate: func(config *openTofuFileConfig) {
			config.Program.Capabilities = append(config.Program.Capabilities, domain.CapabilityObserve)
		}},
		{name: "unsupported output mapping", mutate: func(config *openTofuFileConfig) {
			config.Program.OutputMappings[0].Fields = map[string]string{"hostname": "endpoint", "password": "password"}
		}},
		{name: "incomplete output mapping", mutate: func(config *openTofuFileConfig) {
			config.Program.OutputMappings[0].Fields = map[string]string{"hostname": "endpoint"}
		}},
		{name: "program OpenTofu control environment", mutate: func(config *openTofuFileConfig) {
			config.Program.RequiredEnvironment = []string{"TF_ENCRYPTION"}
		}},
		{name: "backend address environment", mutate: func(config *openTofuFileConfig) {
			config.Backend.RequiredEnvironment = []string{"TF_HTTP_ADDRESS"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := validOpenTofuFileConfig()
			test.mutate(&config)
			path := writeOpenTofuConfig(t, marshalOpenTofuConfig(t, config))
			if _, err := loadOpenTofuConfigFile(context.Background(), path, catalog); err == nil {
				t.Fatal("unsafe or unsupported registration was accepted")
			}
		})
	}
}

func TestOpenTofuConfigRetainsHistoricalRegistrationWhileRoutingCurrent(t *testing.T) {
	current := validOpenTofuFileConfig()
	historical := validOpenTofuFileConfig()
	historical.ProvisionerRef = "liftr-opentofu-pg-v2-2025"
	historical.Identity = "platform-production-2025"
	historical.Program.Ref = "postgres-v2-program-2025"
	historical.Backend.Ref = "state-service-2025"
	set := openTofuConfigSet{
		Registrations: []openTofuFileConfig{historical, current},
		Routes:        []openTofuRouteConfig{{ResourceType: current.Program.ResourceType, ProvisionerRef: current.ProvisionerRef}},
	}
	path := writeOpenTofuConfig(t, marshalOpenTofuConfigSet(t, set))
	loaded, err := loadOpenTofuConfigFile(context.Background(), path, openTofuTestCatalog(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Registrations) != 2 || len(loaded.Routes) != 1 || loaded.Routes[0].ProvisionerRef != current.ProvisionerRef {
		t.Fatalf("loaded registrations=%d routes=%+v", len(loaded.Registrations), loaded.Routes)
	}
}

func TestGenericOpenTofuInputAndEnvironmentStayPrivateAndDefensive(t *testing.T) {
	spec, err := domain.NewResourceSpec(map[string]any{
		"integer": int64(42),
		"decimal": float64(1.25),
		"nested":  map[string]any{"enabled": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := genericOpenTofuInputEncoder(opentofu.Input{Spec: spec, DesiredPresent: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != 2 || encoded["desired_present"] != false {
		t.Fatalf("encoded input = %#v", encoded)
	}
	values, ok := encoded["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec type = %T", encoded["spec"])
	}
	if _, ok := values["integer"].(int64); !ok {
		t.Fatalf("integer representation changed to %T", values["integer"])
	}
	if _, ok := values["decimal"].(float64); !ok {
		t.Fatalf("decimal representation changed to %T", values["decimal"])
	}
	values["integer"] = int64(7)
	if spec.Values()["integer"] != int64(42) {
		t.Fatal("input encoder exposed mutable ResourceSpec state")
	}

	t.Setenv("OTF_DECLARED", "credential")
	t.Setenv("OTF_AMBIENT", "must-not-leak")
	provided, err := openTofuEnvironmentProvider([]string{"OTF_DECLARED"})(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(provided) != 1 || provided["OTF_DECLARED"] != "credential" {
		t.Fatalf("provided environment names = %v", mapKeys(provided))
	}
	if _, leaked := provided["OTF_AMBIENT"]; leaked {
		t.Fatal("ambient environment leaked into OpenTofu")
	}
}

func validOpenTofuFileConfig() openTofuFileConfig {
	return openTofuFileConfig{
		ProvisionerRef: "liftr-opentofu-pg-v2", Identity: "platform-production",
		Executable: openTofuExecutableConfig{Path: "/opt/liftr/bin/tofu", SHA256: strings.Repeat("a", 64)},
		WorkRoot:   "/var/lib/liftr/opentofu/work", QuarantineRoot: "/var/lib/liftr/opentofu/quarantine",
		LockTimeout: "30s", StateKeyVersion: opentofu.StateKeyVersionV1,
		Program: openTofuProgramConfig{
			Ref: "postgres-v2-program-2026-08", ResourceType: postgresqldatabase.V2TypeRef(),
			Capabilities: []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete},
			SourceDir:    "/opt/liftr/programs/postgres-v2", SourceDigest: strings.Repeat("b", 64), BuiltInOnly: true,
			RequiredEnvironment: []string{"ARM_CLIENT_ID"}, ControlMarkerAddress: "terraform_data.liftr_control",
			ManagedWorkloadAddresses: []string{"terraform_data.database"},
			OutputMappings: []openTofuOutputMappingFileConfig{{
				Ref: "postgres-v2-outputs-2026-08", EnvelopeName: "liftr_outputs",
				Fields: map[string]string{"hostname": "endpoint", "port": "port"},
			}},
			CurrentOutputMappingRef: "postgres-v2-outputs-2026-08",
		},
		Backend: openTofuBackendConfig{
			Type: "http", Ref: "state-service-2026-08",
			StateURL: "https://state.example.test/state", LockURL: "https://state.example.test/lock", UnlockURL: "https://state.example.test/unlock",
			RequiredEnvironment: []string{"TF_HTTP_USERNAME", "TF_HTTP_PASSWORD"},
		},
	}
}

func openTofuTestCatalog(t *testing.T) *resourcetypes.Registry {
	t.Helper()
	v1, err := postgresqldatabase.Contract()
	if err != nil {
		t.Fatal(err)
	}
	v2, err := postgresqldatabase.ContractV2()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := resourcetypes.NewRegistry(v1, v2)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func marshalOpenTofuConfig(t *testing.T, config openTofuFileConfig) []byte {
	t.Helper()
	return marshalOpenTofuConfigSet(t, openTofuConfigSet{
		Registrations: []openTofuFileConfig{config},
		Routes:        []openTofuRouteConfig{{ResourceType: config.Program.ResourceType, ProvisionerRef: config.ProvisionerRef}},
	})
}

func marshalOpenTofuConfigSet(t *testing.T, config openTofuConfigSet) []byte {
	t.Helper()
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func writeOpenTofuConfig(t *testing.T, raw []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opentofu.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
