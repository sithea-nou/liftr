// SPDX-License-Identifier: Apache-2.0

//go:build windows

package pulumi

import (
	"os/exec"
	"time"
)

func configureProcess(command *exec.Cmd) {
	command.WaitDelay = 10 * time.Second
}
