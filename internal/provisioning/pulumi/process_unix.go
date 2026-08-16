// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package pulumi

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

func configureProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return os.ErrProcessDone
		}
		if err := syscall.Kill(-command.Process.Pid, syscall.SIGINT); err != nil {
			return command.Process.Kill()
		}
		return nil
	}
	command.WaitDelay = 10 * time.Second
}
