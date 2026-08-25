// SPDX-License-Identifier: Apache-2.0

package opentofu

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"
)

type Command struct {
	Path           string
	Args           []string
	Env            []string
	Dir            string
	MaxOutputBytes int64
}

type CommandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Overflow bool
	// Failure is optional structured evidence supplied by a runner. The OS
	// runner leaves it Unknown because exit status and human diagnostics cannot
	// prove why a command failed.
	Failure CommandFailureKind
}

type CommandFailureKind string

const (
	CommandFailureUnknown       CommandFailureKind = "Unknown"
	CommandFailureDeterministic CommandFailureKind = "Deterministic"
	CommandFailureUnavailable   CommandFailureKind = "Unavailable"
	CommandFailureTimeout       CommandFailureKind = "Timeout"
)

type CommandRunner interface {
	Run(context.Context, Command) (CommandResult, error)
}

type OSCommandRunner struct{}

func (OSCommandRunner) Run(ctx context.Context, command Command) (CommandResult, error) {
	if command.MaxOutputBytes <= 0 {
		return CommandResult{}, errors.New("command output bound is required")
	}
	if err := ctx.Err(); err != nil {
		return CommandResult{ExitCode: -1}, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.Command(command.Path, command.Args...)
	cmd.Dir = command.Dir
	cmd.Env = append([]string(nil), command.Env...)
	configureCommandProcess(cmd)
	limit := &sharedOutputLimit{remaining: command.MaxOutputBytes, cancel: cancel}
	stdout := &boundedBuffer{limit: limit}
	stderr := &boundedBuffer{limit: limit}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		return CommandResult{ExitCode: -1}, err
	}
	stopCancellation := cancelCommandOnContext(runCtx, cmd)
	err := cmd.Wait()
	stopCancellation()
	result := CommandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: 0, Overflow: limit.overflowed()}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err != nil {
		result.ExitCode = -1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			result.ExitCode = exit.ExitCode()
		}
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if result.Overflow {
			return result, errors.New("command output exceeded bound")
		}
		if result.ExitCode >= 0 {
			return result, nil
		}
		return result, errors.New("command execution failed")
	}
	if result.Overflow {
		return result, errors.New("command output exceeded bound")
	}
	return result, nil
}

type sharedOutputLimit struct {
	mu        sync.Mutex
	remaining int64
	overflow  bool
	cancel    context.CancelFunc
}

func (l *sharedOutputLimit) take(size int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.remaining <= 0 {
		l.overflow = true
		l.cancel()
		return 0
	}
	if int64(size) > l.remaining {
		size = int(l.remaining)
		l.remaining = 0
		l.overflow = true
		l.cancel()
		return size
	}
	l.remaining -= int64(size)
	return size
}

func (l *sharedOutputLimit) overflowed() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.overflow }

type boundedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	limit  *sharedOutputLimit
}

func (w *boundedBuffer) Write(p []byte) (int, error) {
	n := w.limit.take(len(p))
	w.mu.Lock()
	_, _ = w.buffer.Write(p[:n])
	w.mu.Unlock()
	if n != len(p) {
		return len(p), io.ErrShortWrite
	}
	return len(p), nil
}

func (w *boundedBuffer) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.buffer.Bytes()...)
}
