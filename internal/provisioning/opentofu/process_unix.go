// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package opentofu

import (
	"context"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const gracefulProcessDrain = 10 * time.Second

func configureCommandProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func cancelCommandOnContext(ctx context.Context, command *exec.Cmd) func() {
	done := make(chan struct{})
	finished := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() { close(done) })
		<-finished
	}
	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
			if command.Process == nil {
				return
			}
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGINT)
			timer := time.NewTimer(gracefulProcessDrain)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-done:
			}
			// The parent may exit on SIGINT before its descendants do. Always
			// force the owned process group empty after cancellation.
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		case <-done:
		}
	}()
	return stop
}

func lockFileNonblocking(fileDescriptor uintptr) error {
	return syscall.Flock(int(fileDescriptor), syscall.LOCK_EX|syscall.LOCK_NB)
}

func sameFilesystem(left, right any) bool {
	l, lok := left.(*syscall.Stat_t)
	r, rok := right.(*syscall.Stat_t)
	return lok && rok && l.Dev == r.Dev
}
