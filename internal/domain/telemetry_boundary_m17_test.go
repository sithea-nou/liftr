// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCorePackagesAreTelemetryFree pins the M17 dependency rule (ADR-0018):
// the core domain packages — and every package that executes lifecycle or
// provisioning logic — never import a telemetry library. Telemetry is
// injected at composition through consumer-side ports; only
// internal/observability and cmd/liftr-server touch OpenTelemetry,
// Prometheus, or log/slog directly.
func TestCorePackagesAreTelemetryFree(t *testing.T) {
	forbiddenAny := []string{
		"go.opentelemetry.io/",
		"github.com/prometheus/",
		"log/slog",
	}
	repoRoot := "../.."
	packages := []string{
		"internal/domain",
		"internal/lifecycle",
		"internal/resourcecontract",
		"internal/identity",
		"internal/application",
		"internal/worker",
		"internal/provisioning",
	}
	for _, pkg := range packages {
		dir := filepath.Join(repoRoot, pkg)
		assertPackageTelemetryFree(t, dir, forbiddenAny)
	}
}

func assertPackageTelemetryFree(t *testing.T, dir string, forbidden []string) {
	t.Helper()
	fileSet := token.NewFileSet()
	files := 0
	walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if base := filepath.Base(path); strings.HasPrefix(base, ".") || base == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		name := info.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		source, parseErr := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}
		files++
		for _, imported := range source.Imports {
			importPath := strings.Trim(imported.Path.Value, `"`)
			for _, bad := range forbidden {
				if strings.HasPrefix(importPath, bad) || importPath == bad {
					t.Errorf("%s imports telemetry package %s", path, importPath)
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	if files == 0 {
		t.Fatalf("no production Go files found under %s", dir)
	}
}
