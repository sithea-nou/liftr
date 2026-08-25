// SPDX-License-Identifier: Apache-2.0

package opentofu

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuiltInOnlyNeedsNoLockAndHasNoNetworkInstallation(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.tf"), []byte(`resource "terraform_data" "control" { input = "ok" }`), 0o600); err != nil {
		t.Fatal(err)
	}
	program := Program{SourceDir: root, BuiltInOnly: true}
	if err := validateDependencySupplyChain(program); err != nil {
		t.Fatalf("built-in validation: %v", err)
	}
	config := builtInCLIConfiguration(filepath.Join(root, "empty-mirror"))
	if strings.Contains(config, "direct") || !strings.Contains(config, "filesystem_mirror") || strings.Contains(config, "registry") {
		t.Fatalf("built-in CLI config does not fail closed to an empty mirror: %s", config)
	}
}

func TestExternalProviderRequiresValidImmutableLockAndMirrorDigest(t *testing.T) {
	root := t.TempDir()
	mirror := filepath.Join(root, "mirror")
	packageDir := filepath.Join(mirror, "registry.opentofu.org", "example", "test")
	if err := os.MkdirAll(packageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.tf"), []byte(`terraform {
  required_providers = {
    test = {
      source = "example/test"
      version = "= 1.2.3"
    }
  }
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	packagePath := filepath.Join(packageDir, "terraform-provider-test_1.2.3_"+runtime.GOOS+"_"+runtime.GOARCH+".zip")
	packageBytes := []byte("verified package")
	if err := os.WriteFile(packagePath, packageBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(packageBytes)
	digestHex := hex.EncodeToString(digest[:])
	program := Program{SourceDir: root, ProviderMirror: mirror, ProviderConstraints: map[string]string{"registry.opentofu.org/example/test": "1.2.3"}, ProviderPackages: []ProviderPackage{{Address: "registry.opentofu.org/example/test", Version: "1.2.3", Path: packagePath, SHA256: digestHex}}}
	if err := validateDependencySupplyChain(program); err == nil {
		t.Fatal("external provider without dependency lock was accepted")
	}
	lock := `provider "registry.opentofu.org/example/test" {
  version = "1.2.3"
  hashes = ["zh:` + digestHex + `"]
}
`
	lockPath := filepath.Join(root, ".terraform.lock.hcl")
	if err := os.WriteFile(lockPath, []byte(lock), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := validateDependencySupplyChain(program); err != nil {
		t.Fatalf("valid external supply chain: %v", err)
	}
	declared, err := declaredProviderConstraints(root)
	if err != nil || declared["registry.opentofu.org/example/test"] != "1.2.3" {
		t.Fatalf("required_providers attribute was not inspected: %#v, %v", declared, err)
	}
	config := cliConfiguration(program)
	if strings.Contains(config, "direct") || !strings.Contains(config, `"registry.opentofu.org/example/test"`) || strings.Contains(config, `"*/*"`) {
		t.Fatalf("CLI config is not exact and offline: %s", config)
	}
	program.ProviderPackages[0].SHA256 = strings.Repeat("0", 64)
	if err := validateDependencySupplyChain(program); err == nil {
		t.Fatal("invalid provider package digest was accepted")
	}
}

func TestCustomHostMirrorHasNoNetworkFallback(t *testing.T) {
	program := Program{ProviderMirror: "/private/mirror", ProviderConstraints: map[string]string{
		"providers.example.internal/acme/widget": "2.0.0",
	}}
	config := cliConfiguration(program)
	if strings.Contains(config, "direct") || !strings.Contains(config, `"providers.example.internal/acme/widget"`) {
		t.Fatalf("custom provider host is not mirror-only: %s", config)
	}
}

func TestRequiredProvidersJSONAttributeIsInspected(t *testing.T) {
	root := t.TempDir()
	raw := `{"terraform":{"required_providers":{"widget":{"source":"providers.example.internal/acme/widget","version":"= 2.0.0"}}}}`
	if err := os.WriteFile(filepath.Join(root, "providers.tofu.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	declared, err := declaredProviderConstraints(root)
	if err != nil || len(declared) != 1 || declared["providers.example.internal/acme/widget"] != "2.0.0" {
		t.Fatalf("JSON required_providers was not inspected: %#v, %v", declared, err)
	}
}

func TestSourceAdmissionCoversAllConfigExtensions(t *testing.T) {
	limits := SourceLimits{MaxFiles: 8, MaxFileBytes: 4096, MaxTotalBytes: 16384, MaxPathBytes: 128}
	for _, test := range []struct {
		name, file, content string
	}{
		{"tofu backend", "main.tofu", `terraform { backend "local" {} }`},
		{"tf json remote module", "main.tf.json", `{"module":{"bad":{"source":"registry.example/a/b"}}}`},
		{"tofu json provisioner", "main.tofu.json", `{"resource":{"terraform_data":{"x":{"provisioner":{"local-exec":{"command":"true"}}}}}}`},
		{"json ephemeral resource", "main.tf.json", `{"ephemeral":{"random_password":{"x":{}}}}`},
		{"json ephemeral output", "main.tofu.json", `{"output":{"x":{"value":"x","ephemeral":true}}}`},
		{"encryption", "main.tofu", `terraform { encryption { key_provider "external" "x" { command = ["sh"] } } }`},
		{"nested check data", "main.tf", `check "external" { data "http" "x" { url = "https://example.test" } assert { condition = true error_message = "x" } }`},
		{"json encryption", "encryption.tofu.json", `{"terraform":{"encryption":{"key_provider":{"external":{"x":{"command":["sh"]}}}}}}`},
		{"json nested check data", "check.tf.json", `{"check":{"external":{"data":{"http":{"x":{"url":"https://example.test"}}},"assert":{"condition":true,"error_message":"x"}}}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, test.file), []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := SourceDigest(root, limits); err == nil {
				t.Fatalf("unsafe %s configuration was accepted", test.file)
			}
		})
	}
}

func TestSourceAdmissionRejectsSameBasenamePrecedence(t *testing.T) {
	for _, files := range [][]string{{"main.tf", "main.tofu"}, {"main.tf.json", "main.tofu.json"}} {
		root := t.TempDir()
		for _, name := range files {
			content := `resource "terraform_data" "x" {}`
			if strings.HasSuffix(name, ".json") {
				content = `{"resource":{"terraform_data":{"x":{}}}}`
			}
			if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := SourceDigest(root, SourceLimits{MaxFiles: 4, MaxFileBytes: 1024, MaxTotalBytes: 4096, MaxPathBytes: 100}); err == nil {
			t.Fatalf("ambiguous precedence was accepted for %v", files)
		}
	}
}

func TestBuiltInOnlyRejectsExternalJSONConfiguration(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.tofu.json"), []byte(`{"data":{"http":{"x":{"url":"https://example.test"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateDependencySupplyChain(Program{SourceDir: root, BuiltInOnly: true}); err == nil {
		t.Fatal("external JSON data source bypassed built-in-only admission")
	}
}

func TestEnvironmentIsConstructedFromEmptyAndReservedNamesRejected(t *testing.T) {
	t.Setenv("INHERITED_CANARY", "must-not-leak")
	environment := strings.Join(baseEnvironment("/opt/tofu", "/private/tofurc", "/private/tmp", map[string]string{"TF_HTTP_USERNAME": "controlled"}), "\n")
	if strings.Contains(environment, "INHERITED_CANARY") {
		t.Fatal("ambient environment leaked")
	}
	if !strings.Contains(environment, "TF_HTTP_USERNAME=controlled") {
		t.Fatal("controlled credential environment was omitted")
	}
	if err := validateNames([]string{"PATH"}); err == nil {
		t.Fatal("reserved environment override was accepted")
	}
	if err := validateProgramEnvironmentNames([]string{"TF_ENCRYPTION"}); err == nil {
		t.Fatal("OpenTofu encryption environment override was accepted")
	}
	if err := validateBackendEnvironmentNames([]string{"TF_HTTP_ADDRESS"}); err == nil {
		t.Fatal("backend address environment override was accepted")
	}
	if err := validateBackendEnvironmentNames([]string{"TF_HTTP_PASSWORD"}); err != nil {
		t.Fatalf("backend credential environment was rejected: %v", err)
	}
}

func TestMachineFailureClassificationIsNarrow(t *testing.T) {
	version := machineResult(0).Stdout
	diagnostic := func(summary string) []byte {
		return append(append([]byte(nil), version...), []byte(`{"type":"diagnostic","diagnostic":{"severity":"error","summary":"`+summary+`"}}`+"\n")...)
	}
	if !deterministicMachineFailure(diagnostic("Invalid value for input variable")) {
		t.Fatal("known semantic input rejection was not deterministic")
	}
	if !deterministicMachineFailure(diagnostic("Invalid value for variable")) {
		t.Fatal("known custom variable validation rejection was not deterministic")
	}
	if deterministicMachineFailure(diagnostic("Error acquiring the state lock")) || deterministicMachineFailure(diagnostic("Failed to get existing workspaces")) {
		t.Fatal("backend or lock diagnostic was treated as deterministic")
	}
}

func TestMachineUIRejectsMalformedAndOversizedLines(t *testing.T) {
	if err := validateMachineUI([]byte("not-json\n")); err == nil {
		t.Fatal("malformed machine UI was accepted")
	}
	if err := validateMachineUI([]byte("plain text\n" + string(machineResult(0).Stdout))); err == nil {
		t.Fatal("untyped content before the machine UI version was accepted")
	}
	if err := validateMachineUI(append(machineResult(0).Stdout, []byte("OpenTofu initialized successfully\n")...)); err != nil {
		t.Fatalf("OpenTofu 1.12.6 post-version init text was rejected: %v", err)
	}
	huge := []byte(`{"type":"version","tofu":"1.12.6","ui":"1.2","padding":"` + strings.Repeat("x", (1<<20)+1) + `"}`)
	if err := validateMachineUI(huge); err == nil {
		t.Fatal("oversized machine UI event was accepted")
	}
}
