// SPDX-License-Identifier: Apache-2.0

package resourcetypes_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestContractPackageDoesNotImportApplication pins the M10 validation
// boundary: the shared ResourceType contract vocabulary lives in the neutral
// resourcecontract package, so concrete ResourceType implementations and this
// package must never import the application orchestration layer — not for
// violation types, not for error construction, not for catalog interfaces.
// The application keeps its consumer port; both sides meet on the neutral
// package instead.
func TestContractPackageDoesNotImportOrchestration(t *testing.T) {
	forbidden := []string{
		"github.com/sithea-nou/liftr/internal/application",
		"github.com/sithea-nou/liftr/internal/provisioning",
		"github.com/sithea-nou/liftr/internal/api",
		"github.com/sithea-nou/liftr/internal/persistence",
		"github.com/sithea-nou/liftr/internal/worker",
		"github.com/pulumi",
		"github.com/sithea-nou/liftr/internal/provisioning/crossplane",
		"k8s.io/",
	}
	assertNoForbiddenImports(t, ".", forbidden)
}

func assertNoForbiddenImports(t *testing.T, dir string, forbidden []string) {
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
		source, err := parser.ParseFile(fileSet, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
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
		t.Fatal("no production Go files found to inspect")
	}
}
