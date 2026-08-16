// SPDX-License-Identifier: Apache-2.0

package pulumi

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type isolatedWorkspace struct {
	root    string
	workDir string
	homeDir string
	lock    *os.File
}

func createWorkspace(root string, program Program, input []byte) (isolatedWorkspace, error) {
	workspaceRoot, err := os.MkdirTemp(root, "liftr-pulumi-")
	if err != nil {
		return isolatedWorkspace{}, fmt.Errorf("create isolated workspace")
	}
	if err := os.Chmod(workspaceRoot, 0o700); err != nil {
		_ = os.RemoveAll(workspaceRoot)
		return isolatedWorkspace{}, fmt.Errorf("protect isolated workspace")
	}
	lock, err := acquireWorkspaceLock(workspaceRoot)
	if err != nil {
		_ = os.RemoveAll(workspaceRoot)
		return isolatedWorkspace{}, fmt.Errorf("lock isolated workspace")
	}
	workspace := isolatedWorkspace{root: workspaceRoot, workDir: filepath.Join(workspaceRoot, "program"), homeDir: filepath.Join(workspaceRoot, "home"), lock: lock}
	if err := os.Mkdir(workspace.workDir, 0o700); err != nil {
		workspace.cleanup()
		return isolatedWorkspace{}, fmt.Errorf("create program workspace")
	}
	if err := os.Mkdir(workspace.homeDir, 0o700); err != nil {
		workspace.cleanup()
		return isolatedWorkspace{}, fmt.Errorf("create Pulumi home")
	}
	if err := copySource(program.SourceDir, workspace.workDir); err != nil {
		workspace.cleanup()
		return isolatedWorkspace{}, err
	}
	digest, err := SourceDigest(workspace.workDir)
	if err != nil || !strings.EqualFold(digest, program.SourceDigest) {
		workspace.cleanup()
		return isolatedWorkspace{}, fmt.Errorf("copied Pulumi source does not match its registration")
	}
	inputDir := filepath.Join(workspace.workDir, ".liftr")
	if err := os.Mkdir(inputDir, 0o700); err != nil {
		workspace.cleanup()
		return isolatedWorkspace{}, fmt.Errorf("create private input directory")
	}
	if err := os.WriteFile(filepath.Join(inputDir, "input"), input, 0o600); err != nil {
		workspace.cleanup()
		return isolatedWorkspace{}, fmt.Errorf("write private program input")
	}
	return workspace, nil
}

func (w isolatedWorkspace) inputPath() string { return filepath.Join(w.workDir, ".liftr", "input") }
func (w isolatedWorkspace) cleanup() {
	if w.lock != nil {
		releaseWorkspaceLock(w.lock)
	}
	_ = os.RemoveAll(w.root)
}

func cleanupStaleWorkspaces(root string, olderThan time.Duration) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("inspect workspace root")
	}
	cutoff := time.Now().Add(-olderThan)
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "liftr-pulumi-") {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		active, release, err := inspectWorkspaceLock(filepath.Join(root, entry.Name()))
		if err != nil || active {
			continue
		}
		if release != nil {
			release()
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return fmt.Errorf("remove stale isolated workspace")
		}
	}
	return nil
}

func copySource(source, destination string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("copy Pulumi source")
		}
		if path == source {
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil || forbiddenSourcePath(relative) {
			return fmt.Errorf("copy Pulumi source")
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (!info.Mode().IsRegular() && !info.IsDir()) {
			return fmt.Errorf("copy Pulumi source")
		}
		target := filepath.Join(destination, relative)
		if info.IsDir() {
			return os.Mkdir(target, 0o700)
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("copy Pulumi source")
		}
		mode := fs.FileMode(0o600)
		if info.Mode()&0o100 != 0 {
			mode = 0o700
		}
		if err := os.WriteFile(target, contents, mode); err != nil {
			return fmt.Errorf("copy Pulumi source")
		}
		return nil
	})
}
