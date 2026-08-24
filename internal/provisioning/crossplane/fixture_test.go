// SPDX-License-Identifier: Apache-2.0

package crossplane

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube/fakeapi"
)

var (
	testResourceType = domain.ResourceTypeRef{Name: "TestResource", Version: "v1"}
	// testOutputMappingRef is this fixture's immutable output-mapping identity.
	testOutputMappingRef = "xrb-test-v1"
)

func testBinding() Binding {
	return Binding{
		ResourceType:          testResourceType,
		Capabilities:          []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete},
		Target:                GVK{Group: "platform.liftr.io", Version: "v1alpha1", Kind: "XTestResource"},
		Plural:                "xtestresources",
		Namespace:             "liftr-test",
		NamingVersion:         NamingVersionV1,
		EncodeInput:           encodeSimpleSpec,
		Readiness:             DefaultConditionRules(),
		TerminalSyncedReasons: []string{"CompositionMissing"},
		OutputMappings: []OutputMapping{{
			Ref:        testOutputMappingRef,
			StatusPath: []string{"liftr", "outputs"},
		}},
		CurrentOutputMappingRef: testOutputMappingRef,
	}
}

func encodeSimpleSpec(input Input) ([]byte, error) {
	return json.Marshal(map[string]any{
		"desired":    input.Spec.Values()["desired"],
		"capability": string(input.Capability),
	})
}

type fixture struct {
	provisioner *Provisioner
	server      *fakeapi.Server
	baseURL     string
	gvr         kube.GVR
	namespace   string
}

func newFixture(t *testing.T, mutate func(cfg *Config)) *fixture {
	t.Helper()
	server, baseURL := fakeapi.New(t)
	binding := testBinding()
	// The fixture's target API resource is served from the start; tests
	// retire it explicitly to model CRD removal.
	server.RegisterFamily(binding.Target.Group, binding.Target.Version, binding.Plural)
	config := Config{
		Identity:       "m14-tests",
		RequestTimeout: 5 * time.Second,
		Bindings:       []Binding{binding},
	}
	if mutate != nil {
		mutate(&config)
	}
	client, err := kube.NewClient(&kube.RestConfig{Host: baseURL}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	provisioner, err := NewWithClient(config, client)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{
		provisioner: provisioner,
		server:      server,
		baseURL:     baseURL,
		gvr:         kube.GVR{Group: "platform.liftr.io", Version: "v1alpha1", Resource: "xtestresources"},
		namespace:   "liftr-test",
	}
}

func simpleSpec(t *testing.T, value any) domain.ResourceSpec {
	t.Helper()
	spec, err := domain.NewResourceSpec(map[string]any{"desired": value})
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

func executionRequest(capability domain.Capability, operationID string, attempt uint64, generation uint64, spec domain.ResourceSpec) provisioning.ExecutionRequest {
	return provisioning.ExecutionRequest{
		OperationID:      domain.OperationID(operationID),
		AttemptNumber:    attempt,
		ResourceID:       "resource-1",
		ResourceType:     testResourceType,
		Spec:             spec,
		Capability:       capability,
		TargetGeneration: generation,
	}
}

// markReady is the deterministic controller step that simulates a healthy
// reconciliation at the given poll. Conditions carry fresh observedGenerations
// and the composition-published status envelope when requested.
func markReady(fromPoll int) fakeapi.Controller {
	return func(poll int, object *fakeapi.Object) {
		if poll < fromPoll {
			return
		}
		refreshStatusEnvelope(object, "ready-"+string(rune('a'+poll%26)))
		object.Raw["status"] = map[string]any{
			"conditions": []any{
				map[string]any{"type": "Synced", "status": "True", "reason": "ReconciliationSucceeded",
					"observedGeneration": float64(object.Generation()), "lastTransitionTime": timestampFor(poll)},
				map[string]any{"type": "Ready", "status": "True", "reason": "Available",
					"observedGeneration": float64(object.Generation()), "lastTransitionTime": timestampFor(poll)},
			},
		}
	}
}

func refreshStatusEnvelope(object *fakeapi.Object, hostname string) {
	annotations, _ := object.Raw["metadata"].(map[string]any)["annotations"].(map[string]any)
	resourceID, _ := annotations[annotationResourceID].(string)
	generationAnnotation, _ := annotations[annotationTargetGenerationKey].(string)
	var generation uint64
	for _, character := range generationAnnotation {
		if character < '0' || character > '9' {
			continue
		}
		generation = generation*10 + uint64(character-'0')
	}
	object.Raw["status"] = map[string]any{
		"liftr": map[string]any{
			"outputs": map[string]any{
				"version":          float64(outputEnvelopeVersion),
				"mapping":          testOutputMappingRef,
				"resourceId":       resourceID,
				"targetGeneration": float64(generation),
				"values":           map[string]any{"endpoint": hostname, "port": float64(5432)},
			},
		},
	}
}

var baseTime = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

func timestampFor(poll int) string {
	return baseTime.Add(time.Duration(poll) * time.Minute).UTC().Format(time.RFC3339)
}

func conditionsAt(generation uint64, entries ...map[string]any) []any {
	list := make([]any, 0, len(entries))
	for _, entry := range entries {
		if _, hasFreshness := entry["observedGeneration"]; !hasFreshness {
			entry["observedGeneration"] = float64(generation)
		}
		if _, hasTime := entry["lastTransitionTime"]; !hasTime {
			entry["lastTransitionTime"] = timestampFor(9)
		}
		list = append(list, entry)
	}
	return list
}

func syncedTrue() map[string]any {
	return map[string]any{"type": "Synced", "status": "True", "reason": "ReconciliationSucceeded"}
}

func readyTrue() map[string]any {
	return map[string]any{"type": "Ready", "status": "True", "reason": "Available"}
}

func foreignObject() map[string]any {
	return map[string]any{
		"apiVersion": "platform.liftr.io/v1alpha1",
		"kind":       "XTestResource",
		"metadata": map[string]any{
			"labels":      map[string]any{"app.kubernetes.io/managed-by": "someone-else"},
			"annotations": map[string]any{"owner.example.com/team": "not-liftr"},
		},
		"spec": map[string]any{"desired": "foreign-state"},
	}
}

func (f *fixture) submit(t *testing.T, request provisioning.ExecutionRequest) provisioning.ExecutionObservation {
	t.Helper()
	submission, err := f.provisioner.Submit(context.Background(), request)
	if err != nil {
		t.Fatalf("submit returned error: %v", err)
	}
	return submission.Observation
}

func observeExecution(t *testing.T, p *Provisioner, request provisioning.ObservationRequest) provisioning.ExecutionObservation {
	t.Helper()
	observation, err := p.Observe(context.Background(), request)
	if err != nil {
		t.Fatalf("observe failed: %v", err)
	}
	return observation
}
