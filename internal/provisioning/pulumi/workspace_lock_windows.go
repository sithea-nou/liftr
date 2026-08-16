// SPDX-License-Identifier: Apache-2.0

//go:build windows

package pulumi

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

const workspaceLockName = ".active"

func acquireWorkspaceLock(root string) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(root, workspaceLockName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func releaseWorkspaceLock(file *os.File) {
	var overlapped windows.Overlapped
	_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
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
	var overlapped windows.Overlapped
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped); err != nil {
		_ = file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return true, nil, nil
		}
		return false, nil, err
	}
	return false, func() { releaseWorkspaceLock(file) }, nil
}
