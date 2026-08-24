// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sithea-nou/liftr/internal/client"
)

func newResourceCommand(a *App) *cobra.Command {
	command := &cobra.Command{
		Use:   "resource",
		Short: "Create, inspect, update, and delete Resources",
	}
	command.AddCommand(
		newResourceCreateCommand(a),
		newResourceGetCommand(a),
		newResourceListCommand(a),
		newResourceUpdateCommand(a),
		newResourceDeleteCommand(a),
	)
	return command
}

// resourceListOptions carries one inventory request. There is deliberately no
// --all: pages are bounded server-side and traversals continue via --cursor.
type resourceListOptions struct {
	owner          string
	typeName       string
	typeVersion    string
	state          string
	includeDeleted bool
	limit          int
	cursor         string
}

func newResourceListCommand(a *App) *cobra.Command {
	options := &resourceListOptions{}
	command := &cobra.Command{
		Use:   "list",
		Short: "List the Resources visible to the authenticated principal",
		Long: `List one page of the ownership-scoped Resource inventory.

The server returns exactly the Resources your current authorization allows
you to read, newest first, as summaries without spec or outputs. Filters only
narrow what you can already see; they never grant access. Deleted Resources
are excluded unless --include-deleted is supplied.

Pagination is cursor-based: pass the printed continuation hint back verbatim.
If your authorization changed since a cursor was issued, the server rejects
it and traversal restarts from the first page.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Flags().Changed("limit") && (options.limit < 1 || options.limit > 100) {
				return fmt.Errorf("--limit must be an integer between 1 and 100")
			}
			if options.typeVersion != "" && options.typeName == "" {
				return fmt.Errorf("--version requires --type")
			}
			if options.state != "" && !validResourceListState(options.state) {
				return fmt.Errorf("--state must be one of Unknown, Pending, Ready, Deleting, Deleted, Failed")
			}
			if err := a.prepare(); err != nil {
				return err
			}
			opts := client.ResourceListOptions{Limit: options.limit, Cursor: options.cursor, IncludeDeleted: options.includeDeleted, TypeName: options.typeName, TypeVersion: options.typeVersion, State: options.state}
			if options.owner != "" {
				kind, id, err := parseOwnerRef(options.owner)
				if err != nil {
					return err
				}
				opts.OwnerKind, opts.OwnerID = kind, id
			}
			list, err := a.api.ListResources(cmd.Context(), opts)
			if err != nil {
				return exit(classifyInterrupted(cmd.Context(), a.reportReadFailure(err)))
			}
			if a.output == outputJSON {
				if err := emitJSON(a.stdout, list.Raw); err != nil {
					fmt.Fprintf(a.stderr, "error: %s\n", a.clean(err.Error()))
					return exit(ExitFailure)
				}
			} else {
				a.renderResourceListText(a.stdout, list)
			}
			if list.NextCursor != "" {
				fmt.Fprintf(a.stderr, "next page: liftr resource list")
				if options.owner != "" {
					fmt.Fprintf(a.stderr, " --owner %s", a.clean(options.owner))
				}
				if options.typeName != "" {
					fmt.Fprintf(a.stderr, " --type %s", a.clean(options.typeName))
				}
				if options.typeVersion != "" {
					fmt.Fprintf(a.stderr, " --version %s", a.clean(options.typeVersion))
				}
				if options.state != "" {
					fmt.Fprintf(a.stderr, " --state %s", a.clean(options.state))
				}
				if options.includeDeleted {
					fmt.Fprintf(a.stderr, " --include-deleted")
				}
				if cmd.Flags().Changed("limit") {
					fmt.Fprintf(a.stderr, " --limit %d", options.limit)
				}
				fmt.Fprintf(a.stderr, " --cursor %s\n", a.clean(list.NextCursor))
			}
			return nil
		},
	}
	command.Flags().StringVar(&options.owner, "owner", "", "narrow to one owner as KIND=ID (must already be within your visibility)")
	command.Flags().StringVar(&options.typeName, "type", "", "exact ResourceType name filter")
	command.Flags().StringVar(&options.typeVersion, "version", "", "exact ResourceType version filter (requires --type)")
	command.Flags().StringVar(&options.state, "state", "", "exact state filter: Unknown|Pending|Ready|Deleting|Deleted|Failed")
	command.Flags().BoolVar(&options.includeDeleted, "include-deleted", false, "include retained Deleted tombstones")
	command.Flags().IntVar(&options.limit, "limit", 0, "maximum summaries to return (1-100; server default when omitted)")
	command.Flags().StringVar(&options.cursor, "cursor", "", "opaque continuation cursor returned by the server")
	return command
}

// validResourceListState mirrors the server's public ResourceState filter
// vocabulary so obvious mistakes fail locally.
func validResourceListState(state string) bool {
	switch state {
	case "Unknown", "Pending", "Ready", "Deleting", "Deleted", "Failed":
		return true
	default:
		return false
	}
}

type mutationOptions struct {
	file           string
	id             string
	typeName       string
	typeVersion    string
	owner          string
	specFile       string
	idempotencyKey string
	wait           bool
	timeout        time.Duration
	generation     uint64
	yes            bool
}

func addWaitFlags(flags *pflag.FlagSet, m *mutationOptions) {
	flags.BoolVar(&m.wait, "wait", false, "wait for the admitted Operation to reach a terminal state")
	flags.DurationVar(&m.timeout, "timeout", DefaultWaitTimeout, "how long --wait waits before giving up")
	flags.StringVar(&m.idempotencyKey, "idempotency-key", "", "replayable idempotency key (a fresh random key is generated when omitted)")
}

func validateTimeout(timeout time.Duration) error {
	if timeout <= 0 || timeout > MaxWaitTimeout {
		return fmt.Errorf("--timeout must be a positive duration of at most %s", MaxWaitTimeout)
	}
	return nil
}

// resolveIdempotencyKey honors an explicit key or mints exactly one
// cryptographically random key per invocation.
func resolveIdempotencyKey(explicit string) (string, error) {
	key := strings.TrimSpace(explicit)
	if key == "" {
		return newIdempotencyKey(), nil
	}
	return key, nil
}

func newResourceCreateCommand(a *App) *cobra.Command {
	m := &mutationOptions{}
	command := &cobra.Command{
		Use:   "create",
		Short: "Admit an asynchronous Resource create request",
		Long: `Admit an asynchronous Resource create request through the Liftr API.

Two mutually exclusive input forms exist:

  liftr resource create -f DOCUMENT      a complete create document:
                                         {"id","type":{"name","version"},"owner":{"kind","id"},"spec"}
  liftr resource create --id ID --type NAME --version VERSION \
      --owner KIND=ID --spec SPEC_FILE   assembly form; SPEC_FILE holds the bare spec object

Use "-" as the file name to read from stdin. Input is JSON only, at most
1 MiB, exactly one JSON object; numeric literals are forwarded verbatim.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.prepare(); err != nil {
				return err
			}
			if err := validateTimeout(m.timeout); err != nil {
				return err
			}
			body, err := buildCreateBody(a, m)
			if err != nil {
				return err
			}
			key, err := resolveIdempotencyKey(m.idempotencyKey)
			if err != nil {
				return err
			}
			result, err := a.api.CreateResource(cmd.Context(), body, key)
			if err != nil {
				if interruptedNow(cmd.Context()) {
					return exit(ExitInterrupted)
				}
				return exit(a.reportMutationFailure(key, err))
			}
			return exit(a.finishMutation(cmd.Context(), "created", result, m))
		},
	}
	flags := command.Flags()
	flags.StringVarP(&m.file, "file", "f", "", "request document FILE ('-' reads stdin)")
	flags.StringVar(&m.id, "id", "", "client-chosen Resource ID (one URL path segment)")
	flags.StringVar(&m.typeName, "type", "", "ResourceType name")
	flags.StringVar(&m.typeVersion, "version", "", "ResourceType version")
	flags.StringVar(&m.owner, "owner", "", "requested owner as KIND=ID")
	flags.StringVar(&m.specFile, "spec", "", "spec document FILE ('-' reads stdin)")
	addWaitFlags(command.Flags(), m)
	return command
}

func buildCreateBody(a *App, m *mutationOptions) ([]byte, error) {
	documentMode := m.file != ""
	assemblyMode := m.id != "" || m.typeName != "" || m.typeVersion != "" || m.owner != "" || m.specFile != ""
	switch {
	case documentMode && assemblyMode:
		return nil, errors.New("use either -f/--file or the assembly flags (--id/--type/--version/--owner/--spec), not both")
	case documentMode:
		raw, err := a.readDocument(m.file)
		if err != nil {
			return nil, err
		}
		if err := validateSingleJSONObject(raw, "request document"); err != nil {
			return nil, err
		}
		return raw, nil
	case assemblyMode:
		var missing []string
		if m.id == "" {
			missing = append(missing, "--id")
		}
		if m.typeName == "" {
			missing = append(missing, "--type")
		}
		if m.typeVersion == "" {
			missing = append(missing, "--version")
		}
		if m.owner == "" {
			missing = append(missing, "--owner")
		}
		if m.specFile == "" {
			missing = append(missing, "--spec")
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("assembly mode requires %s", strings.Join(missing, ", "))
		}
		if err := validateResourceID(m.id); err != nil {
			return nil, err
		}
		ownerKind, ownerID, err := parseOwnerRef(m.owner)
		if err != nil {
			return nil, err
		}
		spec, err := a.readDocument(m.specFile)
		if err != nil {
			return nil, err
		}
		if err := validateSingleJSONObject(spec, "spec"); err != nil {
			return nil, err
		}
		return client.BuildCreateEnvelope(m.id, m.typeName, m.typeVersion, ownerKind, ownerID, spec), nil
	default:
		return nil, errors.New("specify -f/--file with a request document, or all of --id, --type, --version, --owner, and --spec")
	}
}

// validateResourceID mirrors the server's transport rule so obvious mistakes
// fail locally: one non-empty URL path segment without whitespace.
func validateResourceID(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("resource ID is empty")
	}
	for _, r := range id {
		if r <= ' ' || r == 0x7f || r == '/' {
			return errors.New("resource ID must be a single URL path segment without whitespace")
		}
	}
	return nil
}

func newResourceGetCommand(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "get RESOURCE_ID",
		Short: "Read one retained Resource",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.prepare(); err != nil {
				return err
			}
			resource, err := a.api.GetResource(cmd.Context(), args[0])
			if err != nil {
				return exit(classifyInterrupted(cmd.Context(), a.reportReadFailure(err)))
			}
			if err := a.outputResource(resource); err != nil {
				fmt.Fprintf(a.stderr, "error: %s\n", a.clean(err.Error()))
				return exit(ExitFailure)
			}
			return nil
		},
	}
}

func newResourceUpdateCommand(a *App) *cobra.Command {
	m := &mutationOptions{}
	command := &cobra.Command{
		Use:   "update RESOURCE_ID",
		Short: "Admit an asynchronous full spec replacement",
		Long: `Admit an asynchronous full spec replacement for a Resource.

The API performs full replacement only; there is no PATCH semantics in v1.
--spec must contain the complete new spec as one JSON object.

Without --generation the CLI first reads the Resource and preconditions the
update on its current generation. A concurrent change then answers
GENERATION_CONFLICT, which this CLI surfaces and never resolves silently:
re-read the state and re-apply your change deliberately.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.prepare(); err != nil {
				return err
			}
			if err := validateTimeout(m.timeout); err != nil {
				return err
			}
			resourceID := args[0]
			spec, err := a.readSpecDocument(m.specFile)
			if err != nil {
				return err
			}
			generation := m.generation
			if generation == 0 {
				current, err := a.api.GetResource(cmd.Context(), resourceID)
				if err != nil {
					return exit(classifyInterrupted(cmd.Context(), a.reportReadFailure(err)))
				}
				generation = current.Generation
				if a.output == outputText {
					fmt.Fprintf(a.stderr, "preconditioning update on current generation %d\n", generation)
				}
			}
			key, err := resolveIdempotencyKey(m.idempotencyKey)
			if err != nil {
				return err
			}
			result, err := a.api.UpdateResource(cmd.Context(), resourceID, client.WrapUpdateSpec(spec), key, generation)
			if err != nil {
				if interruptedNow(cmd.Context()) {
					return exit(ExitInterrupted)
				}
				return exit(a.reportMutationFailure(key, err))
			}
			return exit(a.finishMutation(cmd.Context(), "update admitted", result, m))
		},
	}
	command.Flags().StringVar(&m.specFile, "spec", "", "complete replacement spec FILE ('-' reads stdin)")
	command.Flags().Uint64Var(&m.generation, "generation", 0, "concrete If-Liftr-Generation precondition; skips the default pre-read")
	addWaitFlags(command.Flags(), m)
	_ = command.MarkFlagRequired("spec")
	return command
}

func newResourceDeleteCommand(a *App) *cobra.Command {
	m := &mutationOptions{}
	command := &cobra.Command{
		Use:   "delete RESOURCE_ID",
		Short: "Admit an asynchronous Resource delete request",
		Long: `Admit an asynchronous delete request for a Resource.

The target is read first and shown before anything is admitted. Interactive
sessions must type the exact Resource ID to confirm; non-interactive use
requires --yes. Without --generation the current generation read from the
target preconditions the deletion. Generation conflicts stop immediately;
they are never retried with a freshly fetched generation.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.prepare(); err != nil {
				return err
			}
			if err := validateTimeout(m.timeout); err != nil {
				return err
			}
			resourceID := args[0]
			target, err := a.api.GetResource(cmd.Context(), resourceID)
			if err != nil {
				return exit(classifyInterrupted(cmd.Context(), a.reportReadFailure(err)))
			}
			a.renderDeleteTarget(target)
			generation := m.generation
			if generation == 0 {
				generation = target.Generation
			}
			if !m.yes {
				confirmed, err := a.confirmDelete(target.ID)
				if err != nil {
					return err
				}
				if !confirmed {
					fmt.Fprintln(a.stderr, "confirmation did not match; nothing was deleted")
					return exit(ExitFailure)
				}
			}
			key, err := resolveIdempotencyKey(m.idempotencyKey)
			if err != nil {
				return err
			}
			result, err := a.api.DeleteResource(cmd.Context(), resourceID, key, generation)
			if err != nil {
				if interruptedNow(cmd.Context()) {
					return exit(ExitInterrupted)
				}
				return exit(a.reportMutationFailure(key, err))
			}
			return exit(a.finishMutation(cmd.Context(), "delete admitted", result, m))
		},
	}
	command.Flags().BoolVarP(&m.yes, "yes", "y", false, "skip interactive confirmation (required when stdin is not a terminal)")
	command.Flags().Uint64Var(&m.generation, "generation", 0, "concrete If-Liftr-Generation precondition; defaults to the generation read from the target")
	addWaitFlags(command.Flags(), m)
	return command
}

func (a *App) renderDeleteTarget(target *client.Resource) {
	fmt.Fprintln(a.stderr, "deleting this Resource:")
	fmt.Fprintf(a.stderr, "  ID:          %s\n", a.clean(target.ID))
	fmt.Fprintf(a.stderr, "  Type:        %s/%s\n", a.clean(target.Type.Name), a.clean(target.Type.Version))
	fmt.Fprintf(a.stderr, "  Owner:       %s/%s\n", a.clean(target.Owner.Kind), a.clean(target.Owner.ID))
	fmt.Fprintf(a.stderr, "  State:       %s\n", a.clean(target.Status.State))
	fmt.Fprintf(a.stderr, "  Generation:  %d\n", target.Generation)
	fmt.Fprintf(a.stderr, "This admits an asynchronous delete operation.\n")
}

// confirmDelete requires typing the exact Resource ID. It never runs when
// stdin is not a terminal — callers enforce --yes there first.
func (a *App) confirmDelete(resourceID string) (bool, error) {
	if !a.isTTY() {
		return false, errors.New("refusing to delete without confirmation: stdin is not interactive; pass --yes for non-interactive use")
	}
	fmt.Fprintf(a.stderr, "Type the resource ID %q to confirm deletion: ", resourceID)
	line, err := a.readLine()
	if err != nil {
		return false, fmt.Errorf("reading confirmation: %w", err)
	}
	return line == resourceID, nil
}

func (a *App) readLine() (string, error) {
	reader := bufio.NewReader(a.stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r"), nil
}

func (a *App) readSpecDocument(specFile string) ([]byte, error) {
	if strings.TrimSpace(specFile) == "" {
		return nil, errors.New("--spec is required: the update replaces the whole spec")
	}
	spec, err := a.readDocument(specFile)
	if err != nil {
		return nil, err
	}
	if err := validateSingleJSONObject(spec, "spec"); err != nil {
		return nil, err
	}
	return spec, nil
}

// finishMutation renders the admission result and optionally follows the
// authoritative monitor Operation.
func (a *App) finishMutation(ctx context.Context, verb string, result *client.MutationResult, m *mutationOptions) int {
	if result.Replay {
		fmt.Fprintln(a.stderr, "note: the server reports that this response replays an earlier admission under this idempotency key")
	}
	if operationID, err := a.api.MonitorOperationID(result); err == nil {
		if a.output == outputText && !m.wait {
			fmt.Fprintf(a.stderr, "monitor with: liftr operation get %s\n", a.clean(operationID))
		}
	} else {
		fmt.Fprintf(a.stderr, "warning: the admission carries no usable monitor reference (%s)\n", a.clean(err.Error()))
	}
	if !m.wait {
		var err error
		if a.output == outputJSON {
			err = emitJSON(a.stdout, result.Resource.Raw)
		} else {
			a.renderAdmissionText(a.stdout, verb, result.Resource)
		}
		if err != nil {
			fmt.Fprintf(a.stderr, "error: %s\n", a.clean(err.Error()))
			return ExitFailure
		}
		return ExitOK
	}
	return a.waitForOperation(ctx, result, m.timeout)
}
