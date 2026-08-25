// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"time"

	"github.com/sithea-nou/liftr/internal/application"
)

type assessmentDTO struct {
	State          application.RecoveryState        `json:"state"`
	Reasons        []application.DiagnosticReason   `json:"reasons"`
	AllowedActions []application.OperatorActionKind `json:"allowedActions"`
}

func assessmentOf(value application.RecoveryAssessment) assessmentDTO {
	reasons := append([]application.DiagnosticReason(nil), value.Reasons...)
	actions := append([]application.OperatorActionKind(nil), value.AllowedActions...)
	if reasons == nil {
		reasons = []application.DiagnosticReason{}
	}
	if actions == nil {
		actions = []application.OperatorActionKind{}
	}
	return assessmentDTO{State: value.State, Reasons: reasons, AllowedActions: actions}
}

type operationRefDTO struct {
	ID               string `json:"id"`
	Capability       string `json:"capability"`
	State            string `json:"state"`
	Phase            string `json:"phase"`
	TargetGeneration uint64 `json:"targetGeneration"`
}

func operationRefOf(value *application.OperationRefSummary) *operationRefDTO {
	if value == nil {
		return nil
	}
	return &operationRefDTO{
		ID: string(value.ID), Capability: string(value.Capability), State: string(value.State),
		Phase: string(value.Phase), TargetGeneration: value.TargetGeneration,
	}
}

type resourceDiagnosticDTO struct {
	ResourceID                   string            `json:"resourceId"`
	ResourceType                 string            `json:"resourceType"`
	ResourceVersion              string            `json:"resourceVersion"`
	OwnerKind                    string            `json:"ownerKind"`
	OwnerID                      string            `json:"ownerId"`
	Generation                   uint64            `json:"generation"`
	ObservedGeneration           uint64            `json:"observedGeneration"`
	State                        string            `json:"state"`
	StatusUpdatedAt              time.Time         `json:"statusUpdatedAt"`
	ActiveOperation              *operationRefDTO  `json:"activeOperation,omitempty"`
	LatestOperation              *operationRefDTO  `json:"latestOperation,omitempty"`
	OperationAgeSeconds          float64           `json:"operationAgeSeconds"`
	ReconciliationSilenceSeconds float64           `json:"reconciliationSilenceAgeSeconds"`
	OutputResolution             string            `json:"outputResolution"`
	ProvisionerKind              string            `json:"provisionerKind,omitempty"`
	ProvisionerRef               string            `json:"provisionerRef"`
	RegistrationAvailable        bool              `json:"registrationAvailable"`
	StateIdentity                *stateIdentityDTO `json:"stateIdentity,omitempty"`
	SpecDigest                   string            `json:"specDigest,omitempty"`
	Assessment                   assessmentDTO     `json:"recovery"`
}

type stateIdentityDTO struct {
	ProvisionerRef string `json:"provisionerRef"`
	Engine         string `json:"engine"`
	Program        string `json:"program"`
	Backend        string `json:"backend"`
	StateKey       string `json:"stateKey"`
	LineagePresent bool   `json:"lineagePresent"`
	Serial         uint64 `json:"serial"`
	DigestPrefix   string `json:"digestPrefix,omitempty"`
	Version        uint64 `json:"version"`
}

func stateIdentityOf(value *application.StateIdentitySummary) *stateIdentityDTO {
	if value == nil {
		return nil
	}
	return &stateIdentityDTO{
		ProvisionerRef: value.ProvisionerRef, Engine: value.Engine, Program: value.Program,
		Backend: value.Backend, StateKey: value.StateKey, LineagePresent: value.LineagePresent,
		Serial: value.Serial, DigestPrefix: value.DigestPrefix, Version: value.Version,
	}
}

type executionDTO struct {
	State                   string `json:"state"`
	Correlation             string `json:"correlation"`
	AcceptanceConfirmed     bool   `json:"acceptanceConfirmed"`
	HandlePresent           bool   `json:"handlePresent"`
	OutputRecovery          bool   `json:"outputRecovery"`
	OutputResolution        string `json:"outputResolution"`
	OutputFailureKind       string `json:"outputFailureKind,omitempty"`
	CurrentAttempt          uint64 `json:"currentAttempt"`
	NextObservationSequence uint64 `json:"nextObservationSequence"`
}

type attemptDTO struct {
	Number          uint64    `json:"number"`
	State           string    `json:"state"`
	BoundaryCrossed bool      `json:"submissionBoundaryCrossed"`
	ClaimedAt       time.Time `json:"claimedAt,omitempty"`
	ResolvedAt      time.Time `json:"resolvedAt,omitempty"`
	FailureKind     string    `json:"failureKind,omitempty"`
}

type workRefDTO struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	State        string    `json:"state"`
	CreatedAt    time.Time `json:"createdAt"`
	AvailableAt  time.Time `json:"availableAt"`
	AttemptCount int       `json:"attemptCount"`
}

type operationDiagnosticDTO struct {
	OperationID      string        `json:"operationId"`
	ResourceID       string        `json:"resourceId"`
	Capability       string        `json:"capability"`
	TargetGeneration uint64        `json:"targetGeneration"`
	State            string        `json:"state"`
	Phase            string        `json:"phase"`
	RetryOf          string        `json:"retryOf,omitempty"`
	RequestedAt      time.Time     `json:"requestedAt"`
	StartedAt        time.Time     `json:"startedAt,omitempty"`
	CompletedAt      time.Time     `json:"completedAt,omitempty"`
	AgeSeconds       float64       `json:"operationAgeSeconds"`
	Execution        *executionDTO `json:"execution,omitempty"`
	// LatestAttempt is the highest-numbered attempt only; older attempts are
	// deliberately not part of this bounded snapshot.
	LatestAttempt *attemptDTO `json:"latestAttempt,omitempty"`
	AttemptCount  uint64      `json:"attemptCount"`
	// ActiveWork holds the complete active (Pending/Leased) set, which the
	// durable schema bounds to one row per kind. Historical work is never
	// returned as an array; WorkCount carries the honest total instead.
	ActiveWork            []workRefDTO  `json:"activeWork"`
	ActiveWorkTruncated   bool          `json:"activeWorkTruncated"`
	WorkCount             int           `json:"workCount"`
	ProvisionerRef        string        `json:"provisionerRef,omitempty"`
	RegistrationAvailable bool          `json:"registrationAvailable"`
	Assessment            assessmentDTO `json:"recovery"`
}

type workDiagnosticDTO struct {
	WorkID               string        `json:"workId"`
	Kind                 string        `json:"kind"`
	State                string        `json:"state"`
	OperationID          string        `json:"operationId,omitempty"`
	ResourceID           string        `json:"resourceId,omitempty"`
	AttemptNumber        uint64        `json:"attemptNumber,omitempty"`
	CreatedAt            time.Time     `json:"createdAt"`
	AvailableAt          time.Time     `json:"availableAt"`
	AttemptCount         int           `json:"attemptCount"`
	LeaseActive          bool          `json:"leaseActive"`
	LeaseExpired         bool          `json:"leaseExpired"`
	TerminalReasonClass  string        `json:"terminalReasonClass,omitempty"`
	TargetTerminal       bool          `json:"targetTerminal"`
	ActiveEquivalentWork bool          `json:"activeEquivalentWork"`
	Superseded           bool          `json:"superseded"`
	Assessment           assessmentDTO `json:"recovery"`
}

type mutationDTO struct {
	Result           string `json:"result"`
	Action           string `json:"action"`
	TargetKind       string `json:"targetKind"`
	TargetID         string `json:"targetId"`
	OperatorActionID string `json:"operatorActionId"`
	CreatedWorkID    string `json:"createdWorkId"`
}
