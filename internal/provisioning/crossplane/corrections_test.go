// SPDX-License-Identifier: Apache-2.0

package crossplane

import (
	"context"
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube/fakeapi"
)

func observeFor(t *testing.T, f *fixture, operationID string, capability domain.Capability, generation uint64) provisioning.ExecutionObservation {
	t.Helper()
	return observeExecution(t, f.provisioner, provisioning.ObservationRequest{
		OperationID:      domain.OperationID(operationID),
		AttemptNumber:    1,
		ResourceID:       "resource-1",
		ResourceType:     testResourceType,
		Spec:             simpleSpec(t, true),
		Capability:       capability,
		TargetGeneration: generation,
	})
}

// Correction 1A: an XR owned by this Resource but stamped with an older
// Operation is physically Present while the current request stays
// uncorrelated. It is never NotFound, and the application's existing
// resubmission machinery may act on the correlation fact.
func TestCorrection1A_StaleOperationAnnotationStaysPresentAndUncorrelated(t *testing.T) {
	f := newFixture(t, nil)
	f.submit(t, executionRequest(domain.CapabilityCreate, "op-old", 1, 1, simpleSpec(t, true)))

	observation := observeFor(t, f, "op-new", domain.CapabilityCreate, 2)
	if observation.Correlation != provisioning.RequestCorrelationNotFound {
		t.Fatalf("request correlation = %s, want NotFound (this Operation never landed)", observation.Correlation)
	}
	if observation.Execution != nil {
		t.Fatalf("uncorrelated request must carry no execution: %+v", observation.Execution)
	}
	if observation.Resource.Presence != provisioning.ResourcePresencePresent {
		t.Fatalf("presence = %s, want Present for a physically existing XR", observation.Resource.Presence)
	}
	if observation.Resource.Readiness == provisioning.ResourceReadinessReady {
		t.Fatal("a stale-generation object must never report Ready")
	}
}

// Correction 1B: a foreign object under the deterministic name fails closed
// with TargetIdentityConflict on every path, reports Presence=Present, and
// can never be adopted or resubmitted onto.
func TestCorrection1B_ForeignObjectFailsClosedNeverNotFound(t *testing.T) {
	name := ObjectName("m14-tests", "liftr-test", testResourceType, "resource-1")

	t.Run("submit create preflight", func(t *testing.T) {
		f := newFixture(t, nil)
		f.server.Put(f.namespace, f.gvr.Resource, name, foreignObject())
		submission, err := f.provisioner.Submit(context.Background(),
			executionRequest(domain.CapabilityCreate, "op-f", 1, 1, simpleSpec(t, true)))
		if err != nil {
			t.Fatal(err)
		}
		requireFound(t, submission.Observation, provisioning.ExecutionStateFailed)
		execution := submission.Observation.Execution
		if execution.Failure.Reason != reasonTargetIdentityConflict {
			t.Fatalf("failure reason = %s, want TargetIdentityConflict", execution.Failure.Reason)
		}
		if submission.Observation.Resource.Presence != provisioning.ResourcePresencePresent {
			t.Fatalf("foreign collision presence = %s, want Present", submission.Observation.Resource.Presence)
		}
		stored, ok := f.server.Get(f.namespace, f.gvr.Resource, name)
		if !ok || stored.Raw["spec"].(map[string]any)["desired"] != "foreign-state" {
			t.Fatal("the foreign object was mutated")
		}
	})

	t.Run("observe execution", func(t *testing.T) {
		f := newFixture(t, nil)
		f.server.Put(f.namespace, f.gvr.Resource, name, foreignObject())
		observation := observeFor(t, f, "op-g", domain.CapabilityUpdate, 3)
		requireFound(t, observation, provisioning.ExecutionStateFailed)
		if observation.Execution.Failure.Reason != reasonTargetIdentityConflict {
			t.Fatalf("reason = %s", observation.Execution.Failure.Reason)
		}
		if observation.Correlation != provisioning.RequestCorrelationFound {
			// Found + Failed prevents resubmission; only conclusive NotFound
			// authorizes another attempt, and a physical object exists here.
			t.Fatalf("correlation = %s, want Found so no resubmission is authorized", observation.Correlation)
		}
	})

	t.Run("delete refuses foreign target", func(t *testing.T) {
		f := newFixture(t, nil)
		f.server.Put(f.namespace, f.gvr.Resource, name, foreignObject())
		submission, err := f.provisioner.Submit(context.Background(),
			executionRequest(domain.CapabilityDelete, "op-h", 1, 1, simpleSpec(t, true)))
		if err != nil {
			t.Fatal(err)
		}
		requireFound(t, submission.Observation, provisioning.ExecutionStateFailed)
		if submission.Observation.Execution.Failure.Reason != reasonTargetIdentityConflict {
			t.Fatalf("reason = %s", submission.Observation.Execution.Failure.Reason)
		}
		if _, ok := f.server.Get(f.namespace, f.gvr.Resource, name); !ok {
			t.Fatal("the foreign object was deleted")
		}
	})
}

// Correction 1C: conclusive managed absence exists only when the object is
// genuinely absent — never as an encoding of stale or foreign metadata.
func TestCorrection1C_AbsenceOnlyFromGenuinePhysicalAbsence(t *testing.T) {
	f := newFixture(t, nil)

	// Genuine absence before any delete ran.
	submission, err := f.provisioner.Submit(context.Background(),
		executionRequest(domain.CapabilityDelete, "op-i", 1, 1, simpleSpec(t, true)))
	if err != nil {
		t.Fatal(err)
	}
	if submission.Observation.Correlation != provisioning.RequestCorrelationNotFound ||
		submission.Observation.Execution == nil ||
		submission.Observation.Execution.State != provisioning.ExecutionStateFailed ||
		submission.Observation.Execution.Failure.Kind != provisioning.FailureNotFound {
		t.Fatalf("genuine-absence delete = %+v", submission.Observation)
	}
	if submission.Observation.Resource.Presence != provisioning.ResourcePresenceNotFound {
		t.Fatalf("absence facts = %+v", submission.Observation.Resource)
	}

	// With an object present, even one carrying foreign operation stamps,
	// no path reports NotFound.
	f.submit(t, executionRequest(domain.CapabilityCreate, "op-j", 1, 1, simpleSpec(t, true)))
	observation := observeFor(t, f, "op-k-unrelated", domain.CapabilityDelete, 9)
	requireFound(t, observation, provisioning.ExecutionStateRunning)
	if observation.Resource.Presence != provisioning.ResourcePresencePresent {
		t.Fatalf("present-object delete observe = %+v", observation.Resource)
	}
}

// Correction 1D: delete completion is purely physical. A stale operation
// annotation during deletion never becomes Deleted or absent.
func TestCorrection1D_DeleteIgnoresOperationCorrelation(t *testing.T) {
	f := newFixture(t, nil)
	f.submit(t, executionRequest(domain.CapabilityCreate, "op-l", 1, 1, simpleSpec(t, true)))
	name := ObjectName("m14-tests", f.namespace, testResourceType, "resource-1")
	f.server.Mutate(f.namespace, f.gvr.Resource, name, func(object *fakeapi.Object) { object.SetFinalizer("held") })

	f.submit(t, executionRequest(domain.CapabilityDelete, "op-m", 1, 1, simpleSpec(t, true)))
	observation := observeFor(t, f, "op-ancient", domain.CapabilityDelete, 4)
	requireFound(t, observation, provisioning.ExecutionStateRunning)
	if observation.Resource.Presence != provisioning.ResourcePresencePresent {
		t.Fatalf("terminating presence = %s, want Present", observation.Resource.Presence)
	}
	if observation.Resource.Readiness != provisioning.ResourceReadinessNotReady {
		t.Fatalf("terminating readiness = %s, want NotReady", observation.Resource.Readiness)
	}
}

// Correction 2 adversarial #1: a verified object replaced by a foreign one
// before DELETE survives untouched; Liftr fails closed via the UID
// precondition instead of deleting the replacement.
func TestCorrection2_UIDPreconditionBlocksForeignReplacementOnDelete(t *testing.T) {
	f := newFixture(t, nil)
	f.submit(t, executionRequest(domain.CapabilityCreate, "op-n", 1, 1, simpleSpec(t, true)))
	name := ObjectName("m14-tests", f.namespace, testResourceType, "resource-1")

	f.server.ArmBeforeNextWrite(func(s *fakeapi.Server) { s.PutLocked(f.namespace, f.gvr.Resource, name, foreignObject()) })
	submission, err := f.provisioner.Submit(context.Background(),
		executionRequest(domain.CapabilityDelete, "op-o", 1, 1, simpleSpec(t, true)))
	if err != nil {
		t.Fatal(err)
	}
	requireFound(t, submission.Observation, provisioning.ExecutionStateFailed)
	if submission.Observation.Execution.Failure.Reason != reasonTargetIdentityConflict {
		t.Fatalf("reason = %s", submission.Observation.Execution.Failure.Reason)
	}
	replacement, ok := f.server.Get(f.namespace, f.gvr.Resource, name)
	if !ok {
		t.Fatal("the replacement object was deleted")
	}
	if replacement.Raw["spec"].(map[string]any)["desired"] != "foreign-state" {
		t.Fatalf("replacement spec mutated: %+v", replacement.Raw["spec"])
	}
}

// Correction 2 adversarial #2: a verified object replaced before update is
// neither mutated nor adopted; the resourceVersion precondition rejects the
// blind write and re-verification detects the foreign replacement.
func TestCorrection2_UpdateNeverAdoptsReplacement(t *testing.T) {
	f := newFixture(t, nil)
	f.submit(t, executionRequest(domain.CapabilityCreate, "op-p", 1, 1, simpleSpec(t, true)))
	name := ObjectName("m14-tests", f.namespace, testResourceType, "resource-1")

	f.server.ArmBeforeNextWrite(func(s *fakeapi.Server) { s.PutLocked(f.namespace, f.gvr.Resource, name, foreignObject()) })
	submission, err := f.provisioner.Submit(context.Background(),
		executionRequest(domain.CapabilityUpdate, "op-q", 1, 2, simpleSpec(t, false)))
	if err != nil {
		t.Fatal(err)
	}
	requireFound(t, submission.Observation, provisioning.ExecutionStateFailed)
	if submission.Observation.Execution.Failure.Reason != reasonTargetIdentityConflict {
		t.Fatalf("reason = %s", submission.Observation.Execution.Failure.Reason)
	}
	replacement, _ := f.server.Get(f.namespace, f.gvr.Resource, name)
	if replacement.Raw["spec"].(map[string]any)["desired"] != "foreign-state" {
		t.Fatalf("replacement was mutated or adopted: %+v", replacement.Raw["spec"])
	}
	metadata := replacement.Raw["metadata"].(map[string]any)
	if _, hasLiftrLabels := metadata["labels"].(map[string]any)["app.kubernetes.io/managed-by"].(string); hasLiftrLabels {
		if labels := metadata["labels"].(map[string]any)["app.kubernetes.io/managed-by"]; labels == managedByValue {
			t.Fatal("ownership labels were overwritten onto the foreign object")
		}
	}
}

// Correction 2 adversarial #3: a benign concurrent status write changes the
// resourceVersion between verification and apply. The precondition rejects
// the write, the adapter re-reads and re-verifies, and retries with fresh
// evidence — it never forces an unconditional write by name.
func TestCorrection2_ResourceVersionConflictReevaluatesThenSucceeds(t *testing.T) {
	f := newFixture(t, nil)
	f.submit(t, executionRequest(domain.CapabilityCreate, "op-r", 1, 1, simpleSpec(t, true)))
	name := ObjectName("m14-tests", f.namespace, testResourceType, "resource-1")

	// Simulate controller churn exactly once, right before the apply.
	f.server.ArmBeforeNextWrite(func(s *fakeapi.Server) {
		s.MutateLocked(f.namespace, f.gvr.Resource, name, func(object *fakeapi.Object) {
			status, _ := object.Raw["status"].(map[string]any)
			if status == nil {
				status = map[string]any{}
			}
			status["observedReconcile"] = "tick"
			object.Raw["status"] = status
			object.ResourceVersion++
		})
	})
	submission, err := f.provisioner.Submit(context.Background(),
		executionRequest(domain.CapabilityUpdate, "op-s", 1, 2, simpleSpec(t, false)))
	if err != nil {
		t.Fatal(err)
	}
	requireFound(t, submission.Observation, provisioning.ExecutionStateAccepted)
}

// Correction 2 adversarial #4: without interference, the same-identity
// conditional write succeeds.
func TestCorrection2_NormalSameIdentityUpdateSucceeds(t *testing.T) {
	f := newFixture(t, nil)
	f.submit(t, executionRequest(domain.CapabilityCreate, "op-t", 1, 1, simpleSpec(t, true)))
	submission, err := f.provisioner.Submit(context.Background(),
		executionRequest(domain.CapabilityUpdate, "op-u", 1, 2, simpleSpec(t, false)))
	if err != nil {
		t.Fatal(err)
	}
	requireFound(t, submission.Observation, provisioning.ExecutionStateAccepted)
}

// Correction 3 matrix: Crossplane freshness AND Liftr generation freshness
// are both required for Ready.
func TestCorrection3_ReadinessRequiresCrossplaneAndLiftrFreshness(t *testing.T) {
	cases := []struct {
		name       string
		controller fakeapi.Controller
		requestGen uint64
		wantReady  bool
	}{
		{
			name: "fresh conditions and current liftr generation are ready",
			controller: func(poll int, object *fakeapi.Object) {
				object.Raw["status"] = map[string]any{"conditions": conditionsAt(object.Generation(), syncedTrue(), readyTrue())}
			},
			requestGen: 1, wantReady: true,
		},
		{
			name: "stale liftr generation annotation blocks ready",
			controller: func(poll int, object *fakeapi.Object) {
				object.Raw["status"] = map[string]any{"conditions": conditionsAt(object.Generation(), syncedTrue(), readyTrue())}
			},
			requestGen: 2, wantReady: false,
		},
		{
			name: "stale condition observedGeneration blocks ready",
			controller: func(poll int, object *fakeapi.Object) {
				object.Raw["status"] = map[string]any{"conditions": conditionsAt(object.Generation()-1, syncedTrue(), readyTrue())}
			},
			requestGen: 1, wantReady: false,
		},
		{
			name: "missing freshness field blocks ready",
			controller: func(poll int, object *fakeapi.Object) {
				synced, ready := syncedTrue(), readyTrue()
				delete(synced, "observedGeneration")
				delete(ready, "observedGeneration")
				object.Raw["status"] = map[string]any{"conditions": []any{synced, ready}}
			},
			requestGen: 1, wantReady: false,
		},
		{
			name: "only Ready true blocks ready",
			controller: func(poll int, object *fakeapi.Object) {
				object.Raw["status"] = map[string]any{"conditions": conditionsAt(object.Generation(), readyTrue())}
			},
			requestGen: 1, wantReady: false,
		},
		{
			name: "only Synced true blocks ready",
			controller: func(poll int, object *fakeapi.Object) {
				object.Raw["status"] = map[string]any{"conditions": conditionsAt(object.Generation(), syncedTrue())}
			},
			requestGen: 1, wantReady: false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			f := newFixture(t, nil)
			f.submit(t, executionRequest(domain.CapabilityCreate, "op-v", 1, 1, simpleSpec(t, true)))
			f.server.SetController(testCase.controller)
			observation := observeExecution(t, f.provisioner, provisioning.ObservationRequest{
				OperationID: "op-v", AttemptNumber: 1, ResourceID: "resource-1",
				ResourceType: testResourceType, Capability: domain.CapabilityCreate,
				Spec: simpleSpec(t, true), TargetGeneration: testCase.requestGen,
				OutputMappingRef: testOutputMappingRef,
			})
			gotReady := observation.Execution != nil && observation.Execution.State == provisioning.ExecutionStateSucceeded &&
				observation.Resource.Readiness == provisioning.ResourceReadinessReady
			if gotReady != testCase.wantReady {
				t.Fatalf("ready=%v observation=%+v", gotReady, observation)
			}
			if observation.Resource.Drift != provisioning.ResourceDriftUnknown {
				t.Fatal("drift must stay Unknown in every readiness scenario")
			}
		})
	}
}

// Correction 3F: passive observation with a stale Liftr generation never
// advances readiness even when the XR itself reports healthy conditions.
func TestCorrection3F_PassiveObservationStaleGenerationNeverReady(t *testing.T) {
	f := newFixture(t, nil)
	f.submit(t, executionRequest(domain.CapabilityCreate, "op-w", 1, 1, simpleSpec(t, true)))
	f.server.SetController(func(poll int, object *fakeapi.Object) {
		object.Raw["status"] = map[string]any{"conditions": conditionsAt(object.Generation(), syncedTrue(), readyTrue())}
	})
	// Resource generation advanced to 5 while the XR still carries 1.
	observation := observeExecution(t, f.provisioner, provisioning.ObservationRequest{
		ResourceID: "resource-1", ResourceType: testResourceType,
		Spec: simpleSpec(t, true), TargetGeneration: 5,
	})
	if observation.Resource.Presence != provisioning.ResourcePresencePresent {
		t.Fatalf("presence = %s, want Present", observation.Resource.Presence)
	}
	if observation.Resource.Readiness == provisioning.ResourceReadinessReady {
		t.Fatal("stale-generation health must never mark the resource Ready through passive observation")
	}
	if observation.Resource.Drift != provisioning.ResourceDriftUnknown {
		t.Fatal("drift must remain Unknown")
	}
}

// Terminal reconciliation reasons are binding-configured and produce curated
// failures; raw condition messages never leak.
func TestTerminalReasonProducesCuratedFailureWithoutMessageLeakage(t *testing.T) {
	f := newFixture(t, nil)
	f.submit(t, executionRequest(domain.CapabilityCreate, "op-x", 1, 1, simpleSpec(t, true)))
	f.server.SetController(func(poll int, object *fakeapi.Object) {
		object.Raw["status"] = map[string]any{"conditions": []any{
			map[string]any{"type": "Synced", "status": "False", "reason": "CompositionMissing",
				"message": "SECRET composition detail /var/run/whatever", "observedGeneration": float64(object.Generation()),
				"lastTransitionTime": timestampFor(3)},
			map[string]any{"type": "Ready", "status": "True", "reason": "Available",
				"observedGeneration": float64(object.Generation()), "lastTransitionTime": timestampFor(3)},
		}}
	})
	observation := observeFor(t, f, "op-x", domain.CapabilityCreate, 1)
	requireFound(t, observation, provisioning.ExecutionStateFailed)
	failure := observation.Execution.Failure
	if failure.Reason != reasonReconciliationFailed {
		t.Fatalf("curated reason = %s", failure.Reason)
	}
	if strings.Contains(failure.Message+failure.Reason, "SECRET") || strings.Contains(failure.Message, "/var/run") {
		t.Fatalf("raw condition message leaked: %+v", failure)
	}
	if observation.Resource.Presence != provisioning.ResourcePresencePresent {
		t.Fatal("terminal reconciliation failure must keep physical presence honest")
	}
}

// RBAC denial normalizes to a safe curated failure on submit and a retryable
// observation error on Observe, with zero raw text leakage (#26).
func TestRBACDenialNormalizesSafely(t *testing.T) {
	f := newFixture(t, nil)
	f.server.FailNextGets(&kube.APIError{Code: 403, Reason: "Forbidden", Message: "User \"svc\" cannot get resource in namespace"}, 1)
	_, err := f.provisioner.Observe(context.Background(), provisioning.ObservationRequest{
		OperationID: "op-rbac", AttemptNumber: 1, ResourceID: "resource-1",
		ResourceType: testResourceType, Capability: domain.CapabilityCreate,
		Spec: simpleSpec(t, true), TargetGeneration: 1,
	})
	var observationErr provisioning.ObservationError
	if !errorsAs(err, &observationErr) {
		t.Fatalf("expected ObservationError, got %v", err)
	}
	if observationErr.Failure.Reason != reasonAccessDenied {
		t.Fatalf("reason = %s, want AccessDenied", observationErr.Failure.Reason)
	}
	if strings.Contains(observationErr.Failure.Message+observationErr.Failure.Reason, `"svc"`) {
		t.Fatal("raw RBAC message leaked")
	}

	f2 := newFixture(t, nil)
	f2.server.FailNextCreates(&kube.APIError{Code: 403, Reason: "Forbidden", Message: "forbidden"}, 1, false)
	submission, submitErr := f2.provisioner.Submit(context.Background(),
		executionRequest(domain.CapabilityCreate, "op-y", 1, 1, simpleSpec(t, true)))
	if submitErr != nil {
		t.Fatal(submitErr)
	}
	if submission.Observation.Correlation != provisioning.RequestCorrelationNotFound ||
		submission.Observation.Execution == nil ||
		submission.Observation.Execution.Failure.Reason != reasonAccessDenied {
		t.Fatalf("conclusive RBAC rejection = %+v", submission.Observation)
	}
}

// CRD absence surfaces conclusively as Unsupported on create (#27).
func TestCRDAbsentNormalizesConclusively(t *testing.T) {
	// A missing CRD manifests as a 404 on the write itself, which is
	// conclusively classifiable because nothing was persisted.
	f := newFixture(t, nil)
	f.server.FailNextCreates(&kube.APIError{Code: 404, Reason: "NotFound", Message: "the server could not find the requested resource"}, 1, false)
	submission, err := f.provisioner.Submit(context.Background(),
		executionRequest(domain.CapabilityCreate, "op-z", 1, 1, simpleSpec(t, true)))
	if err != nil {
		t.Fatal(err)
	}
	if submission.Observation.Correlation != provisioning.RequestCorrelationNotFound ||
		submission.Observation.Execution == nil ||
		submission.Observation.Execution.Failure.Reason != reasonTargetKindUnregistered {
		t.Fatalf("CRD-absent create = %+v", submission.Observation)
	}
}

// Retry semantics: an M13-style explicit retry of a failed create targets
// the same logical identity and reasserts the same XR — never a duplicate.
func TestRetryReassertsSameLogicalXR(t *testing.T) {
	f := newFixture(t, nil)
	first := f.submit(t, executionRequest(domain.CapabilityCreate, "retry-source", 1, 1, simpleSpec(t, true)))
	requireFound(t, first, provisioning.ExecutionStateAccepted)
	// A retried Operation carries a brand-new OperationID but identical
	// logical identity.
	second := f.submit(t, executionRequest(domain.CapabilityCreate, "retry-child", 1, 1, simpleSpec(t, true)))
	requireFound(t, second, provisioning.ExecutionStateAccepted)
	names := f.server.AllNames(f.namespace, f.gvr.Resource)
	if len(names) != 1 {
		t.Fatalf("retry produced %d XRs, want exactly one", len(names))
	}
}

// Adversarial #9: a UID learned from confirmed evidence must never be
// silently reconciled away. When the physical object under the deterministic
// name no longer matches the UID recorded on the persisted handle — even
// one carrying Liftr's own ownership marks — the observation fails closed
// instead of adopting the replacement.
func TestCorrection2_HandleUIDMismatchAtObserveFailsClosed(t *testing.T) {
	f := newFixture(t, nil)
	f.submit(t, executionRequest(domain.CapabilityCreate, "op-uid", 1, 1, simpleSpec(t, true)))
	name := ObjectName("m14-tests", f.namespace, testResourceType, "resource-1")

	// First sighting: the returned handle now records the physical UID.
	firstSighting := observeFor(t, f, "op-uid", domain.CapabilityCreate, 1)
	if firstSighting.Execution == nil || firstSighting.Execution.Handle == nil {
		t.Fatal("observation carried no handle")
	}
	payload, ok := decodeHandle(firstSighting.Execution.Handle)
	if !ok || payload.UID == "" {
		t.Fatalf("first confirmed sighting did not record a UID: %+v", firstSighting.Execution.Handle)
	}

	// Delete and recreate under the same name with identical Liftr ownership
	// marks: only the physical identity differs.
	stored, _ := f.server.Get(f.namespace, f.gvr.Resource, name)
	replacement := map[string]any{
		"apiVersion": "platform.liftr.io/v1alpha1",
		"kind":       "XTestResource",
		"metadata": map[string]any{
			"labels":      stored.Raw["metadata"].(map[string]any)["labels"],
			"annotations": stored.Raw["metadata"].(map[string]any)["annotations"],
		},
		"spec": stored.Raw["spec"],
	}
	f.server.Put(f.namespace, f.gvr.Resource, name, replacement)

	observation := observeExecution(t, f.provisioner, provisioning.ObservationRequest{
		OperationID: "op-uid", AttemptNumber: 1,
		ResourceID: "resource-1", ResourceType: testResourceType,
		Capability: domain.CapabilityCreate, Spec: simpleSpec(t, true),
		TargetGeneration: 1,
		Handle:           firstSighting.Execution.Handle,
	})
	requireFound(t, observation, provisioning.ExecutionStateFailed)
	if reason := observation.Execution.Failure.Reason; reason != reasonTargetIdentityConflict && reason != reasonTargetIdentityChanged {
		t.Fatalf("failure reason = %s, want an identity conflict", reason)
	}
	if observation.Resource.Presence != provisioning.ResourcePresencePresent {
		t.Fatalf("presence = %s, want Present for the physical replacement", observation.Resource.Presence)
	}
}
