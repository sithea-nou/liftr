// SPDX-License-Identifier: Apache-2.0

package observability_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/observability"
)

type admissionPolicyStub struct {
	decision application.AdmissionDecision
	err      error
}

func (p admissionPolicyStub) Revision() application.PolicyRevision {
	return application.NewPolicyRevision([]byte("observability-test"))
}

func (p admissionPolicyStub) Plan(intent application.AdmissionIntent) (application.AdmissionPlan, error) {
	return application.AdmissionPlan{Intent: intent, Revision: p.Revision()}, nil
}

func (p admissionPolicyStub) Decide(application.AdmissionPlan, application.ResourceCountFacts) (application.AdmissionDecision, error) {
	return p.decision, p.err
}

func TestPolicyMetricsUseOnlyBoundedMutationAndOutcomeLabels(t *testing.T) {
	telemetry := newTestTelemetry(t)
	intent := application.AdmissionIntent{
		Mutation:     application.AdmissionCreate,
		Owner:        domain.OwnerRef{Kind: "team", ID: "private-owner"},
		ResourceType: domain.ResourceTypeRef{Name: "PrivateType", Version: "v1"},
	}
	tests := []admissionPolicyStub{
		{decision: application.AdmissionDecision{Outcome: application.AdmissionAllowed}},
		{decision: application.AdmissionDecision{Outcome: application.AdmissionDenied, Denial: &application.PolicyDenial{Kind: application.PolicyDenialCapabilityDisabled, RuleID: "private-rule"}}},
		{decision: application.AdmissionDecision{Outcome: application.AdmissionDenied, Denial: &application.PolicyDenial{Kind: application.PolicyDenialQuotaExceeded, RuleID: "private-rule"}}},
		{err: errors.New("private evaluator detail")},
	}
	for _, stub := range tests {
		wrapped, err := observability.InstrumentAdmissionPolicy(stub, telemetry)
		if err != nil {
			t.Fatal(err)
		}
		plan, err := wrapped.Plan(intent)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = wrapped.Decide(plan, application.ResourceCountFacts{})
	}

	text := gatherText(t, telemetry)
	for _, expected := range []string{"liftr_policy_admissions_total", "mutation=create", "outcome=allowed", "outcome=policy_denied", "outcome=quota_exceeded", "outcome=error"} {
		if !strings.Contains(text, expected) {
			t.Fatalf("policy metric missing %q:\n%s", expected, text)
		}
	}
	for _, forbidden := range []string{"private-owner", "PrivateType", "private-rule", "private evaluator detail", "pol_v1_"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("private policy value %q leaked into metrics:\n%s", forbidden, text)
		}
	}
}
