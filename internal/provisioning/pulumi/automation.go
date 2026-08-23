// SPDX-License-Identifier: Apache-2.0

package pulumi

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/auto"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/opthistory"
	"github.com/pulumi/pulumi/sdk/v3/go/auto/optlist"
)

var (
	errStackNotFound = errors.New("Pulumi stack not found")
	errStackExists   = errors.New("Pulumi stack already exists")
)

type updateSummary struct {
	kind      string
	message   string
	result    string
	startTime string
	endTime   *string
}

type stackInfo struct{ updateInProgress bool }

type automationStack interface {
	Up(context.Context, string) (updateSummary, error)
	Destroy(context.Context, string) (updateSummary, error)
	History(context.Context, int, int) ([]updateSummary, error)
	Info(context.Context) (stackInfo, error)
	// SelectedOutput reads exactly one named stack export as JSON. It must
	// never request secret material: no invocation may pass --show-secrets,
	// so Pulumi-marked secrets arrive in redacted form and are rejected.
	SelectedOutput(ctx context.Context, name string) ([]byte, error)
}

type automationWorkspace interface {
	SelectStack(context.Context, string) (automationStack, error)
	CreateStack(context.Context, string) (automationStack, error)
}

type automationFactory interface {
	Open(context.Context, string, string, map[string]string) (automationWorkspace, error)
}

type localFactory struct{ command auto.PulumiCommand }

func newLocalFactory(root, goExecutable string) (automationFactory, error) {
	command, err := newPinnedCommand(root, goExecutable)
	if err != nil {
		return nil, err
	}
	return localFactory{command: command}, nil
}

func (f localFactory) Open(ctx context.Context, workDir, homeDir string, environment map[string]string) (automationWorkspace, error) {
	workspace, err := auto.NewLocalWorkspace(ctx, auto.WorkDir(workDir), auto.PulumiHome(homeDir), auto.Pulumi(f.command), auto.EnvVars(environment))
	if err != nil {
		return nil, fmt.Errorf("initialize Pulumi workspace")
	}
	return localWorkspace{workspace: workspace}, nil
}

type localWorkspace struct{ workspace auto.Workspace }

func (w localWorkspace) SelectStack(ctx context.Context, name string) (automationStack, error) {
	found, err := w.hasStack(ctx, name)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errStackNotFound
	}
	stack, err := auto.SelectStack(ctx, name, w.workspace)
	if err != nil {
		if auto.IsSelectStack404Error(err) {
			return nil, errStackNotFound
		}
		return nil, err
	}
	return localStack{stack: stack}, nil
}

func (w localWorkspace) CreateStack(ctx context.Context, name string) (automationStack, error) {
	stack, err := auto.NewStack(ctx, name, w.workspace)
	if err != nil {
		if auto.IsCreateStack409Error(err) {
			return nil, errStackExists
		}
		if found, listErr := w.hasStack(ctx, name); listErr == nil && found {
			return nil, errStackExists
		}
		return nil, err
	}
	return localStack{stack: stack}, nil
}

func (w localWorkspace) hasStack(ctx context.Context, name string) (bool, error) {
	stacks, err := w.workspace.ListStacks(ctx, optlist.All())
	if err != nil {
		return false, err
	}
	for _, stack := range stacks {
		if stack.Name == name || stack.Name == name[strings.LastIndex(name, "/")+1:] {
			return true, nil
		}
	}
	return false, nil
}

type localStack struct{ stack auto.Stack }

func (s localStack) Up(ctx context.Context, message string) (updateSummary, error) {
	return s.run(ctx, "up", message)
}

func (s localStack) Destroy(ctx context.Context, message string) (updateSummary, error) {
	return s.run(ctx, "destroy", message)
}

func (s localStack) run(ctx context.Context, operation, message string) (updateSummary, error) {
	workspace := s.stack.Workspace()
	environment := make([]string, 0, len(workspace.GetEnvVars())+1)
	if home := workspace.PulumiHome(); home != "" {
		environment = append(environment, "PULUMI_HOME="+home)
	}
	for key, value := range workspace.GetEnvVars() {
		environment = append(environment, key+"="+value)
	}
	args := []string{operation, "--yes", "--skip-preview", "--stack", s.stack.Name(), "--message", message,
		"--suppress-outputs", "--suppress-progress", "--color", "never"}
	_, _, _, err := workspace.PulumiCommand().Run(ctx, workspace.WorkDir(), nil, nil, nil, environment, args...)
	if err != nil {
		return updateSummary{}, err
	}
	history, err := s.History(ctx, 10, 1)
	if err != nil {
		return updateSummary{}, err
	}
	for _, summary := range history {
		if summary.message == message && strings.EqualFold(summary.kind, expectedCommandKind(operation)) {
			return summary, nil
		}
	}
	return updateSummary{}, fmt.Errorf("Pulumi operation completed without correlated history")
}

func expectedCommandKind(operation string) string {
	if operation == "destroy" {
		return "destroy"
	}
	return "update"
}

func (s localStack) History(ctx context.Context, pageSize, page int) ([]updateSummary, error) {
	history, err := s.stack.History(ctx, pageSize, page, opthistory.ShowSecrets(false))
	if err != nil {
		return nil, err
	}
	result := make([]updateSummary, len(history))
	for i := range history {
		result[i] = normalizeSummary(history[i])
	}
	return result, nil
}

func (s localStack) Info(ctx context.Context) (stackInfo, error) {
	info, err := s.stack.Info(ctx)
	return stackInfo{updateInProgress: info.UpdateInProgress}, err
}

// selectedOutputArgs builds the exact CLI arguments for one selected,
// non-secret output read. The allowlisted name is the only retrieval surface
// and --show-secrets can never appear here.
func selectedOutputArgs(name, fullyQualifiedStack string) []string {
	return []string{"stack", "output", name, "--json", "--stack", fullyQualifiedStack, "--color", "never"}
}

// SelectedOutput invokes `pulumi stack output <name> --json` directly. The
// allowlisted name is the only retrieval surface Liftr exposes, and
// --show-secrets is never passed: the primary secret boundary is what this
// command can ask for, not how its text is filtered afterwards.
func (s localStack) SelectedOutput(ctx context.Context, name string) ([]byte, error) {
	workspace := s.stack.Workspace()
	environment := make([]string, 0, len(workspace.GetEnvVars())+1)
	if home := workspace.PulumiHome(); home != "" {
		environment = append(environment, "PULUMI_HOME="+home)
	}
	for key, value := range workspace.GetEnvVars() {
		environment = append(environment, key+"="+value)
	}
	args := selectedOutputArgs(name, s.stack.Name())
	stdout, _, exitCode, err := workspace.PulumiCommand().Run(ctx, workspace.WorkDir(), nil, nil, nil, environment, args...)
	if err != nil || exitCode != 0 {
		// Raw CLI output is discarded entirely; neither stdout nor stderr may
		// reach errors or logs.
		return nil, fmt.Errorf("selected output %q could not be read", name)
	}
	if len(stdout) > maxOutputBytes {
		return nil, fmt.Errorf("selected output %q exceeds the size bound", name)
	}
	return []byte(stdout), nil
}

func normalizeSummary(summary auto.UpdateSummary) updateSummary {
	message := summary.Message
	if unquoted, err := strconv.Unquote(message); err == nil {
		message = unquoted
	}
	return updateSummary{kind: summary.Kind, message: message, result: summary.Result, startTime: summary.StartTime, endTime: summary.EndTime}
}

func summaryTime(value *string) time.Time {
	if value == nil {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, *value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}
