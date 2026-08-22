// SPDX-License-Identifier: Apache-2.0

package httpapi_test

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestTransportDoesNotValidateResourceTypeSchemas pins that the HTTP layer
// performs only transport-level parsing: it never imports the concrete
// ResourceType implementation or any JSON Schema library, so contract
// validation cannot drift into the transport.
func TestTransportDoesNotValidateResourceTypeSchemas(t *testing.T) {
	forbidden := []string{
		"github.com/santhosh-tekuri/jsonschema",
		"github.com/sithea-nou/liftr/internal/resourcetypes",
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
