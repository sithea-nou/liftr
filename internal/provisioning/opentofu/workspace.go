// SPDX-License-Identifier: Apache-2.0

package opentofu

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const workspacePrefix = "liftr-otf-"

type workspaceMetadata struct {
	Version int    `json:"version"`
	Created string `json:"created"`
}

type workspace struct {
	path           string
	lock           *os.File
	quarantineRoot string
	uncertain      bool
}

func validatePrivateRoots(workRoot, quarantineRoot string) error {
	if !filepath.IsAbs(workRoot) || !filepath.IsAbs(quarantineRoot) || filepath.Clean(workRoot) == filepath.Clean(quarantineRoot) {
		return fmt.Errorf("OpenTofu work and quarantine roots must be distinct absolute paths")
	}
	if pathWithin(workRoot, quarantineRoot) || pathWithin(quarantineRoot, workRoot) {
		return fmt.Errorf("OpenTofu work and quarantine roots must not contain one another")
	}
	if err := validatePrivateDirectory(workRoot); err != nil {
		return err
	}
	if err := validatePrivateDirectory(quarantineRoot); err != nil {
		return err
	}
	workInfo, _ := os.Stat(workRoot)
	quarantineInfo, _ := os.Stat(quarantineRoot)
	if !sameFilesystem(workInfo.Sys(), quarantineInfo.Sys()) {
		return fmt.Errorf("OpenTofu work and quarantine roots must share a filesystem")
	}
	return nil
}

func validatePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("OpenTofu directory must be a private real directory")
	}
	return nil
}

func pinExecutable(source, workRoot, expectedDigest string) (string, error) {
	destination := filepath.Join(workRoot, ".liftr-opentofu-"+strings.ToLower(expectedDigest))
	if info, err := os.Lstat(destination); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o500 {
			return "", fmt.Errorf("pinned OpenTofu executable is invalid")
		}
		digest, err := digestFile(destination, maxExecutableBytes)
		if err != nil || !strings.EqualFold(digest, expectedDigest) {
			return "", fmt.Errorf("pinned OpenTofu executable digest mismatch")
		}
		return destination, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect pinned OpenTofu executable: %w", err)
	}

	input, err := os.Open(source)
	if err != nil {
		return "", fmt.Errorf("open OpenTofu executable: %w", err)
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxExecutableBytes {
		return "", fmt.Errorf("OpenTofu executable changed during admission")
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o400)
	if err != nil {
		return "", fmt.Errorf("create pinned OpenTofu executable: %w", err)
	}
	keep := false
	defer func() {
		_ = output.Close()
		if !keep {
			_ = os.Remove(destination)
		}
	}()
	written, copyErr := io.Copy(output, io.LimitReader(input, maxExecutableBytes+1))
	if copyErr != nil || written != info.Size() || written > maxExecutableBytes {
		return "", fmt.Errorf("copy pinned OpenTofu executable")
	}
	if err := output.Sync(); err != nil {
		return "", fmt.Errorf("sync pinned OpenTofu executable: %w", err)
	}
	if err := output.Close(); err != nil {
		return "", fmt.Errorf("close pinned OpenTofu executable: %w", err)
	}
	digest, err := digestFile(destination, maxExecutableBytes)
	if err != nil || !strings.EqualFold(digest, expectedDigest) {
		return "", fmt.Errorf("pinned OpenTofu executable digest mismatch")
	}
	if err := os.Chmod(destination, 0o500); err != nil {
		return "", fmt.Errorf("protect pinned OpenTofu executable: %w", err)
	}
	keep = true
	return destination, nil
}

func newWorkspace(workRoot, quarantineRoot string) (*workspace, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	path := filepath.Join(workRoot, workspacePrefix+hex.EncodeToString(random[:]))
	if err := os.Mkdir(path, 0o700); err != nil {
		return nil, err
	}
	metadata, _ := json.Marshal(workspaceMetadata{Version: 1, Created: time.Now().UTC().Format(time.RFC3339Nano)})
	if err := writePrivateFile(filepath.Join(path, ".liftr-metadata.json"), metadata, 0o600); err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(path, ".liftr-lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		_ = os.RemoveAll(path)
		return nil, err
	}
	if err := lockFileNonblocking(lock.Fd()); err != nil {
		_ = lock.Close()
		_ = os.RemoveAll(path)
		return nil, err
	}
	return &workspace{path: path, lock: lock, quarantineRoot: quarantineRoot}, nil
}

func (w *workspace) close() error {
	if w == nil {
		return nil
	}
	if hasErroredState(w.path) {
		w.uncertain = true
	}
	if w.uncertain {
		err := moveToQuarantine(w.path, w.quarantineRoot)
		if w.lock != nil {
			_ = w.lock.Close()
		}
		return err
	}
	if w.lock != nil {
		_ = w.lock.Close()
	}
	return os.RemoveAll(w.path)
}

func (w *workspace) quarantine() { w.uncertain = true }

func moveToQuarantine(path, root string) error {
	name := filepath.Base(path) + "-" + time.Now().UTC().Format("20060102T150405.000000000")
	return os.Rename(path, filepath.Join(root, name))
}

func hasErroredState(root string) bool {
	found := false
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.EqualFold(entry.Name(), "errored.tfstate") {
			found = true
			return io.EOF
		}
		return nil
	})
	return found
}

func scanOrphanWorkspaces(workRoot, quarantineRoot string) error {
	entries, err := os.ReadDir(workRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), workspacePrefix) {
			continue
		}
		path := filepath.Join(workRoot, entry.Name())
		info, err := os.Lstat(path)
		if err != nil || info.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("orphan OpenTofu workspace has invalid ownership metadata")
		}
		metadataRaw, err := os.ReadFile(filepath.Join(path, ".liftr-metadata.json"))
		if err != nil || len(metadataRaw) > 4096 {
			return fmt.Errorf("orphan OpenTofu workspace metadata is invalid")
		}
		var metadata workspaceMetadata
		if json.Unmarshal(metadataRaw, &metadata) != nil || metadata.Version != 1 {
			return fmt.Errorf("orphan OpenTofu workspace metadata is invalid")
		}
		lock, err := os.OpenFile(filepath.Join(path, ".liftr-lock"), os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("orphan OpenTofu workspace lock is invalid")
		}
		if err := lockFileNonblocking(lock.Fd()); err != nil {
			_ = lock.Close()
			continue
		}
		if err := moveToQuarantine(path, quarantineRoot); err != nil {
			_ = lock.Close()
			return err
		}
		_ = lock.Close()
	}
	return nil
}

func copySource(source string, target *workspace, limits SourceLimits, expectedDigest string) (string, error) {
	files, err := inspectSource(source, limits)
	if err != nil {
		return "", err
	}
	destinationRoot := filepath.Join(target.path, "source")
	if err := os.Mkdir(destinationRoot, 0o700); err != nil {
		return "", err
	}
	for _, file := range files {
		destination := filepath.Join(destinationRoot, file.rel)
		if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
			return "", err
		}
		input, err := os.Open(filepath.Join(source, file.rel))
		if err != nil {
			return "", err
		}
		mode := os.FileMode(0o600)
		if filepath.Base(file.rel) == ".terraform.lock.hcl" {
			mode = 0o400
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err == nil {
			_, err = io.Copy(output, io.LimitReader(input, file.size+1))
		}
		closeErr := outputClose(output)
		_ = input.Close()
		if err != nil {
			return "", err
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	digest, err := SourceDigest(destinationRoot, limits)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(digest, expectedDigest) {
		return "", fmt.Errorf("copied OpenTofu source digest mismatch")
	}
	return destinationRoot, nil
}

func outputClose(file *os.File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func writePrivateFile(path string, content []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err = file.Write(content); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}
