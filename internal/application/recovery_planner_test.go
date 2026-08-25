// SPDX-License-Identifier: Apache-2.0

package application

import (
	"testing"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

func TestRecoveryPlannerConservativeBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		assessment RecoveryAssessment
		state      RecoveryState
		action     OperatorActionKind
	}{
		{
			name: "observable accepted operation", state: RecoverySafeObserve, action: ActionKindTriggerObserve,
			assessment: PlanOperationObserve(OperationRecoverySnapshot{
				OperationState: domain.OperationStateRunning, HasExecution: true, RegistrationAvailable: true,
				Execution: executionObservationSummary{State: AttemptAccepted, Correlation: provisioning.RequestCorrelationFound},
			}),
		},
		{
			name: "terminal operation", state: RecoveryTerminal,
			assessment: PlanOperationObserve(OperationRecoverySnapshot{OperationState: domain.OperationStateSucceeded}),
		},
		{
			name: "deleted resource", state: RecoveryUnsupportedRepair,
			assessment: PlanResourcePassiveObserve(ResourcePassiveObserveSnapshot{ResourceState: domain.ResourceStateDeleted, RegistrationAvailable: true}),
		},
		{
			name: "passive observe", state: RecoverySafePassiveObserve, action: ActionKindTriggerPassiveObserve,
			assessment: PlanResourcePassiveObserve(ResourcePassiveObserveSnapshot{ResourceState: domain.ResourceStateReady, RegistrationAvailable: true}),
		},
		{
			name: "dead dispatch maps through observable evidence", state: RecoverySafeRecoverDeadWork, action: ActionKindRecoverDeadWork,
			assessment: PlanDeadWorkRecovery(DeadWorkKindSnapshot{
				Kind: OutboxDispatch, State: OutboxDead, OperationTarget: true, OperationState: domain.OperationStateRunning,
				HasExecution: true, RegistrationAvailable: true,
				Execution: executionObservationSummary{State: AttemptAccepted, Correlation: provisioning.RequestCorrelationFound},
			}),
		},
		{
			name: "dead pre-submit dispatch is manual", state: RecoveryManualInterventionNeeded,
			assessment: PlanDeadWorkRecovery(DeadWorkKindSnapshot{
				Kind: OutboxDispatch, State: OutboxDead, OperationTarget: true, OperationState: domain.OperationStateRunning,
				HasExecution: true, RegistrationAvailable: true,
				Execution: executionObservationSummary{State: AttemptPending},
			}),
		},
		{
			name: "dead drive needs no execution", state: RecoverySafeRecoverDeadWork, action: ActionKindRecoverDeadWork,
			assessment: PlanDeadWorkRecovery(DeadWorkKindSnapshot{
				Kind: OutboxDrive, State: OutboxDead, OperationTarget: true, OperationState: domain.OperationStatePending,
				RegistrationAvailable: true,
			}),
		},
		{
			name: "resource target only permits passive observe", state: RecoveryUnsupportedRepair,
			assessment: PlanDeadWorkRecovery(DeadWorkKindSnapshot{
				Kind: OutboxDrive, State: OutboxDead, ResourceState: domain.ResourceStateReady, RegistrationAvailable: true,
			}),
		},
		{
			name: "non-dead work is never recommended", state: RecoveryNoActionNeeded,
			assessment: PlanDeadWorkRecovery(DeadWorkKindSnapshot{
				Kind: OutboxObserve, State: OutboxPending, OperationTarget: true, RegistrationAvailable: true,
			}),
		},
		{
			name: "passive recovery yields to active operation", state: RecoveryNoActionNeeded,
			assessment: PlanDeadWorkRecovery(DeadWorkKindSnapshot{
				Kind: OutboxPassiveObserve, State: OutboxDead, ResourceState: domain.ResourceStateReady,
				ActiveOperation: true, RegistrationAvailable: true,
			}),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.assessment.State != test.state {
				t.Fatalf("state = %q, want %q", test.assessment.State, test.state)
			}
			if test.action == "" {
				if len(test.assessment.AllowedActions) != 0 {
					t.Fatalf("allowed actions = %v, want none", test.assessment.AllowedActions)
				}
				return
			}
			if len(test.assessment.AllowedActions) != 1 || test.assessment.AllowedActions[0] != test.action {
				t.Fatalf("allowed actions = %v, want [%s]", test.assessment.AllowedActions, test.action)
			}
		})
	}
}

func TestDriveMessagesCarryNoDecisionBearingPayload(t *testing.T) {
	messages := []OutboxMessage{
		DriveMessage("operation", 4),
		operatorDriveMessage("action", "operation", 4),
	}
	for _, message := range messages {
		if message.Kind != OutboxDrive || string(message.Payload) != `{}` || message.PayloadVersion != 1 {
			t.Fatalf("unsafe Drive message: %+v", message)
		}
	}
}

func TestDiagnosticRevisionRequiresExactStrongETag(t *testing.T) {
	current := "diag_v1_value"
	tests := map[string]bool{
		"":                     true,
		`"diag_v1_value"`:      true,
		"diag_v1_value":        false,
		`W/"diag_v1_value"`:    false,
		`"diag_v1_value", "x"`: false,
		"*":                    false,
		`""diag_v1_value""`:    false,
		` "diag_v1_value"`:     false,
	}
	for value, want := range tests {
		if got := matchDiagnosticRevision(value, current); got != want {
			t.Errorf("matchDiagnosticRevision(%q) = %t, want %t", value, got, want)
		}
	}
}

// TestOperationRevisionIgnoresIrrelevantHistory pins the bounded-diagnostic
// contract: appending historical rows that cannot change recovery safety must
// not alter the diagnostic revision, and the planner outcome must be
// identical regardless of how much irrelevant history exists.
func TestOperationRevisionIgnoresIrrelevantHistory(t *testing.T) {
	operation := OperationRecord{Version: 7}
	execution := ExecutionDiagnostics{State: AttemptAccepted, Correlation: "Found"}
	active := WorkHistorySummary{
		Active: []OutboxMessage{{ID: "observe:op:3", Kind: OutboxObserve, State: OutboxLeased, AttemptCount: 1}},
	}
	baseAttempts := AttemptHistorySummary{Count: 1250, Latest: SubmissionAttemptRecord{AttemptNumber: 2500, State: "Rejected"}}
	base := operationRevision(operation, baseAttempts, active, &execution, true)

	gapFilled := AttemptHistorySummary{Count: 1251, Latest: baseAttempts.Latest}
	if operationRevision(operation, gapFilled, active, &execution, true) != base {
		t.Fatal("revision changed when history grew without moving the latest attempt")
	}
	movedLatest := AttemptHistorySummary{Count: 10000, Latest: SubmissionAttemptRecord{AttemptNumber: 20000, State: "Rejected"}}
	if operationRevision(operation, movedLatest, active, &execution, true) == base {
		t.Fatal("revision failed to change when the latest attempt moved")
	}

	grownCounts := WorkHistorySummary{
		Active: active.Active,
		Counts: map[OutboxState]int{OutboxCompleted: 10000},
	}
	if operationRevision(operation, movedLatest, grownCounts, &execution, true) !=
		operationRevision(operation, movedLatest, WorkHistorySummary{Active: active.Active}, &execution, true) {
		t.Fatal("revision changed when only completed-history counts grew")
	}

	changedActive := WorkHistorySummary{
		Active: []OutboxMessage{{ID: "observe:op:4", Kind: OutboxObserve, State: OutboxPending}},
	}
	if operationRevision(operation, movedLatest, grownCounts, &execution, true) ==
		operationRevision(operation, movedLatest, changedActive, &execution, true) {
		t.Fatal("revision failed to change when the active work set changed")
	}
}
