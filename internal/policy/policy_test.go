// SPDX-License-Identifier: Apache-2.0

package policy_test

import (
	"errors"
	"testing"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/policy"
)

var (
	widget = domain.ResourceTypeRef{Name: "Widget", Version: "v1"}
	owner  = domain.OwnerRef{Kind: "team", ID: "payments"}
)

func parse(t *testing.T, document string) *policy.Policy {
	t.Helper()
	compiled, err := policy.Parse([]byte(document), []domain.ResourceTypeRef{widget})
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func TestSelectorGrammarAndComposition(t *testing.T) {
	compiled := parse(t, `{
  "apiVersion":"liftr.dev/admission-policy/v1",
  "rules":[
    {"id":"all-owner-total","kind":"resource-count-quota","limit":10},
    {"id":"widget-per-owner","kind":"resource-count-quota","resourceType":{"name":"Widget","version":"v1"},"limit":5},
    {"id":"payments-widget","kind":"resource-count-quota","owner":{"kind":"team","id":"payments"},"resourceType":{"name":"Widget","version":"v1"},"limit":3},
    {"id":"disable-update","kind":"capability-deny","resourceType":{"name":"Widget","version":"v1"},"capabilities":["update"]}
  ]
}`)
	create := application.AdmissionIntent{Mutation: application.AdmissionCreate, Owner: owner, ResourceType: widget}
	plan, err := compiled.Plan(create)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.CountConstraints) != 2 {
		t.Fatalf("constraints=%v", plan.CountConstraints)
	}
	if plan.CountConstraints[0].Limit != 10 || plan.CountConstraints[1].Limit != 3 || plan.CountConstraints[1].RuleID != "payments-widget" {
		t.Fatalf("constraints=%+v", plan.CountConstraints)
	}
	decision, err := compiled.Decide(plan, application.ResourceCountFacts{
		Available: true, Owner: owner, ResourceType: widget, OwnerNonDeleted: 9, TypeNonDeleted: 2,
	})
	if err != nil || decision.Outcome != application.AdmissionAllowed {
		t.Fatalf("decision=%+v error=%v", decision, err)
	}
	plan, err = compiled.Plan(application.AdmissionIntent{Mutation: application.AdmissionUpdate, Owner: owner, ResourceType: widget})
	if err != nil {
		t.Fatal(err)
	}
	if plan.RequiresResourceCounts() {
		t.Fatal("update unexpectedly requires count facts")
	}
	decision, err = compiled.Decide(plan, application.ResourceCountFacts{})
	if err != nil || decision.Outcome != application.AdmissionDenied || decision.Denial.Kind != application.PolicyDenialCapabilityDisabled {
		t.Fatalf("decision=%+v error=%v", decision, err)
	}
}

func TestQuotaDeniesAndMissingFactsFailClosed(t *testing.T) {
	compiled := parse(t, `{"apiVersion":"liftr.dev/admission-policy/v1","rules":[{"id":"limit","kind":"resource-count-quota","limit":1}]}`)
	plan, err := compiled.Plan(application.AdmissionIntent{Mutation: application.AdmissionCreate, Owner: owner, ResourceType: widget})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := compiled.Decide(plan, application.ResourceCountFacts{}); !errors.Is(err, application.ErrPolicyEvaluation) {
		t.Fatalf("missing facts error=%v", err)
	}
	decision, err := compiled.Decide(plan, application.ResourceCountFacts{Available: true, Owner: owner, ResourceType: widget, OwnerNonDeleted: 1})
	if err != nil || decision.Outcome != application.AdmissionDenied || decision.Denial.Kind != application.PolicyDenialQuotaExceeded {
		t.Fatalf("decision=%+v error=%v", decision, err)
	}
}

func TestRevisionIgnoresInputOrderAndFormatting(t *testing.T) {
	first := parse(t, `{"apiVersion":"liftr.dev/admission-policy/v1","rules":[{"id":"b","kind":"resource-count-quota","limit":5},{"id":"a","kind":"capability-deny","resourceType":{"name":"Widget","version":"v1"},"capabilities":["update","create"]}]}`)
	second := parse(t, `{
  "rules": [
    {"capabilities":["create","update"],"resourceType":{"version":"v1","name":"Widget"},"kind":"capability-deny","id":"a"},
    {"limit":5,"kind":"resource-count-quota","id":"b"}
  ],
  "apiVersion":"liftr.dev/admission-policy/v1"
}`)
	if first.Revision() != second.Revision() {
		t.Fatalf("revisions differ: %s != %s", first.Revision(), second.Revision())
	}
	changed := parse(t, `{"apiVersion":"liftr.dev/admission-policy/v1","rules":[{"id":"b","kind":"resource-count-quota","limit":4},{"id":"a","kind":"capability-deny","resourceType":{"name":"Widget","version":"v1"},"capabilities":["create","update"]}]}`)
	if first.Revision() == changed.Revision() {
		t.Fatal("semantic change retained revision")
	}
}

func TestStrictParsingRejectsInvalidGrammar(t *testing.T) {
	tests := map[string]string{
		"duplicate JSON member": `{"apiVersion":"liftr.dev/admission-policy/v1","apiVersion":"liftr.dev/admission-policy/v1","rules":[]}`,
		"trailing document":     `{"apiVersion":"liftr.dev/admission-policy/v1","rules":[]} {}`,
		"unknown field":         `{"apiVersion":"liftr.dev/admission-policy/v1","rules":[],"Rules":[]}`,
		"null rules":            `{"apiVersion":"liftr.dev/admission-policy/v1","rules":null}`,
		"deny missing type":     `{"apiVersion":"liftr.dev/admission-policy/v1","rules":[{"id":"deny","kind":"capability-deny","capabilities":["create"]}]}`,
		"deny delete":           `{"apiVersion":"liftr.dev/admission-policy/v1","rules":[{"id":"deny","kind":"capability-deny","resourceType":{"name":"Widget","version":"v1"},"capabilities":["delete"]}]}`,
		"unknown type":          `{"apiVersion":"liftr.dev/admission-policy/v1","rules":[{"id":"quota","kind":"resource-count-quota","resourceType":{"name":"Other","version":"v1"},"limit":1}]}`,
		"duplicate quota":       `{"apiVersion":"liftr.dev/admission-policy/v1","rules":[{"id":"one","kind":"resource-count-quota","limit":1},{"id":"two","kind":"resource-count-quota","limit":2}]}`,
		"duplicate capability":  `{"apiVersion":"liftr.dev/admission-policy/v1","rules":[{"id":"one","kind":"capability-deny","resourceType":{"name":"Widget","version":"v1"},"capabilities":["create"]},{"id":"two","kind":"capability-deny","resourceType":{"name":"Widget","version":"v1"},"capabilities":["create"]}]}`,
	}
	for name, document := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := policy.Parse([]byte(document), []domain.ResourceTypeRef{widget}); err == nil {
				t.Fatal("invalid policy was accepted")
			}
		})
	}
}

func TestOmittedOwnerIsPerActualOwnerNotGlobal(t *testing.T) {
	compiled := parse(t, `{"apiVersion":"liftr.dev/admission-policy/v1","rules":[{"id":"limit","kind":"resource-count-quota","limit":1}]}`)
	for _, actual := range []domain.OwnerRef{owner, {Kind: "team", ID: "other"}} {
		plan, err := compiled.Plan(application.AdmissionIntent{Mutation: application.AdmissionCreate, Owner: actual, ResourceType: widget})
		if err != nil {
			t.Fatal(err)
		}
		decision, err := compiled.Decide(plan, application.ResourceCountFacts{Available: true, Owner: actual, ResourceType: widget})
		if err != nil || decision.Outcome != application.AdmissionAllowed {
			t.Fatalf("owner=%v decision=%+v error=%v", actual, decision, err)
		}
	}
}
