// SPDX-License-Identifier: Apache-2.0

package crossplane

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func runtimeCaller() (uintptr, string, int, bool) { return runtime.Caller(0) }

// TestKubernetesStaysInsideTheAdapter walks the packages that must remain
// implementation-blind and fails if any of them imports a Kubernetes library
// or the Crossplane adapter subtree. Together with the per-package boundary
// tests this pins ADR-0015's central promise: Kubernetes concepts exist only
// inside internal/provisioning/crossplane.
func TestKubernetesStaysInsideTheAdapter(t *testing.T) {
	repoRoot := findRepoRoot(t)
	boundaries := map[string][]string{
		"internal/domain":               {"k8s.io/", "github.com/sithea-nou/liftr/internal/provisioning"},
		"internal/lifecycle":            {"k8s.io/", "github.com/sithea-nou/liftr/internal/provisioning"},
		"internal/resourcecontract":     {"k8s.io/", "github.com/sithea-nou/liftr/internal/provisioning"},
		"internal/resourcetypes":        {"k8s.io/"},
		"internal/application":          {"k8s.io/", "github.com/sithea-nou/liftr/internal/provisioning/crossplane", "github.com/pulumi"},
		"internal/api/http":             {"k8s.io/", "github.com/sithea-nou/liftr/internal/provisioning"},
		"internal/worker":               {"k8s.io/", "github.com/sithea-nou/liftr/internal/provisioning/crossplane", "github.com/pulumi"},
		"internal/persistence/postgres": {"k8s.io/", "github.com/sithea-nou/liftr/internal/provisioning/crossplane"},
	}
	for directory, forbidden := range boundaries {
		t.Run(directory, func(t *testing.T) {
			assertNoProductionImports(t, filepath.Join(repoRoot, directory), forbidden)
		})
	}
}

// TestAdapterSubtreeIsTheOnlyKubeConsumer proves the reverse direction: no
// production package outside internal/provisioning/crossplane imports the
// private kube client or fake API server.
func TestAdapterSubtreeIsTheOnlyKubeConsumer(t *testing.T) {
	repoRoot := findRepoRoot(t)
	prefixes := []string{
		"github.com/sithea-nou/liftr/internal/provisioning/crossplane/kube",
	}
	allowed := "internal/provisioning/crossplane"
	fileSet := token.NewFileSet()
	err := filepath.WalkDir(repoRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return nil
		}
		if strings.HasPrefix(relative, ".") || relative == ".git" || strings.HasPrefix(relative, ".git/") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() && (relative == "vendor" || relative == ".idea" || relative == "docs") {
			return filepath.SkipDir
		}
		if entry.IsDir() || !strings.HasSuffix(relative, ".go") {
			return nil
		}
		cleaned := filepath.ToSlash(relative)
		if strings.HasPrefix(cleaned, allowed) {
			return nil
		}
		source, parseErr := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return nil
		}
		for _, imported := range source.Imports {
			importPath := strings.Trim(imported.Path.Value, `"`)
			for _, prefix := range prefixes {
				if strings.HasPrefix(importPath, prefix) {
					t.Errorf("%s imports the private Kubernetes boundary %s", cleaned, importPath)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// assertNoProductionImports skips _test.go files: test packages may wire
// fakes and adapters directly; production code may not.
func assertNoProductionImports(t *testing.T, dir string, forbidden []string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fileSet := token.NewFileSet()
	files := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, parseErr := parser.ParseFile(fileSet, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		files++
		for _, imported := range source.Imports {
			path := strings.Trim(imported.Path.Value, `"`)
			for _, bad := range forbidden {
				if strings.HasPrefix(path, bad) {
					t.Errorf("%s imports forbidden package %s", name, path)
				}
			}
		}
	}
	if files == 0 {
		t.Fatalf("no Go files found under %s", dir)
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	_, packageFile, _, _ := runtimeCaller()
	dir := filepath.Dir(packageFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot locate repository root")
		}
		dir = parent
	}
}
