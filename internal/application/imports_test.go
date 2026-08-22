// SPDX-License-Identifier: Apache-2.0

package application_test

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestApplicationDoesNotDependOnResourceTypeImplementation pins invariant B:
// the application consumes ResourceTypes only through its consumer-owned
// port. It must never import the concrete contract/schema package or any
// JSON Schema implementation, so validator swaps cannot reach admission
// orchestration.
func TestApplicationDoesNotDependOnResourceTypeImplementation(t *testing.T) {
	forbidden := []string{
		"github.com/santhosh-tekuri/jsonschema",
		"github.com/sithea-nou/liftr/internal/resourcetypes",
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
		source, err := parser.ParseFile(fileSet, name, nil, parser.ImportsOnly)
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
