// SPDX-License-Identifier: Apache-2.0

package crossplane

import (
	"context"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube"
)

func kubeObjectOf(document map[string]any) *kube.Object { return kube.NewObject(document) }

func validBinding() Binding { return testBinding() }

func TestConfigRejectsInvalidBindings(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"missing identity", func(c *Config) { c.Identity = " " }},
		{"no bindings", func(c *Config) { c.Bindings = nil }},
		{"duplicate resource type", func(c *Config) { c.Bindings = append(c.Bindings, validBinding()) }},
		{"empty capabilities", func(c *Config) { c.Bindings[0].Capabilities = nil }},
		{"unsupported capability", func(c *Config) { c.Bindings[0].Capabilities = []domain.Capability{domain.CapabilityObserve} }},
		{"incomplete target", func(c *Config) { c.Bindings[0].Target.Group = "" }},
		{"invalid plural", func(c *Config) { c.Bindings[0].Plural = "Not-A-Plural" }},
		{"invalid namespace", func(c *Config) { c.Bindings[0].Namespace = "NOT_A_NAMESPACE" }},
		{"unknown naming version", func(c *Config) { c.Bindings[0].NamingVersion = "v2" }},
		{"nil encoder", func(c *Config) { c.Bindings[0].EncodeInput = nil }},
		{"empty readiness rules", func(c *Config) { c.Bindings[0].Readiness = []ConditionRule{} }},
		{"duplicated condition types", func(c *Config) {
			c.Bindings[0].Readiness = []ConditionRule{{Type: "Ready", Required: true}, {Type: "Ready", Required: false}}
		}},
		{"no required conditions", func(c *Config) {
			c.Bindings[0].Readiness = []ConditionRule{{Type: "Ready", Required: false}, {Type: "Synced", Required: false}}
		}},
		{"unregistered current output mapping", func(c *Config) { c.Bindings[0].CurrentOutputMappingRef = "ghost" }},
		{"output mapping without status path", func(c *Config) {
			c.Bindings[0].OutputMappings = []OutputMapping{{Ref: "m1"}}
		}},
		{"self-compatible recovery mapping", func(c *Config) {
			c.Bindings[0].OutputMappings = []OutputMapping{{
				Ref: "m1", StatusPath: []string{"liftr"}, CompatibleSourceMappingRef: "m1",
			}}
			c.Bindings[0].CurrentOutputMappingRef = "m1"
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := Config{Identity: "install", Bindings: []Binding{validBinding()}}
			testCase.mutate(&config)
			if _, _, err := config.resolve(); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestConfigAcceptsValidBindingAndDefaultsReadiness(t *testing.T) {
	config := Config{Identity: "install", Bindings: []Binding{validBinding()}}
	digest, bindings, err := config.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if digest != PlatformDigest("install") || len(bindings) != 1 {
		t.Fatalf("resolved digest=%q bindings=%d", digest, len(bindings))
	}
	binding := bindings[testResourceType]
	if binding.gvr.Group != "platform.liftr.io" || binding.gvr.Resource != "xtestresources" {
		t.Fatalf("gvr = %+v", binding.gvr)
	}
	if len(binding.readiness) != 2 || !binding.readiness[0].Required || !binding.readiness[1].Required {
		t.Fatalf("default readiness rules = %+v", binding.readiness)
	}
	for _, rule := range binding.readiness {
		if rule.Type != "Ready" && rule.Type != "Synced" {
			t.Fatalf("unexpected default rule %q", rule.Type)
		}
	}
}

func TestCapabilitiesAreSortedDeterministically(t *testing.T) {
	f := newFixture(t, nil)
	first := f.provisioner.Capabilities()
	second := f.provisioner.Capabilities()
	if len(first) != 3 || len(second) != 3 {
		t.Fatalf("capabilities = %v / %v", first, second)
	}
	for index := range first {
		if first[index] != second[index] {
			t.Fatalf("capabilities are not deterministic: %v vs %v", first, second)
		}
	}
}

func TestManifestAssemblyRejectsReservedSpecFields(t *testing.T) {
	f := newFixture(t, func(cfg *Config) {
		cfg.Bindings[0].EncodeInput = func(Input) ([]byte, error) {
			return []byte(`{"desired":true,"metadata":{"labels":{"x":"y"}}}`), nil
		}
	})
	submission, err := f.provisioner.Submit(context.Background(), executionRequest(domain.CapabilityCreate, "op-reserved", 1, 1, simpleSpec(t, true)))
	if err != nil {
		t.Fatal(err)
	}
	execution := submission.Observation.Execution
	if execution == nil || execution.State != provisioning.ExecutionStateFailed {
		t.Fatalf("reserved-field spec must fail closed before any write: %+v", submission.Observation)
	}
	if execution.Failure.Reason != "ProgramInputInvalid" {
		t.Fatalf("reason = %s", execution.Failure.Reason)
	}
	if names := f.server.AllNames(f.namespace, f.gvr.Resource); len(names) != 0 {
		t.Fatalf("rejected manifests must never persist objects: %v", names)
	}
}

func TestCorrelationMetadataSeparatesOwnershipFromOperation(t *testing.T) {
	identity := identityMetadata{
		platformDigest: PlatformDigest("install"),
		resourceID:     "resource-1",
		resourceType:   testResourceType,
	}
	document := map[string]any{"metadata": map[string]any{}}
	identity.stamp(document)
	stampOperationCorrelation(document, "op-1", 7)

	object := kubeObjectOf(document)
	if !identity.verify(object) {
		t.Fatal("ownership verification failed on a freshly stamped object")
	}
	if !operationCorrelated(object, "op-1", 7) {
		t.Fatal("operation correlation failed on a freshly stamped object")
	}
	if operationCorrelated(object, "op-2", 7) || operationCorrelated(object, "op-1", 8) {
		t.Fatal("correlation matched the wrong operation or generation")
	}

	// Ownership survives an operation re-stamp; the dimensions are distinct.
	stampOperationCorrelation(document, "op-9", 12)
	if !identity.verify(object) {
		t.Fatal("ownership was disturbed by an operation re-stamp")
	}
	if !operationCorrelated(object, "op-9", 12) {
		t.Fatal("re-stamped correlation was not applied")
	}
}
