// SPDX-License-Identifier: Apache-2.0

package postgresqldatabase_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestContractPackageDoesNotImportOrchestration pins the M9 boundary: the
// PostgreSQLDatabase developer contract — including its update-transition
// rules — must never import the application orchestration layer, the
// provisioning contract, or any provisioner adapter. Transition rules are
// authored against resourcetypes-local types only, so implementations of this
// contract stay free of orchestration dependencies.
func TestContractPackageDoesNotImportOrchestration(t *testing.T) {
	forbidden := []string{
		"github.com/sithea-nou/liftr/internal/application",
		"github.com/sithea-nou/liftr/internal/provisioning",
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
