// SPDX-License-Identifier: Apache-2.0

package auth_test

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestAuthIsAnImplementationPackage pins the M11 dependency directions
// (ADR-0012): auth implements consumers' ports and imports only identity,
// domain, and the standard library. It must never import application, the
// transport, persistence, or provisioning, so policy implementations stay
// structurally substitutable behind composition.
func TestAuthIsAnImplementationPackage(t *testing.T) {
	forbidden := []string{
		"github.com/sithea-nou/liftr/internal/application",
		"github.com/sithea-nou/liftr/internal/api",
		"github.com/sithea-nou/liftr/internal/persistence",
		"github.com/sithea-nou/liftr/internal/provisioning",
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
