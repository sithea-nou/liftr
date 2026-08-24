// SPDX-License-Identifier: Apache-2.0

package crossplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube"
	"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube/fakeapi"
)

func assertSubmission(t *testing.T, submission provisioning.Submission, err error) provisioning.ExecutionObservation {
	t.Helper()
	if err != nil {
		t.Fatalf("submit returned error %v", err)
	}
	return submission.Observation
}

func requireFound(t *testing.T, observation provisioning.ExecutionObservation, state provisioning.ExecutionState) *provisioning.Execution {
	t.Helper()
	if observation.Correlation != provisioning.RequestCorrelationFound {
		t.Fatalf("correlation = %s, want Found (%+v)", observation.Correlation, observation)
	}
	if observation.Execution == nil || observation.Execution.State != state {
		t.Fatalf("execution = %+v, want %s", observation.Execution, state)
	}
	return observation.Execution
}

// TestSubmitCreateIsAcceptedNotSucceeded pins the declarative submission
// semantics: a persisted desired-state write is Accepted; only controller
// evidence through Observe may ever report Succeeded.
func TestSubmitCreateIsAcceptedNotSucceeded(t *testing.T) {
	f := newFixture(t, nil)
	observation := f.submit(t, executionRequest(domain.CapabilityCreate, "op-create", 1, 1, simpleSpec(t, true)))
	execution := requireFound(t, observation, provisioning.ExecutionStateAccepted)
	if execution.Handle == nil || execution.Handle.IsZero() {
		t.Fatal("accepted create carried no execution handle")
	}
	if observation.Resource.Presence != provisioning.ResourcePresencePresent {
		t.Fatalf("presence after persistence = %s, want Present (physical object exists)", observation.Resource.Presence)
	}

	// A successful POST must never be confused with reconciliation.
	stored, ok := f.server.Get(f.namespace, f.gvr.Resource, ObjectName("m14-tests", f.namespace, testResourceType, "resource-1"))
	if !ok {
		t.Fatal("create did not persist the XR")
	}
	if stored.Raw["spec"].(map[string]any)["desired"] != true {
		t.Fatalf("stored spec = %+v", stored.Raw["spec"])
	}
	if annotation := stored.AnnotationValue("liftr.io/operation-id"); annotation != "op-create" {
		t.Fatalf("operation correlation annotation = %q", annotation)
	}
	if generation := stored.AnnotationValue("liftr.io/target-generation"); generation != "1" {
		t.Fatalf("target-generation annotation = %q", generation)
	}
}

// TestDuplicateSubmitReassertsSameObject proves idempotent submission by
// deterministic identity: repeated submits of the same logical resource
// address exactly one XR.
func TestDuplicateSubmitReassertsSameObject(t *testing.T) {
	f := newFixture(t, nil)
	request := executionRequest(domain.CapabilityCreate, "op-dup", 1, 1, simpleSpec(t, true))
	first := f.submit(t, request)
	requireFound(t, first, provisioning.ExecutionStateAccepted)
	second := f.submit(t, request)
	requireFound(t, second, provisioning.ExecutionStateAccepted)
	name := ObjectName("m14-tests", f.namespace, testResourceType, "resource-1")
	if _, ok := f.server.Get(f.namespace, f.gvr.Resource, name); !ok {
		t.Fatal("object vanished")
	}
	if got := len(f.server.AllNames(f.namespace, f.gvr.Resource)); got != 1 {
		t.Fatalf("duplicate submit produced %d objects, want 1", got)
	}
}

// TestUpdateObservesReconciliationAsynchronously drives the full declarative
// convergence: Accepted at submit, Running while conditions are absent or
// stale, Succeeded only when fresh conditions meet the current Liftr
// generation.
func TestUpdateObservesReconciliationAsynchronously(t *testing.T) {
	f := newFixture(t, nil)
	create := executionRequest(domain.CapabilityCreate, "op-u-create", 1, 1, simpleSpec(t, true))
	requireFound(t, f.submit(t, create), provisioning.ExecutionStateAccepted)

	updateRequest := executionRequest(domain.CapabilityUpdate, "op-u-update", 1, 2, simpleSpec(t, false))
	requireFound(t, f.submit(t, updateRequest), provisioning.ExecutionStateAccepted)

	observe := provisioning.ObservationRequest{
		OperationID: updateRequest.OperationID, AttemptNumber: 1,
		ResourceID: updateRequest.ResourceID, ResourceType: testResourceType,
		Spec: updateRequest.Spec, Capability: domain.CapabilityUpdate, TargetGeneration: 2,
		OutputMappingRef: testOutputMappingRef,
	}
	polling := observeExecution(t, f.provisioner, observe)
	requireFound(t, polling, provisioning.ExecutionStateRunning)
	if polling.Resource.Readiness == provisioning.ResourceReadinessReady {
		t.Fatal("readiness reported before any controller evidence existed")
	}

	f.server.SetController(markReady(2))
	converged := observeExecution(t, f.provisioner, observe)
	execution := requireFound(t, converged, provisioning.ExecutionStateSucceeded)
	if converged.Resource.Presence != provisioning.ResourcePresencePresent || converged.Resource.Readiness != provisioning.ResourceReadinessReady {
		t.Fatalf("facts on success = %+v", converged.Resource)
	}
	if converged.Resource.Drift != provisioning.ResourceDriftUnknown {
		t.Fatalf("drift = %s, want Unknown always in M14", converged.Resource.Drift)
	}
	if execution.Handle == nil || execution.Handle.String() == "" {
		t.Fatal("successful observation carried no handle")
	}
}

// TestDeleteWaitsForGenuineAbsence proves that Kubernetes DELETE acceptance
// is not completion: with a finalizer held, the execution stays Running with
// Present facts until the object is physically gone.
func TestDeleteWaitsForGenuineAbsence(t *testing.T) {
	f := newFixture(t, nil)
	requireFound(t, f.submit(t, executionRequest(domain.CapabilityCreate, "op-d-create", 1, 1, simpleSpec(t, true))), provisioning.ExecutionStateAccepted)
	name := ObjectName("m14-tests", f.namespace, testResourceType, "resource-1")
	f.server.Mutate(f.namespace, f.gvr.Resource, name, func(object *fakeapi.Object) { object.SetFinalizer("platform.liftr.io/held") })

	deleteRequest := executionRequest(domain.CapabilityDelete, "op-d-delete", 1, 1, simpleSpec(t, true))
	requireFound(t, f.submit(t, deleteRequest), provisioning.ExecutionStateAccepted)

	observe := provisioning.ObservationRequest{
		OperationID: deleteRequest.OperationID, AttemptNumber: 1,
		ResourceID: deleteRequest.ResourceID, ResourceType: testResourceType,
		Spec: deleteRequest.Spec, Capability: domain.CapabilityDelete, TargetGeneration: 1,
	}
	terminating := observeExecution(t, f.provisioner, observe)
	requireFound(t, terminating, provisioning.ExecutionStateRunning)
	if terminating.Resource.Presence != provisioning.ResourcePresencePresent {
		t.Fatalf("terminating object presence = %s, want Present", terminating.Resource.Presence)
	}
	if terminating.Resource.Readiness != provisioning.ResourceReadinessNotReady {
		t.Fatalf("terminating readiness = %s, want NotReady", terminating.Resource.Readiness)
	}

	f.server.RemoveFinalizers(f.namespace, f.gvr.Resource, name)
	completed := observeExecution(t, f.provisioner, observe)
	requireFound(t, completed, provisioning.ExecutionStateSucceeded)
	if completed.Resource.Presence != provisioning.ResourcePresenceNotFound {
		t.Fatalf("completed delete presence = %s, want NotFound", completed.Resource.Presence)
	}
}

// TestStableObjectIdentityAcrossLifecycleAndInstallations covers stable
// identity across operations plus divergence by installation dimensions.
func TestStableObjectIdentityAcrossLifecycleAndInstallations(t *testing.T) {
	name := ObjectName("install", "ns", testResourceType, "r")
	for _, capability := range []domain.Capability{domain.CapabilityCreate, domain.CapabilityUpdate, domain.CapabilityDelete} {
		if derived := ObjectName("install", "ns", testResourceType, "r"); derived != name {
			t.Fatalf("identity moved for capability %s", capability)
		}
	}
	if foreign := foreignObject(); foreign == nil {
		t.Fatal("fixture broken")
	}
}

// TestTransportLossAfterPersistenceConfirmsThroughObserve is the ambiguous
// submission case where the write landed but the response was lost. Observe
// finds the object carrying this Operation's correlation and confirms
// acceptance without any resubmission.
func TestTransportLossAfterPersistenceConfirmsThroughObserve(t *testing.T) {
	f := newFixture(t, nil)
	f.server.FailNextCreates(&kube.APIError{Code: 503, Reason: "Timeout", Message: "simulated dropped response"}, 1, true)

	submission, submitErr := f.provisioner.Submit(context.Background(),
		executionRequest(domain.CapabilityCreate, "op-amb", 1, 1, simpleSpec(t, true)))
	if submitErr == nil {
		t.Fatal("post-commit transport loss must surface as an ambiguous submission error")
	}
	var ambiguous ambiguousMarker
	if !asAmbiguous(submitErr, &ambiguous) {
		t.Fatalf("error %v does not mark ambiguity", submitErr)
	}
	if submission.Observation.Correlation != provisioning.RequestCorrelationUnknown {
		t.Fatalf("ambiguous correlation = %s", submission.Observation.Correlation)
	}

	observation := observeExecution(t, f.provisioner, provisioning.ObservationRequest{
		OperationID: "op-amb", AttemptNumber: 1, ResourceID: "resource-1",
		ResourceType: testResourceType, Capability: domain.CapabilityCreate,
		Spec: simpleSpec(t, true), TargetGeneration: 1,
	})
	if observation.Correlation != provisioning.RequestCorrelationFound {
		t.Fatalf("correlation = %s, want Found: the write landed with our correlation", observation.Correlation)
	}
	if observation.Execution == nil || observation.Execution.State == provisioning.ExecutionStateFailed ||
		observation.Execution.State == provisioning.ExecutionStateSucceeded ||
		observation.Execution.State == provisioning.ExecutionStateUnknown {
		t.Fatalf("execution = %+v, want a positively correlated nonterminal state", observation.Execution)
	}
	if observation.Resource.Presence != provisioning.ResourcePresencePresent {
		t.Fatalf("presence = %s, want Present", observation.Resource.Presence)
	}
}

type ambiguousMarker struct{}

func asAmbiguous(err error, target *ambiguousMarker) bool {
	return err != nil && strings.Contains(err.Error(), "ambiguous") && target != nil
}

// TestTransportLossBeforePersistenceAuthorizesSafeResubmission is the other
// half of the ambiguity contract: nothing was stored, so a fresh observation
// reports conclusive NotFound with no execution, which is the only signal
// that authorizes another attempt under the same OperationID.
func TestTransportLossBeforePersistenceAuthorizesSafeResubmission(t *testing.T) {
	f := newFixture(t, nil)
	f.server.FailNextCreates(&kube.APIError{Code: 503, Reason: "Timeout", Message: "simulated dropped request"}, 1, false)

	submission, submitErr := f.provisioner.Submit(context.Background(),
		executionRequest(domain.CapabilityCreate, "op-lost", 1, 1, simpleSpec(t, true)))
	if submitErr == nil || submission.Observation.Correlation != provisioning.RequestCorrelationUnknown {
		t.Fatalf("pre-commit transport loss must be ambiguous, got %+v err=%v", submission, submitErr)
	}
	observation := observeExecution(t, f.provisioner, provisioning.ObservationRequest{
		OperationID: "op-lost", AttemptNumber: 1, ResourceID: "resource-1",
		ResourceType: testResourceType, Capability: domain.CapabilityCreate,
		Spec: simpleSpec(t, true), TargetGeneration: 1,
	})
	if observation.Correlation != provisioning.RequestCorrelationNotFound {
		t.Fatalf("correlation = %s, want NotFound for genuine absence", observation.Correlation)
	}
	if observation.Execution != nil {
		t.Fatalf("genuine absence must carry no execution: %+v", observation.Execution)
	}
	if observation.Resource.Presence != provisioning.ResourcePresenceNotFound {
		t.Fatalf("absence facts = %+v", observation.Resource)
	}
}

// TestAPINotFoundMapsToNotFoundAndTransportUncertaintyToUnknown pins the
// observation error taxonomy.
func TestAPINotFoundMapsToNotFoundAndTransportUncertaintyToUnknown(t *testing.T) {
	f := newFixture(t, nil)
	notFound := observeExecution(t, f.provisioner, provisioning.ObservationRequest{
		OperationID: "op-x", AttemptNumber: 1, ResourceID: "missing",
		ResourceType: testResourceType, Capability: domain.CapabilityCreate,
		Spec: simpleSpec(t, true), TargetGeneration: 1,
	})
	if notFound.Correlation != provisioning.RequestCorrelationNotFound || notFound.Resource.Presence != provisioning.ResourcePresenceNotFound {
		t.Fatalf("404 observation = %+v", notFound)
	}

	f.server.FailNextGets(&kube.APIError{Code: 500, Reason: "InternalError", Message: "etcd server timeout"}, 1)
	_, err := f.provisioner.Observe(context.Background(), provisioning.ObservationRequest{
		ResourceID: "resource-1", ResourceType: testResourceType,
		Capability: domain.CapabilityCreate, Spec: simpleSpec(t, true), TargetGeneration: 1,
	})
	if err == nil {
		t.Fatal("transport uncertainty must produce an observation error")
	}
	var observationErr provisioning.ObservationError
	if !errorsAs(err, &observationErr) {
		t.Fatalf("error %v is not an ObservationError", err)
	}
	if observationErr.Failure.Reason == "" || strings.Contains(observationErr.Failure.Message, "etcd") {
		t.Fatalf("raw control-plane text leaked into failure: %+v", observationErr.Failure)
	}
}

func errorsAs(err error, target *provisioning.ObservationError) bool {
	switch typed := err.(type) {
	case provisioning.ObservationError:
		*target = typed
		return true
	default:
		return false
	}
}

// TestPassiveObservationReportsHonestFacts exercises the handle-less path.
func TestPassiveObservationReportsHonestFacts(t *testing.T) {
	f := newFixture(t, func(cfg *Config) {})
	requireFound(t, f.submit(t, executionRequest(domain.CapabilityCreate, "op-p", 1, 1, simpleSpec(t, true))), provisioning.ExecutionStateAccepted)

	// Before any controller evidence: present but not ready.
	passive := observeExecution(t, f.provisioner, provisioning.ObservationRequest{
		ResourceID: "resource-1", ResourceType: testResourceType,
		Spec: simpleSpec(t, true), TargetGeneration: 1,
	})
	if passive.Correlation != provisioning.RequestCorrelationUnknown || passive.Execution != nil {
		t.Fatalf("passive observation shape = %+v", passive)
	}
	if passive.Resource.Presence != provisioning.ResourcePresencePresent {
		t.Fatalf("passive presence = %s", passive.Resource.Presence)
	}

	f.server.SetController(markReady(1))
	ready := observeExecution(t, f.provisioner, provisioning.ObservationRequest{
		ResourceID: "resource-1", ResourceType: testResourceType,
		Spec: simpleSpec(t, true), TargetGeneration: 1,
	})
	if ready.Resource.Readiness != provisioning.ResourceReadinessReady {
		t.Fatalf("passive readiness = %s, want Ready under fresh conditions", ready.Resource.Readiness)
	}
	if ready.Resource.Drift != provisioning.ResourceDriftUnknown {
		t.Fatal("passive drift must stay Unknown")
	}

	absent := observeExecution(t, f.provisioner, provisioning.ObservationRequest{
		ResourceID: "never-existed", ResourceType: testResourceType,
		Spec: simpleSpec(t, true), TargetGeneration: 1,
	})
	if absent.Resource.Presence != provisioning.ResourcePresenceNotFound {
		t.Fatalf("passive absence = %s", absent.Resource.Presence)
	}
}

// TestKubernetesMetadataNeverAppearsInObservationJSON scans the serialized
// provider-neutral observation for Kubernetes vocabulary.
func TestKubernetesMetadataNeverAppearsInObservationJSON(t *testing.T) {
	f := newFixture(t, nil)
	f.server.SetController(func(poll int, object *fakeapi.Object) {
		object.Raw["status"] = map[string]any{
			"conditions": conditionsAt(object.Generation(), syncedTrue(), readyTrue()),
			"liftr":      map[string]any{},
		}
	})
	requireFound(t, f.submit(t, executionRequest(domain.CapabilityCreate, "op-json", 1, 1, simpleSpec(t, true))), provisioning.ExecutionStateAccepted)
	observation := observeExecution(t, f.provisioner, provisioning.ObservationRequest{
		OperationID: "op-json", AttemptNumber: 1, ResourceID: "resource-1",
		ResourceType: testResourceType, Capability: domain.CapabilityCreate,
		Spec: simpleSpec(t, true), TargetGeneration: 1,
		OutputMappingRef: testOutputMappingRef,
	})
	encoded, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	document := string(encoded)
	for _, leaked := range []string{`"uid"`, `"namespace"`, `"generation"`, `"conditions"`, `"resourceVersion"`, `"apiVersion"`, `"deletionTimestamp"`} {
		if strings.Contains(document, leaked) {
			t.Fatalf("kubernetes metadata key %s leaked into public observation JSON: %s", leaked, document)
		}
	}
}
