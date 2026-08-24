// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/sithea-nou/liftr/internal/client"
)

const (
	// maxConsecutivePollFailures tolerates transient control-plane trouble
	// during a wait before declaring the invocation failed.
	maxConsecutivePollFailures = 5

	// DefaultWaitTimeout bounds --wait when the flag is not given.
	DefaultWaitTimeout = 10 * time.Minute

	// MaxWaitTimeout caps --timeout to keep deadline arithmetic sane.
	MaxWaitTimeout = 24 * time.Hour
)

// pollInterval is deliberately modest: no aggressive hammering, no adaptive
// backoff machinery in M12. It is a variable so tests can shorten waits.
var pollInterval = 2 * time.Second

func jittered(d time.Duration) time.Duration {
	spread := d / 5
	if spread <= 0 {
		return d
	}
	return d - spread + time.Duration(rand.Int64N(int64(2*spread)))
}

// reportReadFailure classifies and prints any ordinary read failure,
// returning the matching exit code.
func (a *App) reportReadFailure(err error) int {
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		a.renderProblem(apiErr)
		if apiErr.IsAuthentication() {
			return ExitAuth
		}
		return ExitRejected
	}
	fmt.Fprintf(a.stderr, "error: %s\n", a.clean(err.Error()))
	return ExitFailure
}

// reportFinalReadFailure implements the pinned wait semantics for "Operation
// succeeded but the final Resource read failed": the Operation did NOT fail,
// so this is never exit 5 — it is a command/read failure (exit 1, or exit 3
// when authentication failed). Nothing stale is emitted as a final snapshot.
func (a *App) reportFinalReadFailure(operationID string, err error) int {
	fmt.Fprintf(a.stderr, "Operation %s succeeded, but the final Resource could not be retrieved.\n", operationID)
	var apiErr *client.APIError
	if errors.As(err, &apiErr) && apiErr.IsAuthentication() {
		a.renderProblem(apiErr)
		return ExitAuth
	}
	var generic *client.APIError
	if errors.As(err, &generic) {
		fmt.Fprintf(a.stderr, "%s\n", a.clean(generic.Error()))
	} else {
		fmt.Fprintf(a.stderr, "error: %s\n", a.clean(err.Error()))
	}
	return ExitFailure
}

// reportMutationFailure adds idempotency guidance for transport failures on
// mutations: the outcome is unknown, so only a replay under the same key is
// safe. The key itself is printed once here — an identifier, never a
// credential.
func (a *App) reportMutationFailure(idempotencyKey string, err error) int {
	var terr *client.TransportError
	if errors.As(err, &terr) && terr.OutcomeUnknown {
		fmt.Fprintf(a.stderr, "error: %s\n", a.clean(terr.Error()))
		fmt.Fprintf(a.stderr, "The outcome of this request could not be determined.\n")
		fmt.Fprintf(a.stderr, "To resolve it safely, re-run this command with --idempotency-key %s to replay the identical request.\n", idempotencyKey)
		return ExitFailure
	}
	return a.reportReadFailure(err)
}

func (a *App) outputResource(resource *client.Resource) error {
	if a.output == outputJSON {
		return emitJSON(a.stdout, resource.Raw)
	}
	a.renderResourceText(a.stdout, resource)
	return nil
}

// waitForOperation follows exactly the authoritative monitor Operation of
// one admission until a terminal state or the timeout. There is no fallback
// to Resource.latestOperation and no inference from Resource state: the
// Operation endpoint is the authoritative mutation result.
//
// Terminal semantics are pinned:
//   - Succeeded + successful final Resource read -> emit Resource, exit 0
//   - Succeeded but final read fails -> exit 1 (3 for auth); the Operation
//     did NOT fail; nothing stale is emitted as a final snapshot
//   - Failed/Canceled/timeout -> exit 5
//   - authentication failure mid-poll -> exit 3
//   - unexpected Operation absence -> protocol failure, exit 1
func (a *App) waitForOperation(ctx context.Context, admission *client.MutationResult, timeout time.Duration) int {
	operationID, err := a.api.MonitorOperationID(admission)
	if err != nil {
		fmt.Fprintf(a.stderr, "error: cannot determine the admitted Operation: %s\n", a.clean(err.Error()))
		return ExitFailure
	}
	if a.output == outputText {
		fmt.Fprintf(a.stderr, "waiting for operation %s (timeout %s)\n", operationID, timeout)
	}

	deadline := time.Now().Add(timeout)
	consecutiveFailures := 0
	lastState := ""
	for {
		operation, err := a.api.GetOperation(ctx, operationID)
		if ctx.Err() != nil {
			return ExitInterrupted
		}
		if err == nil {
			consecutiveFailures = 0
			if operation.State != lastState {
				if a.output == outputText && lastState != "" {
					fmt.Fprintf(a.stderr, "operation %s: %s\n", a.clean(operationID), a.clean(operation.State))
				}
				lastState = operation.State
			}
			switch operation.State {
			case client.StateSucceeded:
				resource, err := a.api.GetResource(ctx, operation.ResourceID)
				if ctx.Err() != nil {
					return ExitInterrupted
				}
				if err != nil {
					return a.reportFinalReadFailure(operationID, err)
				}
				if err := a.outputResource(resource); err != nil {
					fmt.Fprintf(a.stderr, "error: %s\n", a.clean(err.Error()))
					return ExitFailure
				}
				return ExitOK
			case client.StateFailed, client.StateCanceled:
				a.renderTerminalFailure(operation)
				return ExitOperationFailed
			}
		} else {
			var apiErr *client.APIError
			if errors.As(err, &apiErr) {
				if apiErr.IsAuthentication() {
					return a.reportReadFailure(err)
				}
				if apiErr.HasCode(client.CodeOperationNotFound) {
					fmt.Fprintf(a.stderr, "error: operation %s unexpectedly disappeared while waiting; this is a protocol violation\n", a.clean(operationID))
					return ExitFailure
				}
			}
			consecutiveFailures++
			if consecutiveFailures >= maxConsecutivePollFailures {
				fmt.Fprintf(a.stderr, "error: waiting for operation %s failed after repeated request failures: %s\n", a.clean(operationID), a.clean(err.Error()))
				return ExitFailure
			}
		}

		sleep := jittered(pollInterval)
		if remaining := time.Until(deadline); remaining <= 0 {
			a.renderWaitTimeout(operationID, timeout)
			return ExitOperationFailed
		} else if sleep > remaining {
			sleep = remaining
		}
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ExitInterrupted
		case <-timer.C:
		}
	}
}

// waitForRetryOperation follows only the child Operation admitted by retry.
// Unlike Resource mutation waits, every terminal result is the Operation
// itself and no Resource snapshot is read.
func (a *App) waitForRetryOperation(ctx context.Context, admission *client.MutationResult, timeout time.Duration) int {
	operationID, err := a.retryMonitorOperationID(admission)
	if err != nil {
		fmt.Fprintf(a.stderr, "error: cannot determine the admitted Operation: %s\n", a.clean(err.Error()))
		return ExitFailure
	}
	if a.output == outputText {
		fmt.Fprintf(a.stderr, "waiting for operation %s (timeout %s)\n", a.clean(operationID), timeout)
	}

	deadline := time.Now().Add(timeout)
	consecutiveFailures := 0
	lastState := ""
	for {
		operation, err := a.api.GetOperation(ctx, operationID)
		if ctx.Err() != nil {
			return ExitInterrupted
		}
		if err == nil {
			consecutiveFailures = 0
			if operation.State != lastState {
				if a.output == outputText && lastState != "" {
					fmt.Fprintf(a.stderr, "operation %s: %s\n", a.clean(operationID), a.clean(operation.State))
				}
				lastState = operation.State
			}
			switch operation.State {
			case client.StateSucceeded:
				if err := a.outputOperation(operation); err != nil {
					fmt.Fprintf(a.stderr, "error: %s\n", a.clean(err.Error()))
					return ExitFailure
				}
				return ExitOK
			case client.StateFailed, client.StateCanceled:
				if err := a.outputOperation(operation); err != nil {
					fmt.Fprintf(a.stderr, "error: %s\n", a.clean(err.Error()))
					return ExitFailure
				}
				a.renderTerminalFailure(operation)
				return ExitOperationFailed
			}
		} else {
			var apiErr *client.APIError
			if errors.As(err, &apiErr) {
				if apiErr.IsAuthentication() {
					return a.reportReadFailure(err)
				}
				if apiErr.HasCode(client.CodeOperationNotFound) {
					fmt.Fprintf(a.stderr, "error: operation %s unexpectedly disappeared while waiting; this is a protocol violation\n", a.clean(operationID))
					return ExitFailure
				}
			}
			consecutiveFailures++
			if consecutiveFailures >= maxConsecutivePollFailures {
				fmt.Fprintf(a.stderr, "error: waiting for operation %s failed after repeated request failures: %s\n", a.clean(operationID), a.clean(err.Error()))
				return ExitFailure
			}
		}

		sleep := jittered(pollInterval)
		if remaining := time.Until(deadline); remaining <= 0 {
			a.renderWaitTimeout(operationID, timeout)
			return ExitOperationFailed
		} else if sleep > remaining {
			sleep = remaining
		}
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ExitInterrupted
		case <-timer.C:
		}
	}
}

func (a *App) outputOperation(operation *client.Operation) error {
	if a.output == outputJSON {
		return emitJSON(a.stdout, operation.Raw)
	}
	a.renderOperationText(a.stdout, operation)
	return nil
}

func (a *App) renderTerminalFailure(operation *client.Operation) {
	state := "failed"
	if operation.State == client.StateCanceled {
		state = "was canceled"
	}
	fmt.Fprintf(a.stderr, "operation %s %s\n", a.clean(operation.ID), state)
	if operation.Failure != nil {
		if operation.Failure.Reason != "" {
			fmt.Fprintf(a.stderr, "reason: %s\n", a.clean(operation.Failure.Reason))
		}
		if operation.Failure.Message != "" {
			fmt.Fprintf(a.stderr, "%s\n", a.clean(operation.Failure.Message))
		}
	}
	fmt.Fprintf(a.stderr, "inspect with: liftr operation get %s\n", a.clean(operation.ID))
	fmt.Fprintf(a.stderr, "resource state: liftr resource get %s\n", a.clean(operation.ResourceID))
}

func (a *App) renderWaitTimeout(operationID string, timeout time.Duration) {
	id := a.clean(operationID)
	fmt.Fprintf(a.stderr, "error: timed out after %s waiting for operation %s; it may still complete — check with: liftr operation get %s\n",
		timeout, id, id)
}
