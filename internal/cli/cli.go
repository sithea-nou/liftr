// SPDX-License-Identifier: Apache-2.0

// Package cli implements the Liftr command-line interface on top of the
// reusable public HTTP client in internal/client. It owns terminal concerns
// only: flag parsing, configuration resolution, human and JSON rendering,
// destructive-action confirmation, Operation waiting, and exit codes. Like
// the client, it imports nothing from the Liftr server implementation: every
// effect flows CLI -> public HTTP API -> Liftr.
//
// Exit-code contract (documented in ADR-0013):
//
//	0   success
//	1   generic client/network/protocol failure or user abort
//	2   CLI usage, configuration, or input error
//	3   authentication failure
//	4   API-rejected request (any Problem other than authentication)
//	5   admitted Operation failed, was canceled, or the wait timed out
//	130 interrupted
//
// Command implementations print their own diagnostics and return an
// exitError carrying only a code. Any other error reaching Execute is by
// convention framework-level misuse (flag parsing, unknown commands,
// unresolved configuration) and maps to exit code 2.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sithea-nou/liftr/internal/client"
)

const (
	ExitOK              = 0
	ExitFailure         = 1
	ExitUsage           = 2
	ExitAuth            = 3
	ExitRejected        = 4
	ExitOperationFailed = 5
	ExitInterrupted     = 130
)

// interruptGrace bounds how long a shutting-down command may keep the
// process alive after a signal before being terminated forcefully.
const interruptGrace = 2 * time.Second

// stdinIsTTYOverride lets tests simulate interactive and non-interactive
// terminals; production always probes the real stdin.
var stdinIsTTYOverride func() bool

type exitError struct{ code int }

func (e *exitError) Error() string { return "" }

func exit(code int) error { return &exitError{code: code} }

// App carries one invocation's resolved state.
type App struct {
	stdout  io.Writer
	stderr  io.Writer
	stdin   io.Reader
	isTTY   func() bool
	version string

	origin        *url.URL
	correlationID string
	api           *client.Client
	output        string
	flagServer    string
	flagTokenFile string
	flagOutput    string
}

// Execute runs the CLI and returns the process exit code.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer, stdin io.Reader, version string) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	finished := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			timer := time.NewTimer(interruptGrace)
			defer timer.Stop()
			select {
			case <-finished:
			case <-timer.C:
				os.Exit(ExitInterrupted)
			}
		case <-finished:
		}
	}()
	defer close(finished)

	app := &App{
		stdout:        stdout,
		stderr:        stderr,
		stdin:         stdin,
		version:       version,
		correlationID: newCorrelationID(),
	}
	app.isTTY = func() bool {
		if stdinIsTTYOverride != nil {
			return stdinIsTTYOverride()
		}
		return defaultStdinIsTTY()
	}

	root := app.newRootCmd()
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(args)

	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}
	var carried *exitError
	if errors.As(err, &carried) {
		return carried.code
	}
	message := err.Error()
	if message == "" {
		message = "invalid usage"
	}
	fmt.Fprintf(stderr, "error: %s\n", sanitize(message))
	fmt.Fprintf(stderr, "run 'liftr --help' for usage\n")
	return ExitUsage
}

func (a *App) newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "liftr",
		Short:         "Command-line client for the Liftr resource lifecycle control plane",
		Long:          "Liftr CLI talks exclusively to the public Liftr HTTP API (/v1). Configure the server with --server or LIFTR_SERVER and present an already-issued bearer access token via --token-file, LIFTR_TOKEN_FILE, or LIFTR_TOKEN.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	flags := root.PersistentFlags()
	flags.StringVar(&a.flagServer, "server", "", "Liftr server origin URL (default LIFTR_SERVER, then "+DefaultServerURL+")")
	flags.StringVar(&a.flagTokenFile, "token-file", "", "file containing an access token (default LIFTR_TOKEN_FILE, then LIFTR_TOKEN)")
	flags.StringVarP(&a.flagOutput, "output", "o", outputText, `output mode: "text" (default) or "json"`)

	root.AddCommand(
		newResourceTypeCommand(a),
		newResourceCommand(a),
		newOperationCommand(a),
		newVersionCommand(a),
	)
	return root
}

func newVersionCommand(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			fmt.Fprintf(a.stdout, "liftr version %s\n", a.version)
			return nil
		},
	}
}

func defaultStdinIsTTY() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
