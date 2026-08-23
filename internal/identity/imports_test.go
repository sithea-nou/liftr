// SPDX-License-Identifier: Apache-2.0

package identity_test

import (
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// TestIdentityImportsOnlyDomainAndStdlib pins the neutral-vocabulary
// boundary (ADR-0012): the identity package may import only the domain and
// the standard library. It must never know about application ports, HTTP,
// authentication protocols, persistence, or policy implementations.
func TestIdentityImportsOnlyDomainAndStdlib(t *testing.T) {
	allowedPrefixes := []string{
		"crypto/",
		"encoding/",
		"errors",
		"fmt",
		"hash",
		"sort",
		"strings",
		"github.com/sithea-nou/liftr/internal/domain",
	}
	entries, err := os.ReadDir(".")
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
			allowed := false
			for _, prefix := range allowedPrefixes {
				if path == prefix || strings.HasPrefix(path, prefix) {
					allowed = true
					break
				}
			}
			if !allowed {
				t.Errorf("%s imports %q; identity vocabulary allows only domain and stdlib", name, path)
			}
		}
	}
	if files == 0 {
		t.Fatal("no production Go files found to inspect")
	}
}
