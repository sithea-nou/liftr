// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestClientImportsOnlyThePublicContract pins the M12 boundary: the HTTP
// client depends on the published OpenAPI contract and the standard library
// alone. It must never import Liftr's application, domain, or server
// implementation packages, nor schema, YAML, or CLI frameworks.
func TestClientImportsOnlyThePublicContract(t *testing.T) {
	forbidden := []string{
		"github.com/sithea-nou/liftr/internal/application",
		"github.com/sithea-nou/liftr/internal/domain",
		"github.com/sithea-nou/liftr/internal/api",
		"github.com/sithea-nou/liftr/internal/auth",
		"github.com/sithea-nou/liftr/internal/identity",
		"github.com/sithea-nou/liftr/internal/lifecycle",
		"github.com/sithea-nou/liftr/internal/persistence",
		"github.com/sithea-nou/liftr/internal/provisioning",
		"github.com/sithea-nou/liftr/internal/resourcetypes",
		"github.com/sithea-nou/liftr/internal/server",
		"github.com/sithea-nou/liftr/internal/worker",
		"github.com/santhosh-tekuri/jsonschema",
		"gopkg.in/yaml",
		"github.com/spf13/cobra",
	}
	assertCleanImports(t, ".", forbidden)
}

func assertCleanImports(t *testing.T, dir string, forbidden []string) {
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
		source, err := parser.ParseFile(fileSet, dir+"/"+name, nil, parser.ImportsOnly)
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
