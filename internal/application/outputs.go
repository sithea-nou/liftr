// SPDX-License-Identifier: Apache-2.0

package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
)

// OutputResolution is Liftr's durable output-materialization state machine.
// Backend terminal success and reconciliation completion are separate
// monotonic dimensions: an execution can be backend-Succeeded while its
// outputs are still Pending, and a permanently invalid output set fails the
// operation while the backend success evidence remains intact.
//
//	None      no output claim is tracked (delete operations, types without contracts)
//	Pending   backend succeeded; extraction/publication has not completed yet
//	Published validated values are durably persisted for the target generation
//	Rejected  outputs deterministically violate the contract; the operation fails
type OutputResolution string

const (
	OutputResolutionNone      OutputResolution = "None"
	OutputResolutionPending   OutputResolution = "Pending"
	OutputResolutionPublished OutputResolution = "Published"
	OutputResolutionRejected  OutputResolution = "Rejected"
)

func ValidOutputResolution(value OutputResolution) bool {
	switch value {
	case OutputResolutionNone, OutputResolutionPending, OutputResolutionPublished, OutputResolutionRejected:
		return true
	default:
		return false
	}
}

// ReasonOutputPostconditionRejected is the fixed, curated failure reason used
// when managed infrastructure succeeded but the ResourceType's required
// outputs could not be realized. The message carries the narrow sub-
// classification; neither ever contains offending keys or values.
const ReasonOutputPostconditionRejected = "OutputPostconditionRejected"

// ManagedTargetAbsentReason is the fixed success reason recorded when a
// cleanup delete proves conclusively — before any launch and without prior
// acceptance — that the managed target is already absent. Destruction is
// satisfied; ambiguity is never converted into this outcome.
const ManagedTargetAbsentReason = "ManagedTargetAbsent"

// ResourceOutputRecord is the durable proof of how one output snapshot was
// produced. It carries the correlation and provenance that must never enter
// the semantic domain value: producing Operation, private mapping identity,
// contract identity, and the canonical content digest. Records are immutable;
// contradictory evidence for one Resource/generation pair is rejected.
type ResourceOutputRecord struct {
	ResourceID           domain.ResourceID
	ObservedGeneration   uint64
	OperationID          domain.OperationID
	Capability           domain.Capability
	OutputMappingRef     string
	OutputContractDigest string
	Values               domain.ResourceOutputs
	ValuesDigest         string
}

// OutputSnapshot exposes the semantic portion of a record.
func (r ResourceOutputRecord) OutputSnapshot() domain.ResourceOutputs { return r.Values }

// ResourceOutputRepository persists immutable output snapshots. Save is
// idempotent only when provenance and content digests match exactly;
// contradicting evidence fails closed. Latest returns the snapshot with the
// highest published generation.
type ResourceOutputRepository interface {
	SaveResourceOutputs(context.Context, ResourceOutputRecord) error
	LatestResourceOutputs(context.Context, domain.ResourceID) (ResourceOutputRecord, bool, error)
}

// OutputContractDigest derives the stable identity of an output contract's
// declared fields. It participates in provenance only.
func OutputContractDigest(contract *resourcecontract.OutputContract) (string, error) {
	if contract == nil {
		return "", nil
	}
	fields := contract.Fields()
	encoded, err := json.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("encode output contract: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// ValuesDigest derives the canonical content identity of validated output
// values. encoding/json sorts map keys deterministically.
func ValuesDigest(values map[string]any) (string, error) {
	encoded, err := json.Marshal(values)
	if err != nil {
		return "", fmt.Errorf("encode output values: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// OutputPlanAction classifies how terminal create/update success interacts
// with the ResourceType's declared outputs.
type OutputPlanAction int

const (
	// OutputPlanNone means no output publication is involved; finish plainly.
	OutputPlanNone OutputPlanAction = iota
	// OutputPlanPublish publishes the validated snapshot atomically with
	// lifecycle success.
	OutputPlanPublish
	// OutputPlanDefer keeps the operation active: persist backend success
	// with Pending resolution and re-drive extraction. Never re-executes the
	// backend.
	OutputPlanDefer
	// OutputPlanReject marks resolution Rejected and finishes the operation
	// as failed with the curated postcondition reason.
	OutputPlanReject
)

// OutputPlan is the decision produced by PlanTerminalOutputs.
type OutputPlan struct {
	Action   OutputPlanAction
	Snapshot domain.ResourceOutputs
	Failure  provisioning.ExecutionFailure
}

// PlanTerminalOutputs inspects provider output evidence against the
// developer contract. Evidence is untrusted implementation input; only
// contract-validated scalars may ever become a public snapshot.
func PlanTerminalOutputs(contract resourcecontract.Contract, capability domain.Capability, evidence *provisioning.OutputEvidence, targetGeneration uint64, publishedAt time.Time) (OutputPlan, error) {
	if capability == domain.CapabilityDelete {
		return OutputPlan{Action: OutputPlanNone}, nil
	}
	declared := contract != nil && contract.OutputContract() != nil
	if !declared {
		// A provider that emits outputs for a type without a contract violates
		// the boundary: undeclared output never reaches persistence or clients.
		if evidence != nil && (evidence.State == provisioning.OutputsAvailable || evidence.State == provisioning.OutputsInvalid) {
			return OutputPlan{Action: OutputPlanReject, Failure: provisioning.ExecutionFailure{
				Kind: provisioning.FailureUnknown, Reason: ReasonOutputPostconditionRejected,
				Message: "provider reported outputs for a resource type without an output contract",
			}}, nil
		}
		return OutputPlan{Action: OutputPlanNone}, nil
	}
	requires := contract.OutputContract().RequiresOutputs()
	if evidence == nil {
		if requires {
			return OutputPlan{Action: OutputPlanDefer}, nil
		}
		return OutputPlan{Action: OutputPlanNone}, nil
	}
	switch evidence.State {
	case provisioning.OutputsUnavailable:
		if requires {
			return OutputPlan{Action: OutputPlanDefer}, nil
		}
		return OutputPlan{Action: OutputPlanNone}, nil
	case provisioning.OutputsInvalid:
		message := evidence.Reason
		if message == "" {
			message = "declared outputs deterministically violated the resource type contract"
		}
		return OutputPlan{Action: OutputPlanReject, Failure: provisioning.ExecutionFailure{
			Kind: provisioning.FailureUnknown, Reason: ReasonOutputPostconditionRejected, Message: message,
		}}, nil
	case provisioning.OutputsAvailable:
		if err := contract.OutputContract().Validate(evidence.Values); err != nil {
			return OutputPlan{Action: OutputPlanReject, Failure: provisioning.ExecutionFailure{
				Kind: provisioning.FailureUnknown, Reason: ReasonOutputPostconditionRejected, Message: err.Error(),
			}}, nil
		}
		snapshot, err := domain.NewResourceOutputs(targetGeneration, evidence.Values, publishedAt)
		if err != nil {
			return OutputPlan{Action: OutputPlanReject, Failure: provisioning.ExecutionFailure{
				Kind: provisioning.FailureUnknown, Reason: ReasonOutputPostconditionRejected, Message: err.Error(),
			}}, nil
		}
		return OutputPlan{Action: OutputPlanPublish, Snapshot: snapshot}, nil
	default:
		return OutputPlan{}, fmt.Errorf("invalid output evidence state %q", evidence.State)
	}
}
