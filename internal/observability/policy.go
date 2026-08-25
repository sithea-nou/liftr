// SPDX-License-Identifier: Apache-2.0

package observability

import (
	"context"
	"errors"

	"github.com/sithea-nou/liftr/internal/application"
	"go.opentelemetry.io/otel/metric"
)

const (
	PolicyOutcomeAllowed       = "allowed"
	PolicyOutcomeDenied        = "policy_denied"
	PolicyOutcomeQuotaExceeded = "quota_exceeded"
	PolicyOutcomeError         = "error"
)

type instrumentedAdmissionPolicy struct {
	inner application.AdmissionPolicy
	tel   *Telemetry
}

func InstrumentAdmissionPolicy(inner application.AdmissionPolicy, tel *Telemetry) (application.AdmissionPolicy, error) {
	if inner == nil || tel == nil || tel.instruments == nil {
		return nil, errors.New("instrumented admission policy dependencies are required")
	}
	return &instrumentedAdmissionPolicy{inner: inner, tel: tel}, nil
}

func (p *instrumentedAdmissionPolicy) Revision() application.PolicyRevision {
	return p.inner.Revision()
}

func (p *instrumentedAdmissionPolicy) Plan(intent application.AdmissionIntent) (application.AdmissionPlan, error) {
	plan, err := p.inner.Plan(intent)
	if err != nil {
		p.tel.policyAdmission(intent.Mutation, PolicyOutcomeError)
	}
	return plan, err
}

func (p *instrumentedAdmissionPolicy) Decide(plan application.AdmissionPlan, facts application.ResourceCountFacts) (application.AdmissionDecision, error) {
	decision, err := p.inner.Decide(plan, facts)
	outcome := PolicyOutcomeAllowed
	switch {
	case err != nil:
		outcome = PolicyOutcomeError
	case decision.Denial != nil && decision.Denial.Kind == application.PolicyDenialQuotaExceeded:
		outcome = PolicyOutcomeQuotaExceeded
	case decision.Outcome == application.AdmissionDenied:
		outcome = PolicyOutcomeDenied
	}
	p.tel.policyAdmission(plan.Intent.Mutation, outcome)
	return decision, err
}

func (t *Telemetry) policyAdmission(mutation application.AdmissionMutation, outcome string) {
	if !t.ready() {
		return
	}
	mutationLabel := string(mutation)
	if mutation != application.AdmissionCreate && mutation != application.AdmissionUpdate {
		mutationLabel = "invalid"
	}
	t.instruments.policyAdmissions.Add(context.Background(), 1, metric.WithAttributes(
		attributeString(attrPolicyMutation, mutationLabel),
		attributeString(attrPolicyOutcome, outcome),
	))
}
