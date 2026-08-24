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

// observeAbsenceScenario drives one execution observation for resource-1 at
// the given generation and returns either the observation or the error.
func observeAbsenceScenario(t *testing.T, f *fixture, operationID string, capability domain.Capability, generation uint64) (provisioning.ExecutionObservation, error) {
	t.Helper()
	return f.provisioner.Observe(context.Background(), provisioning.ObservationRequest{
		OperationID:      domain.OperationID(operationID),
		AttemptNumber:    1,
		ResourceID:       "resource-1",
		ResourceType:     testResourceType,
		Spec:             simpleSpec(t, true),
		Capability:       capability,
		TargetGeneration: generation,
	})
}

// TestA_GVRSeededAndObjectAbsentReportsNotFound: positive evidence that the
// API resource is served plus a definitive object 404 establishes genuine
// managed absence.
func TestA_GVRSeededAndObjectAbsentReportsNotFound(t *testing.T) {
	f := newFixture(t, nil)
	observation, err := observeAbsenceScenario(t, f, "op-absent", domain.CapabilityCreate, 1)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Correlation != provisioning.RequestCorrelationNotFound || observation.Resource.Presence != provisioning.ResourcePresenceNotFound {
		t.Fatalf("served-GVR absence = %+v", observation)
	}
	if observation.Execution != nil {
		t.Fatal("genuine absence carries no execution")
	}
}

// TestB_GVRAbsentIsKindUnavailableNeverAbsence: when the CRD/API kind is
// gone, an object-path 404 yields a curated kind-unavailable classification —
// never NotFound correlation and never Presence=NotFound.
func TestB_GVRAbsentIsKindUnavailableNeverAbsence(t *testing.T) {
	f := newFixture(t, nil)
	f.server.RetireCRD(f.namespace, f.gvr.Resource)
	observation, err := observeAbsenceScenario(t, f, "op-nokind", domain.CapabilityCreate, 1)
	if err == nil {
		t.Fatalf("unserved kind produced %+v instead of a failure", observation)
	}
	var observationErr provisioning.ObservationError
	if !errorsAs(err, &observationErr) {
		t.Fatalf("error %v is not an ObservationError", err)
	}
	// Mid-operation observations stay retryable (Unavailable) so the
	// operation survives until an operator restores the API kind; the
	// curated reason carries the precise classification. Either way the
	// answer is never NotFound correlation or Presence=NotFound.
	if observationErr.Failure.Kind != provisioning.FailureUnavailable {
		t.Fatalf("kind = %s", observationErr.Failure.Kind)
	}
	if observationErr.Failure.Reason != reasonTargetKindUnregistered {
		t.Fatalf("reason = %s, want TargetKindUnregistered", observationErr.Failure.Reason)
	}
}

// TestC_DeleteObserveWithGVRAbsentDoesNotCompleteDeletion: a missing CRD is
// a control-plane failure. Deletion completion requires served-GVR proof —
// both at the delete preflight and after an accepted DELETE whose object
// then disappears together with its API kind.
func TestC_DeleteObserveWithGVRAbsentDoesNotCompleteDeletion(t *testing.T) {
	t.Run("delete preflight against retired kind", func(t *testing.T) {
		f := newFixture(t, nil)
		f.submit(t, executionRequest(domain.CapabilityCreate, "op-del", 1, 1, simpleSpec(t, true)))
		f.server.RetireCRD(f.namespace, f.gvr.Resource)
		submission, err := f.provisioner.Submit(context.Background(),
			executionRequest(domain.CapabilityDelete, "op-del-go", 1, 1, simpleSpec(t, true)))
		if err != nil {
			t.Fatal(err)
		}
		execution := submission.Observation.Execution
		if execution == nil || execution.State != provisioning.ExecutionStateFailed ||
			execution.Failure.Reason != reasonTargetKindUnregistered {
			t.Fatalf("delete against retired CRD = %+v", submission.Observation)
		}
		if submission.Observation.Correlation == provisioning.RequestCorrelationNotFound &&
			execution.Failure.Kind == provisioning.FailureNotFound {
			t.Fatal("retired CRD was misreported as conclusive managed absence")
		}
	})

	t.Run("accepted delete cannot complete once the kind disappears", func(t *testing.T) {
		f := newFixture(t, nil)
		f.submit(t, executionRequest(domain.CapabilityCreate, "op-c2", 1, 1, simpleSpec(t, true)))
		name := ObjectName("m14-tests", f.namespace, testResourceType, "resource-1")
		f.server.Mutate(f.namespace, f.gvr.Resource, name, func(object *fakeapi.Object) { object.SetFinalizer("held") })
		requireFound(t, f.submit(t, executionRequest(domain.CapabilityDelete, "op-c2-del", 1, 1, simpleSpec(t, true))),
			provisioning.ExecutionStateAccepted)
		// The CRD vanishes mid-deletion, taking its instances with it. The
		// next observation sees a 404 on an unserved kind and must refuse to
		// report completed destruction.
		f.server.RetireCRD(f.namespace, f.gvr.Resource)
		_, err := observeAbsenceScenario(t, f, "op-c2-del", domain.CapabilityDelete, 1)
		var observationErr provisioning.ObservationError
		if !errorsAs(err, &observationErr) || observationErr.Failure.Reason != reasonTargetKindUnregistered {
			t.Fatalf("delete observation after CRD removal resolved to %+v err=%v", nil, err)
		}
	})
}

// TestD_AmbiguousCreateWithGVRAbsentDoesNotResubmit: an ambiguous create
// followed by a kind-level 404 must stay unresolved; only served-GVR absence
// authorizes another attempt.
func TestD_AmbiguousCreateWithGVRAbsentDoesNotResubmit(t *testing.T) {
	f := newFixture(t, nil)
	f.server.FailNextCreates(&kube.APIError{Code: 503, Reason: "Timeout", Message: "dropped"}, 1, false)
	if _, submitErr := f.provisioner.Submit(context.Background(),
		executionRequest(domain.CapabilityCreate, "op-amb2", 1, 1, simpleSpec(t, true))); submitErr == nil {
		t.Fatal("expected ambiguous submission")
	}
	f.server.RetireCRD(f.namespace, f.gvr.Resource)
	_, err := observeAbsenceScenario(t, f, "op-amb2", domain.CapabilityCreate, 1)
	var observationErr provisioning.ObservationError
	if !errorsAs(err, &observationErr) {
		t.Fatalf("kind-absent ambiguity resolved to %+v err=%v; it must never resolve to NotFound", err, err)
	}
	if strings.Contains(observationErr.Failure.Reason+observationErr.Failure.Message, "absent") {
		t.Fatalf("kind-unavailable classification claims absence: %+v", observationErr.Failure)
	}
}

// TestE_AmbiguousUpdateWithGVRAbsentDoesNotResubmit: same invariant for an
// update whose patch landed but whose response was lost before the CRD
// vanished.
func TestE_AmbiguousUpdateWithGVRAbsentDoesNotResubmit(t *testing.T) {
	f := newFixture(t, nil)
	f.submit(t, executionRequest(domain.CapabilityCreate, "op-e-create", 1, 1, simpleSpec(t, true)))
	f.server.FailNextUpdates(&kube.APIError{Code: 503, Reason: "Timeout", Message: "dropped"}, 1, true)
	if _, submitErr := f.provisioner.Submit(context.Background(),
		executionRequest(domain.CapabilityUpdate, "op-e-update", 1, 2, simpleSpec(t, false))); submitErr == nil {
		t.Fatal("expected ambiguous update submission")
	}
	f.server.RetireCRD(f.namespace, f.gvr.Resource)
	_, err := observeAbsenceScenario(t, f, "op-e-update", domain.CapabilityUpdate, 2)
	var observationErr provisioning.ObservationError
	if !errorsAs(err, &observationErr) {
		t.Fatalf("update ambiguity resolved to %+v err=%v; kind absence must never authorize resubmission", err, err)
	}
}

// TestF_DiscoveryRemovedBeforeObject404IsSeenLive: Liftr keeps no discovery
// cache, so a GVR that was served during create but removed before the next
// object 404 is answered from live verification — stale knowledge cannot
// manufacture absence.
func TestF_DiscoveryRemovedBeforeObject404IsSeenLive(t *testing.T) {
	f := newFixture(t, nil)
	f.submit(t, executionRequest(domain.CapabilityCreate, "op-f", 1, 1, simpleSpec(t, true)))
	name := ObjectName("m14-tests", f.namespace, testResourceType, "resource-1")
	// CRD removal deletes instances and unserves the kind after creation.
	f.server.RetireCRD(f.namespace, f.gvr.Resource)
	if _, stillThere := f.server.Get(f.namespace, f.gvr.Resource, name); stillThere {
		t.Fatal("fixture failed to remove instances on retirement")
	}
	_, err := observeAbsenceScenario(t, f, "op-f", domain.CapabilityUpdate, 2)
	if err == nil {
		t.Fatal("stale-served knowledge must not authorize NotFound")
	}
	var observationErr provisioning.ObservationError
	if !errorsAs(err, &observationErr) || observationErr.Failure.Reason != reasonTargetKindUnregistered {
		t.Fatalf("expected curated kind-unavailable failure, got %v", err)
	}
}

// TestG_DiscoveryUncertaintyFailsClosed: authorization denials and server
// faults during discovery yield unavailable classifications — no NotFound,
// no deletion completion, no resubmission signal.
func TestG_DiscoveryUncertaintyFailsClosed(t *testing.T) {
	t.Run("observe stays uncertain", func(t *testing.T) {
		f := newFixture(t, nil)
		f.server.FailNextDiscoveries(&kube.APIError{Code: 500, Reason: "InternalError", Message: "SECRET discovery backend text"}, 5)
		_, err := observeAbsenceScenario(t, f, "op-g", domain.CapabilityCreate, 1)
		var observationErr provisioning.ObservationError
		if !errorsAs(err, &observationErr) {
			t.Fatalf("discovery uncertainty produced %+v, want an observation error", err)
		}
		if observationErr.Failure.Reason != reasonControlPlaneUnavailable {
			t.Fatalf("reason = %s, want ControlPlaneUnavailable", observationErr.Failure.Reason)
		}
	})

	t.Run("delete preflight cannot claim managed absence", func(t *testing.T) {
		f := newFixture(t, nil)
		f.server.FailNextDiscoveries(&kube.APIError{Code: 403, Reason: "Forbidden", Message: "SECRET RBAC text"}, 5)
		submission, submitErr := f.provisioner.Submit(context.Background(),
			executionRequest(domain.CapabilityDelete, "op-g-del", 1, 1, simpleSpec(t, true)))
		if submitErr != nil {
			t.Fatal(submitErr)
		}
		execution := submission.Observation.Execution
		if execution == nil || execution.State != provisioning.ExecutionStateFailed ||
			execution.Failure.Kind != provisioning.FailureUnavailable {
			t.Fatalf("delete under uncertain discovery = %+v", submission.Observation)
		}
		if submission.Observation.Resource.Presence == provisioning.ResourcePresenceNotFound {
			t.Fatal("uncertain discovery reported physical absence")
		}
	})
}

// TestH_NoDiscoveryErrorTextLeaksPublicly pins the raw-text boundary for the
// discovery path specifically.
func TestH_NoDiscoveryErrorTextLeaksPublicly(t *testing.T) {
	f := newFixture(t, nil)
	f.server.FailNextDiscoveries(&kube.APIError{
		Code: 403, Reason: "Forbidden",
		Message: `User "system:serviceaccount:x:liftr" cannot get path "/apis" — CLASSIFIED-DISCOVERY-TEXT`,
	}, 5)
	_, err := observeAbsenceScenario(t, f, "op-h", domain.CapabilityCreate, 1)
	if err == nil {
		t.Fatal("expected a failure to inspect")
	}
	rendered := err.Error()
	if strings.Contains(rendered, "CLASSIFIED-DISCOVERY-TEXT") || strings.Contains(rendered, "system:serviceaccount") {
		t.Fatalf("raw discovery error text leaked: %s", rendered)
	}
	var observationErr provisioning.ObservationError
	if errorsAs(err, &observationErr) && observationErr.Failure.Message == "" {
		t.Fatal("curated failures always carry a message")
	}
}

// TestDiscoveryAnswersOnlyServedness documents the security boundary:
// discovery state changes ownership outcomes in no way. A foreign object on
// a served GVR fails closed exactly as before.
func TestDiscoveryAnswersOnlyServedness(t *testing.T) {
	f := newFixture(t, nil)
	name := ObjectName("m14-tests", f.namespace, testResourceType, "resource-1")
	f.server.Put(f.namespace, f.gvr.Resource, name, foreignObject())
	submission, err := f.provisioner.Submit(context.Background(),
		executionRequest(domain.CapabilityDelete, "op-disc", 1, 1, simpleSpec(t, true)))
	if err != nil {
		t.Fatal(err)
	}
	requireFound(t, submission.Observation, provisioning.ExecutionStateFailed)
	if submission.Observation.Execution.Failure.Reason != reasonTargetIdentityConflict {
		t.Fatalf("reason = %s", submission.Observation.Execution.Failure.Reason)
	}
}
