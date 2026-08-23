// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestCLIImportsOnlyTheClient pins the M12 boundary for the command layer:
// internal/cli may import internal/client and third-party CLI frameworks,
// but never Liftr's application, domain, or server implementation packages,
// nor schema or YAML libraries. The same rule holds for cmd/liftr.
func TestCLIImportsOnlyTheClient(t *testing.T) {
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
	}
	assertCleanImports(t, ".", forbidden)
	assertCleanImports(t, "../../cmd/liftr", forbidden)
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
		t.Fatalf("no production Go files found to inspect in %s", dir)
	}
}
