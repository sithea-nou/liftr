// SPDX-License-Identifier: Apache-2.0

package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/sithea-nou/liftr/internal/domain"
)

var (
	ErrPolicyDenied           = errors.New("platform policy denied admission")
	ErrQuotaExceeded          = errors.New("platform quota exceeded")
	ErrPolicyEvaluation       = errors.New("platform policy evaluation failed")
	ErrPersistenceUnavailable = errors.New("persistence unavailable")
	ErrQuotaInvariant         = errors.New("quota accounting invariant violated")
)

// PolicyRevision identifies one normalized immutable admission-policy
// configuration. It is private operator/audit metadata, not a public Resource
// or Operation field.
type PolicyRevision string

// NewPolicyRevision derives the versioned policy revision from a canonical
// semantic representation. Callers, never configuration authors, own it.
func NewPolicyRevision(canonical []byte) PolicyRevision {
	digest := sha256.Sum256(canonical)
	return PolicyRevision("pol_v1_" + hex.EncodeToString(digest[:]))
}

type AdmissionMutation string

const (
	AdmissionCreate AdmissionMutation = "create"
	AdmissionUpdate AdmissionMutation = "update"
)

func (m AdmissionMutation) valid() bool {
	return m == AdmissionCreate || m == AdmissionUpdate
}

// AdmissionIntent is the complete input policy may inspect in M18. The owner
// has already passed authorization. Principal and ResourceSpec are absent by
// construction so policy cannot grant access or duplicate ResourceType rules.
type AdmissionIntent struct {
	Mutation     AdmissionMutation
	Owner        domain.OwnerRef
	ResourceType domain.ResourceTypeRef
}

type QuotaDimension string

const (
	QuotaOwnerResources     QuotaDimension = "owner_resources"
	QuotaOwnerTypeResources QuotaDimension = "owner_type_resources"
)

type ResourceCountConstraint struct {
	RuleID    string
	Dimension QuotaDimension
	Limit     uint64
}

type PolicyDenialKind string

const (
	PolicyDenialCapabilityDisabled PolicyDenialKind = "capability_disabled"
	PolicyDenialQuotaExceeded      PolicyDenialKind = "quota_exceeded"
)

// PolicyDenial contains private structured diagnostics. HTTP maps the kind to
// Liftr-owned text and never serializes the remaining fields.
type PolicyDenial struct {
	Kind      PolicyDenialKind
	RuleID    string
	Measure   string
	Current   uint64
	Requested uint64
	Limit     uint64
}

type AdmissionPlan struct {
	Intent           AdmissionIntent
	Revision         PolicyRevision
	CapabilityDenial *PolicyDenial
	CountConstraints []ResourceCountConstraint
}

func (p AdmissionPlan) RequiresResourceCounts() bool {
	return len(p.CountConstraints) != 0
}

// ResourceCountFacts binds authoritative counts to the actual admission
// dimensions. Available is explicit so missing facts can never become zero.
type ResourceCountFacts struct {
	Available       bool
	Owner           domain.OwnerRef
	ResourceType    domain.ResourceTypeRef
	OwnerNonDeleted uint64
	TypeNonDeleted  uint64
}

type AdmissionOutcome string

const (
	AdmissionAllowed AdmissionOutcome = "allow"
	AdmissionDenied  AdmissionOutcome = "deny"
)

type AdmissionDecision struct {
	Outcome  AdmissionOutcome
	Revision PolicyRevision
	Denial   *PolicyDenial
}

// AdmissionPolicy is a closed pure port. Implementations plan from immutable
// configuration and decide only from explicit application-supplied facts.
type AdmissionPolicy interface {
	Revision() PolicyRevision
	Plan(AdmissionIntent) (AdmissionPlan, error)
	Decide(AdmissionPlan, ResourceCountFacts) (AdmissionDecision, error)
}

// NoRestrictionsAdmissionPolicy preserves pre-M18 behavior for compositions
// that do not supply a policy. Production composition still loads the empty
// policy file model so its canonical revision is logged and audited.
type NoRestrictionsAdmissionPolicy struct{}

func (NoRestrictionsAdmissionPolicy) Revision() PolicyRevision {
	return NewPolicyRevision([]byte(`{"apiVersion":"liftr.dev/admission-policy/v1","rules":[]}`))
}

func (p NoRestrictionsAdmissionPolicy) Plan(intent AdmissionIntent) (AdmissionPlan, error) {
	if err := validateAdmissionIntent(intent); err != nil {
		return AdmissionPlan{}, err
	}
	return AdmissionPlan{Intent: intent, Revision: p.Revision()}, nil
}

func (p NoRestrictionsAdmissionPolicy) Decide(plan AdmissionPlan, facts ResourceCountFacts) (AdmissionDecision, error) {
	if plan.Revision != p.Revision() || len(plan.CountConstraints) != 0 || plan.CapabilityDenial != nil {
		return AdmissionDecision{}, fmt.Errorf("%w: invalid unrestricted admission plan", ErrPolicyEvaluation)
	}
	if err := validateAdmissionIntent(plan.Intent); err != nil {
		return AdmissionDecision{}, err
	}
	return AdmissionDecision{Outcome: AdmissionAllowed, Revision: p.Revision()}, nil
}

func validateAdmissionIntent(intent AdmissionIntent) error {
	if !intent.Mutation.valid() || intent.Owner.Kind == "" || intent.Owner.ID == "" ||
		intent.ResourceType.Name == "" || intent.ResourceType.Version == "" {
		return fmt.Errorf("%w: admission intent is incomplete", ErrPolicyEvaluation)
	}
	return nil
}

// PolicyAdmissionError carries one closed policy decision through the
// application boundary without exposing configuration-provided text.
type PolicyAdmissionError struct {
	Revision PolicyRevision
	Denial   PolicyDenial
}

func (e *PolicyAdmissionError) Error() string {
	if e == nil {
		return ErrPolicyDenied.Error()
	}
	if e.Denial.Kind == PolicyDenialQuotaExceeded {
		return ErrQuotaExceeded.Error()
	}
	return ErrPolicyDenied.Error()
}

func (e *PolicyAdmissionError) Unwrap() error {
	if e != nil && e.Denial.Kind == PolicyDenialQuotaExceeded {
		return ErrQuotaExceeded
	}
	return ErrPolicyDenied
}

// QuotaRepository owns durable quota serialization and fact acquisition. A
// caller must acquire the actual-owner transaction lock before reading facts
// and retain the transaction through admission commit.
type QuotaRepository interface {
	LockOwnerQuota(context.Context, domain.OwnerRef) error
	ResourceCountFacts(context.Context, domain.OwnerRef, domain.ResourceTypeRef) (ResourceCountFacts, error)
}
