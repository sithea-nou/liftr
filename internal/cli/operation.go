// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/sithea-nou/liftr/internal/client"
)

func newOperationCommand(a *App) *cobra.Command {
	command := &cobra.Command{
		Use:   "operation",
		Short: "Inspect and retry lifecycle Operations",
	}
	command.AddCommand(
		newOperationGetCommand(a),
		newOperationListCommand(a),
		newOperationRetryCommand(a),
	)
	return command
}

func newOperationGetCommand(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "get OPERATION_ID",
		Short: "Read one lifecycle Operation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.prepare(); err != nil {
				return err
			}
			operation, err := a.api.GetOperation(cmd.Context(), args[0])
			if err != nil {
				return exit(classifyInterrupted(cmd.Context(), a.reportReadFailure(err)))
			}
			return a.finishOperationOutput(cmd, operation)
		},
	}
}

type operationListOptions struct {
	resourceID string
	limit      int
	cursor     string
}

func newOperationListCommand(a *App) *cobra.Command {
	options := &operationListOptions{}
	command := &cobra.Command{
		Use:   "list --resource ID",
		Short: "List one page of lifecycle Operations for a Resource",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("limit") && (options.limit < 1 || options.limit > 100) {
				return fmt.Errorf("--limit must be an integer between 1 and 100")
			}
			if err := a.prepare(); err != nil {
				return err
			}
			list, err := a.api.ListResourceOperations(cmd.Context(), options.resourceID, options.limit, options.cursor)
			if err != nil {
				return exit(classifyInterrupted(cmd.Context(), a.reportReadFailure(err)))
			}
			if a.output == outputJSON {
				if err := emitJSON(a.stdout, list.Raw); err != nil {
					fmt.Fprintf(a.stderr, "error: %s\n", a.clean(err.Error()))
					return exit(ExitFailure)
				}
			} else {
				a.renderOperationListText(a.stdout, list)
			}
			if list.NextCursor != "" {
				fmt.Fprintf(a.stderr, "next page: liftr operation list --resource %s", a.clean(options.resourceID))
				if cmd.Flags().Changed("limit") {
					fmt.Fprintf(a.stderr, " --limit %d", options.limit)
				}
				fmt.Fprintf(a.stderr, " --cursor %s\n", a.clean(list.NextCursor))
			}
			return nil
		},
	}
	command.Flags().StringVar(&options.resourceID, "resource", "", "Resource ID whose Operation history to list")
	command.Flags().IntVar(&options.limit, "limit", 0, "maximum Operations to return (1-100; server default when omitted)")
	command.Flags().StringVar(&options.cursor, "cursor", "", "opaque continuation cursor returned by the server")
	_ = command.MarkFlagRequired("resource")
	return command
}

type operationRetryOptions struct {
	generation     uint64
	idempotencyKey string
	wait           bool
	timeout        time.Duration
}

func newOperationRetryCommand(a *App) *cobra.Command {
	options := &operationRetryOptions{}
	command := &cobra.Command{
		Use:   "retry OPERATION_ID",
		Short: "Admit a new Operation retrying a failed Operation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("generation") && options.generation == 0 {
				return fmt.Errorf("--generation must be greater than zero")
			}
			if err := a.prepare(); err != nil {
				return err
			}
			if err := validateTimeout(options.timeout); err != nil {
				return err
			}
			operationID := args[0]
			generation := options.generation
			if !cmd.Flags().Changed("generation") {
				source, err := a.api.GetOperation(cmd.Context(), operationID)
				if err != nil {
					return exit(classifyInterrupted(cmd.Context(), a.reportReadFailure(err)))
				}
				resource, err := a.api.GetResource(cmd.Context(), source.ResourceID)
				if err != nil {
					return exit(classifyInterrupted(cmd.Context(), a.reportReadFailure(err)))
				}
				generation = resource.Generation
				if a.output == outputText {
					fmt.Fprintf(a.stderr, "preconditioning retry on current generation %d\n", generation)
				}
			}
			key, err := resolveIdempotencyKey(options.idempotencyKey)
			if err != nil {
				return err
			}
			result, err := a.api.RetryOperation(cmd.Context(), operationID, key, generation)
			if err != nil {
				if interruptedNow(cmd.Context()) {
					return exit(ExitInterrupted)
				}
				return exit(a.reportMutationFailure(key, err))
			}
			return exit(a.finishOperationRetry(cmd, result, options))
		},
	}
	command.Flags().Uint64Var(&options.generation, "generation", 0, "concrete If-Liftr-Generation precondition; skips source and Resource pre-reads")
	command.Flags().StringVar(&options.idempotencyKey, "idempotency-key", "", "replayable idempotency key (a fresh random key is generated when omitted)")
	command.Flags().BoolVar(&options.wait, "wait", false, "wait for the admitted child Operation to reach a terminal state")
	command.Flags().DurationVar(&options.timeout, "timeout", DefaultWaitTimeout, "how long --wait waits before giving up")
	return command
}

func (a *App) finishOperationOutput(cmd *cobra.Command, operation *client.Operation) error {
	if a.output == outputJSON {
		return finishJSON(cmd, emitJSON(a.stdout, operation.Raw))
	}
	a.renderOperationText(a.stdout, operation)
	return nil
}

func (a *App) finishOperationRetry(cmd *cobra.Command, result *client.MutationResult, options *operationRetryOptions) int {
	if result != nil && result.Replay {
		fmt.Fprintln(a.stderr, "note: the server reports that this response replays an earlier admission under this idempotency key")
	}
	if !options.wait {
		if result == nil || result.Operation == nil || result.Operation.ID == "" {
			fmt.Fprintln(a.stderr, "error: retry admission omitted its Operation")
			return ExitFailure
		}
		if _, monitorErr := a.retryMonitorOperationID(result); monitorErr != nil {
			fmt.Fprintf(a.stderr, "warning: retry admission carries unusable monitor metadata: %s\n", a.clean(monitorErr.Error()))
		}
		if a.output == outputText {
			fmt.Fprintf(a.stderr, "monitor with: liftr operation get %s\n", a.clean(result.Operation.ID))
		}
		if err := a.finishOperationOutput(cmd, result.Operation); err != nil {
			fmt.Fprintf(a.stderr, "error: %s\n", a.clean(err.Error()))
			return ExitFailure
		}
		return ExitOK
	}
	if _, monitorErr := a.retryMonitorOperationID(result); monitorErr != nil {
		fmt.Fprintf(a.stderr, "error: retry admission protocol failure: %s\n", a.clean(monitorErr.Error()))
		return ExitFailure
	}
	return a.waitForRetryOperation(cmd.Context(), result, options.timeout)
}

func (a *App) retryMonitorOperationID(result *client.MutationResult) (string, error) {
	if result == nil || result.Operation == nil || result.Operation.ID == "" {
		return "", fmt.Errorf("retry admission omitted its Operation")
	}
	operationID, err := a.api.MonitorOperationID(result)
	if err != nil {
		return "", err
	}
	if operationID != result.Operation.ID {
		return "", fmt.Errorf("monitor reference identifies Operation %q, but the response contains %q", operationID, result.Operation.ID)
	}
	return operationID, nil
}
