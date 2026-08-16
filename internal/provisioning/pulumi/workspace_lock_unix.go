// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package pulumi

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

const workspaceLockName = ".active"

func acquireWorkspaceLock(root string) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(root, workspaceLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func releaseWorkspaceLock(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

func inspectWorkspaceLock(root string) (bool, func(), error) {
	file, err := os.OpenFile(filepath.Join(root, workspaceLockName), os.O_RDWR, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return true, nil, nil
		}
		return false, nil, err
	}
	return false, func() { releaseWorkspaceLock(file) }, nil
}
