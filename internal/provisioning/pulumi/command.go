// SPDX-License-Identifier: Apache-2.0

package pulumi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/blang/semver"
)

type pinnedCommand struct {
	path    string
	version semver.Version
}

func newPinnedCommand(root string) (pinnedCommand, error) {
	path := filepath.Join(root, "bin", "pulumi")
	if runtime.GOOS == "windows" {
		path += ".exe"
	}
	version := semver.MustParse(CLIVersion)
	command := exec.Command(path, "version")
	command.Env = []string{"PATH=" + filepath.Dir(path), "PULUMI_SKIP_UPDATE_CHECK=true"}
	output, err := command.Output()
	if err != nil {
		return pinnedCommand{}, fmt.Errorf("validate pinned Pulumi CLI")
	}
	actual, err := semver.ParseTolerant(strings.TrimSpace(string(output)))
	if err != nil || !actual.EQ(version) {
		return pinnedCommand{}, fmt.Errorf("pinned Pulumi CLI version mismatch")
	}
	return pinnedCommand{path: path, version: version}, nil
}

func (p pinnedCommand) Version() semver.Version { return p.version }

func (p pinnedCommand) Run(ctx context.Context, workDir string, stdin io.Reader, additionalOutput, additionalErrorOutput []io.Writer, additionalEnvironment []string, args ...string) (string, string, int, error) {
	if !containsArgument(args, "--non-interactive") {
		args = append([]string{"--non-interactive"}, args...)
	}
	command := exec.CommandContext(ctx, p.path, args...)
	command.Dir = workDir
	command.Stdin = stdin
	command.Env = sanitizedEnvironment(p.path, additionalEnvironment)
	configureProcess(command)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = io.MultiWriter(append(additionalOutput, &stdout)...)
	command.Stderr = io.MultiWriter(append(additionalErrorOutput, &stderr)...)
	err := command.Run()
	exitCode := 0
	if err != nil {
		exitCode = -1
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			exitCode = exitError.ExitCode()
		}
	}
	return stdout.String(), stderr.String(), exitCode, err
}

func sanitizedEnvironment(pulumiPath string, additional []string) []string {
	values := map[string]string{
		"NO_COLOR":                 "1",
		"PATH":                     filepath.Dir(pulumiPath),
		"PULUMI_AUTOMATION_API":    "true",
		"PULUMI_SKIP_UPDATE_CHECK": "true",
	}
	if temporary := os.TempDir(); temporary != "" {
		values["TMPDIR"] = temporary
		values["TMP"] = temporary
		values["TEMP"] = temporary
	}
	for _, entry := range additional {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			values[key] = value
		}
	}
	values["USER"] = "liftr"
	values["LOGNAME"] = "liftr"
	if home := values["PULUMI_HOME"]; home != "" {
		values["HOME"] = home
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys))
	for _, key := range keys {
		environment = append(environment, key+"="+values[key])
	}
	return environment
}

func containsArgument(args []string, value string) bool {
	for _, argument := range args {
		if argument == value {
			return true
		}
	}
	return false
}
