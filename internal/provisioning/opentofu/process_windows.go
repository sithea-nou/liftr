// SPDX-License-Identifier: Apache-2.0

//go:build windows

package opentofu

import (
	"context"
	"errors"
	"os/exec"
)

func configureCommandProcess(*exec.Cmd)                        {}
func cancelCommandOnContext(context.Context, *exec.Cmd) func() { return func() {} }
func lockFileNonblocking(uintptr) error                        { return errors.New("safe workspace locking unsupported") }
func sameFilesystem(any, any) bool                             { return false }
