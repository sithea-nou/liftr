// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"

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
					return finishJSON(cmd, emitJSON(a.stdout, list.Raw))
				}
				a.renderResourceTypeListText(a.stdout, list)
				return nil
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
					return finishJSON(cmd, emitJSON(a.stdout, detail.Raw))
				}
				a.renderResourceTypeDetailText(a.stdout, detail)
				return nil
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

func finishJSON(cmd *cobra.Command, err error) error {
	if err != nil {
		return exit(ExitFailure)
	}
	return nil
}
