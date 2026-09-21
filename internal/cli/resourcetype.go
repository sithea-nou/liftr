// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func newResourceTypeCommand(a *App) *cobra.Command {
	command := &cobra.Command{
		Use:   "resource-type",
		Short: "Discover ResourceType contracts",
	}
	command.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List registered ResourceType contracts",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				if err := a.prepare(); err != nil {
					return err
				}
				list, err := a.api.ListResourceTypes(cmd.Context())
				if err != nil {
					return exit(classifyInterrupted(cmd.Context(), a.reportReadFailure(err)))
				}
				if a.output == outputJSON {
					return finishOutput(a, emitJSON(a.stdout, list.Raw))
				}
				return finishOutput(a, a.renderResourceTypeListText(a.stdout, list))
			},
		},
		&cobra.Command{
			Use:   "get NAME VERSION",
			Short: "Read one ResourceType contract including its spec schema",
			Args:  cobra.ExactArgs(2),
			RunE: func(cmd *cobra.Command, args []string) error {
				if err := a.prepare(); err != nil {
					return err
				}
				detail, err := a.api.GetResourceType(cmd.Context(), args[0], args[1])
				if err != nil {
					return exit(classifyInterrupted(cmd.Context(), a.reportReadFailure(err)))
				}
				if a.output == outputJSON {
					return finishOutput(a, emitJSON(a.stdout, detail.Raw))
				}
				return finishOutput(a, a.renderResourceTypeDetailText(a.stdout, detail))
			},
		},
	)
	return command
}

// classifyInterrupted lets cancellation take precedence over any other
// classification so Ctrl-C always maps to the interrupted exit code.
func classifyInterrupted(ctx context.Context, fallback int) int {
	if ctx.Err() != nil {
		return ExitInterrupted
	}
	return fallback
}

// interruptedNow reports pure cancellation, for paths that must not print
// mutation-failure guidance after Ctrl-C.
func interruptedNow(ctx context.Context) bool {
	return ctx.Err() != nil
}

func finishOutput(a *App, err error) error {
	if err != nil {
		_, _ = fmt.Fprintf(a.stderr, "error: %s\n", a.clean(err.Error()))
		return exit(ExitFailure)
	}
	return nil
}
