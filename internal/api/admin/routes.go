// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/identity"
)

func (h *handler) resourceDiagnostics(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	id := domain.ResourceID(r.PathValue("id"))
	if err := h.service.AuthorizeOperatorAction(r.Context(), principal, identity.ActionOperatorDiagnosticsRead,
		identity.OperatorTarget{Kind: identity.OperatorTargetResource, ID: string(id)}); err != nil {
		h.mapError(w, r, err, codeResourceNotFound)
		return
	}
	diag, err := h.service.ResourceOperatorDiagnostics(r.Context(), principal, id)
	if err != nil {
		h.mapError(w, r, err, codeResourceNotFound)
		return
	}
	body := resourceDiagnosticDTO{
		ResourceID: string(diag.ResourceID), ResourceType: diag.TypeName, ResourceVersion: diag.TypeVersion,
		OwnerKind: diag.OwnerKind, OwnerID: diag.OwnerID, Generation: diag.Generation,
		ObservedGeneration: diag.ObservedGeneration, State: string(diag.State), StatusUpdatedAt: diag.StatusUpdatedAt,
		ActiveOperation: operationRefOf(diag.ActiveOperation), LatestOperation: operationRefOf(diag.LatestOperation),
		OperationAgeSeconds: diag.OperationAgeSeconds, ReconciliationSilenceSeconds: diag.ReconciliationSilenceSeconds,
		OutputResolution: string(diag.OutputResolution), ProvisionerRef: string(diag.ProvisionerRef),
		ProvisionerKind: h.provisionerKinds[diag.ProvisionerRef], RegistrationAvailable: diag.RegistrationAvailable,
		StateIdentity: stateIdentityOf(diag.StateIdentity), Assessment: assessmentOf(diag.Assessment),
	}
	if diag.SpecDigestAvailable {
		body.SpecDigest = diag.SpecDigest
	}
	h.writeDiagnostic(w, diag.Revision, body)
	h.observeRequest("diagnostics_read", "read")
}

func (h *handler) operationDiagnostics(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	id := domain.OperationID(r.PathValue("id"))
	if err := h.service.AuthorizeOperatorAction(r.Context(), principal, identity.ActionOperatorDiagnosticsRead,
		identity.OperatorTarget{Kind: identity.OperatorTargetOperation, ID: string(id)}); err != nil {
		h.mapError(w, r, err, codeOperationNotFound)
		return
	}
	diag, err := h.service.OperationOperatorDiagnostics(r.Context(), principal, id)
	if err != nil {
		h.mapError(w, r, err, codeOperationNotFound)
		return
	}
	body := operationDiagnosticDTO{
		OperationID: string(diag.OperationID), ResourceID: string(diag.ResourceID), Capability: string(diag.Capability),
		TargetGeneration: diag.TargetGeneration, State: string(diag.State), Phase: string(diag.Phase),
		RetryOf: string(diag.RetryOf), RequestedAt: diag.RequestedAt, StartedAt: diag.StartedAt,
		CompletedAt: diag.CompletedAt, AgeSeconds: diag.AgeSeconds,
		ProvisionerRef: string(diag.ProvisionerRef), RegistrationAvailable: diag.RegistrationAvailable,
		Assessment:          assessmentOf(diag.Assessment),
		AttemptCount:        diag.AttemptCount,
		ActiveWork:          []workRefDTO{},
		ActiveWorkTruncated: diag.ActiveWorkTruncated,
		WorkCount:           diag.WorkCount,
	}
	if diag.LatestAttempt != nil {
		attempt := *diag.LatestAttempt
		body.LatestAttempt = &attemptDTO{
			Number: attempt.Number, State: string(attempt.State), BoundaryCrossed: attempt.BoundaryCrossed,
			ClaimedAt: attempt.ClaimedAt, ResolvedAt: attempt.ResolvedAt, FailureKind: attempt.FailureKind,
		}
	}
	if diag.Execution != nil {
		body.Execution = &executionDTO{
			State: string(diag.Execution.State), Correlation: diag.Execution.Correlation,
			AcceptanceConfirmed: diag.Execution.AcceptanceConfirmed, HandlePresent: diag.Execution.HandlePresent,
			OutputRecovery: diag.Execution.IsOutputRecovery, OutputResolution: string(diag.Execution.OutputResolution),
			OutputFailureKind: diag.Execution.OutputFailureKind, CurrentAttempt: diag.Execution.CurrentAttempt,
			NextObservationSequence: diag.Execution.NextObservationSequence,
		}
	}
	for _, work := range diag.ActiveWork {
		body.ActiveWork = append(body.ActiveWork, workRefDTO{
			ID: work.ID, Kind: string(work.Kind), State: string(work.State), CreatedAt: work.CreatedAt,
			AvailableAt: work.AvailableAt, AttemptCount: work.AttemptCount,
		})
	}
	h.writeDiagnostic(w, diag.Revision, body)
	h.observeRequest("diagnostics_read", "read")
}

func (h *handler) workDiagnostics(w http.ResponseWriter, r *http.Request) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if err := h.service.AuthorizeOperatorAction(r.Context(), principal, identity.ActionOperatorDiagnosticsRead,
		identity.OperatorTarget{Kind: identity.OperatorTargetWork, ID: id}); err != nil {
		h.mapError(w, r, err, codeWorkNotFound)
		return
	}
	diag, err := h.service.WorkOperatorDiagnostics(r.Context(), principal, id)
	if err != nil {
		h.mapError(w, r, err, codeWorkNotFound)
		return
	}
	body := workDiagnosticDTO{
		WorkID: diag.WorkID, Kind: string(diag.Kind), State: string(diag.State),
		OperationID: string(diag.OperationID), ResourceID: string(diag.ResourceID), AttemptNumber: diag.AttemptNumber,
		CreatedAt: diag.CreatedAt, AvailableAt: diag.AvailableAt, AttemptCount: diag.AttemptCount,
		LeaseActive: diag.LeaseActive, LeaseExpired: diag.LeaseExpired,
		TerminalReasonClass: diag.TerminalReasonClass, TargetTerminal: diag.TargetTerminal,
		ActiveEquivalentWork: diag.ActiveEquivalentWork, Superseded: diag.Superseded,
		Assessment: assessmentOf(diag.Assessment),
	}
	h.writeDiagnostic(w, diag.Revision, body)
	h.observeRequest("diagnostics_read", "read")
}

func (h *handler) triggerObserve(w http.ResponseWriter, r *http.Request) {
	id := domain.OperationID(r.PathValue("id"))
	h.mutate(w, r, identity.ActionOperatorObserveTrigger, identity.OperatorTargetOperation, string(id), "trigger_observe", codeOperationNotFound,
		func(principal identity.Principal, cmd application.OperatorMutationCommand) (application.OperatorMutationResult, error) {
			return h.service.TriggerOperationObservation(r.Context(), cmd, id)
		})
}

func (h *handler) triggerPassiveObserve(w http.ResponseWriter, r *http.Request) {
	id := domain.ResourceID(r.PathValue("id"))
	h.mutate(w, r, identity.ActionOperatorObserveTrigger, identity.OperatorTargetResource, string(id), "trigger_passive_observe", codeResourceNotFound,
		func(principal identity.Principal, cmd application.OperatorMutationCommand) (application.OperatorMutationResult, error) {
			return h.service.TriggerResourcePassiveObservation(r.Context(), cmd, id)
		})
}

func (h *handler) recoverDeadWork(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h.mutate(w, r, identity.ActionOperatorWorkRecover, identity.OperatorTargetWork, id, "recover_dead_work", codeWorkNotFound,
		func(principal identity.Principal, cmd application.OperatorMutationCommand) (application.OperatorMutationResult, error) {
			return h.service.RecoverDeadWork(r.Context(), cmd, id)
		})
}

func (h *handler) mutate(w http.ResponseWriter, r *http.Request, permission identity.Action,
	targetKind identity.OperatorTargetKind, targetID, metricAction, missingCode string,
	apply func(identity.Principal, application.OperatorMutationCommand) (application.OperatorMutationResult, error),
) {
	principal, ok := h.requirePrincipal(w, r)
	if !ok {
		return
	}
	if h.draining() {
		writeProblem(w, r, codeAdminDraining, "the operator plane is shutting down", nil)
		return
	}
	if err := h.service.AuthorizeOperatorAction(r.Context(), principal, permission, identity.OperatorTarget{Kind: targetKind, ID: targetID}); err != nil {
		h.observeRequest(metricAction, "denied")
		h.mapError(w, r, err, missingCode)
		return
	}
	if !emptyBody(r) {
		h.observeRequest(metricAction, "error")
		writeProblem(w, r, codeInvalidArgument, "operator mutation requests have no body", nil)
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		h.observeRequest(metricAction, "error")
		writeProblem(w, r, codePreconditionRequired, "Idempotency-Key is required", nil)
		return
	}
	if len(key) > 200 {
		h.observeRequest(metricAction, "error")
		writeProblem(w, r, codeInvalidArgument, "Idempotency-Key exceeds 200 bytes", nil)
		return
	}
	result, err := apply(principal, application.OperatorMutationCommand{
		Actor: principal, IdempotencyKey: key, RequestID: requestID(r.Context()), IfMatch: r.Header.Get("If-Match"),
	})
	if err != nil {
		h.observeRequest(metricAction, requestOutcome(err))
		h.mapError(w, r, err, missingCode)
		return
	}
	outcome := "applied"
	if result.Replay {
		outcome = "replayed"
		w.Header().Set("Idempotency-Replayed", "true")
	}
	body := mutationDTO{Result: outcome, Action: string(result.Action), TargetKind: string(result.TargetKind),
		TargetID: result.TargetID, OperatorActionID: result.OperatorActionID, CreatedWorkID: result.CreatedWorkID}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(body)
	h.observeRequest(metricAction, outcome)
	if result.SourceWorkKind != "" && h.telemetry != nil {
		h.telemetry.OperatorRecovery(string(result.SourceWorkKind), outcome)
	}
	if h.logger != nil {
		h.logger.Info("operator mutation accepted", "request_id", requestID(r.Context()), "operator_principal_id", principal.ID,
			"operator_principal_kind", principal.Kind, "action", result.Action, "target_kind", result.TargetKind,
			"target_id", result.TargetID, "operator_action_id", result.OperatorActionID, "result", outcome)
	}
}

func (h *handler) requirePrincipal(w http.ResponseWriter, r *http.Request) (identity.Principal, bool) {
	if h.service == nil {
		writeProblem(w, r, codePersistenceUnavailable, "durable operator state is unavailable", nil)
		return identity.Principal{}, false
	}
	value, ok := principal(r.Context())
	if !ok {
		writeUnauthenticated(w, r, false)
		return identity.Principal{}, false
	}
	return value, true
}

func (h *handler) writeDiagnostic(w http.ResponseWriter, revision string, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("ETag", `"`+revision+`"`)
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}

func (h *handler) mapError(w http.ResponseWriter, r *http.Request, err error, missingCode string) {
	switch {
	case errors.Is(err, application.ErrNotAuthorized):
		writeProblem(w, r, codeOperatorForbidden, "the operator principal is not authorized for this action", nil)
	case errors.Is(err, application.ErrIdempotencyConflict):
		writeProblem(w, r, codeOperatorIdempotencyConflict, "this Idempotency-Key is bound to a different operator request", nil)
	case errors.Is(err, application.ErrDiagnosticStale), errors.Is(err, application.ErrConcurrencyConflict):
		writeProblem(w, r, codeDiagnosticStale, "durable diagnostic state changed; read it again before retrying", nil)
	case errors.Is(err, application.ErrRecoveryAlreadyActive):
		var active *application.RecoveryAlreadyActiveError
		errors.As(err, &active)
		ext := &problem{}
		if active != nil {
			ext.ExistingWorkID = active.ExistingWorkID
		}
		writeProblem(w, r, codeRecoveryAlreadyActive, "equivalent recovery work is already active", ext)
	case errors.Is(err, application.ErrActionNotApplicable):
		writeProblem(w, r, codeActionNotApplicable, "the action is not applicable to current durable state", nil)
	case errors.Is(err, application.ErrRecoveryUnsafe):
		writeProblem(w, r, codeRecoveryUnsafe, "Liftr cannot safely automate this recovery", nil)
	case errors.Is(err, application.ErrInvalidApplicationCall):
		writeProblem(w, r, codeInvalidArgument, "the operator request is invalid", nil)
	case errors.Is(err, application.ErrResourceNotFound), errors.Is(err, application.ErrOperationNotFound):
		if missingCode == "" {
			missingCode = codeWorkNotFound
		}
		writeProblem(w, r, missingCode, "no durable target exists with this ID", nil)
	default:
		writeProblem(w, r, codePersistenceUnavailable, "the operator plane cannot currently reach durable state", nil)
	}
}

func emptyBody(r *http.Request) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1025))
	return err == nil && len(body) <= 1024 && strings.TrimSpace(string(body)) == ""
}

func requestOutcome(err error) string {
	switch {
	case errors.Is(err, application.ErrDiagnosticStale), errors.Is(err, application.ErrConcurrencyConflict):
		return "stale"
	case errors.Is(err, application.ErrActionNotApplicable):
		return "not_applicable"
	case errors.Is(err, application.ErrRecoveryUnsafe):
		return "unsafe"
	case errors.Is(err, application.ErrRecoveryAlreadyActive), errors.Is(err, application.ErrIdempotencyConflict):
		return "conflict"
	case errors.Is(err, application.ErrNotAuthorized):
		return "denied"
	default:
		return "error"
	}
}

func (h *handler) observeRequest(action, result string) {
	if h.telemetry != nil {
		h.telemetry.OperatorRequest(action, result)
	}
}
func (h *handler) draining() bool { return h.drainCheck != nil && h.drainCheck() }
