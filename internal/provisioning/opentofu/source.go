// SPDX-License-Identifier: Apache-2.0

package opentofu

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/json"
	"github.com/zclconf/go-cty/cty"
)

const maxProviderPackageBytes = 1 << 30

var providerAddressSegment = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)

type SourceLimits struct {
	MaxFiles      int
	MaxFileBytes  int64
	MaxTotalBytes int64
	MaxPathBytes  int
}

type sourceFile struct {
	rel  string
	mode os.FileMode
	size int64
}

func SourceDigest(root string, limits SourceLimits) (string, error) {
	files, err := inspectSource(root, limits)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, file := range files {
		writeFrame(h, []byte(file.rel))
		contents, err := os.ReadFile(filepath.Join(root, file.rel))
		if err != nil {
			return "", err
		}
		writeFrame(h, contents)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeFrame(w io.Writer, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = w.Write(length[:])
	_, _ = w.Write(value)
}

func inspectSource(root string, limits SourceLimits) ([]sourceFile, error) {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("source must be a real directory")
	}
	var files []sourceFile
	var total int64
	precedence := map[string]string{}
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || len(rel) > limits.MaxPathBytes || rel == "." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("source path exceeds its trust boundary")
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return fmt.Errorf("source contains a symlink or special file")
		}
		if forbiddenSourcePath(rel, info.IsDir()) {
			return fmt.Errorf("source contains generated or mutable OpenTofu data")
		}
		if info.IsDir() {
			return nil
		}
		if info.Size() > limits.MaxFileBytes {
			return fmt.Errorf("source file exceeds the size bound")
		}
		total += info.Size()
		if total > limits.MaxTotalBytes || len(files)+1 > limits.MaxFiles {
			return fmt.Errorf("source exceeds aggregate bounds")
		}
		if kind, base, config := configFileKind(rel); config {
			key := filepath.Join(filepath.Dir(rel), base+":"+kind)
			extension := configExtension(rel)
			if previous, found := precedence[key]; found && previous != extension {
				return fmt.Errorf("source contains ambiguous same-basename OpenTofu configuration")
			}
			precedence[key] = extension
			if err := inspectConfig(path, extension); err != nil {
				return err
			}
		}
		files = append(files, sourceFile{rel: rel, mode: info.Mode(), size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("source is empty")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].rel < files[j].rel })
	return files, nil
}

func configExtension(path string) string {
	lower := strings.ToLower(path)
	for _, extension := range []string{".tofu.json", ".tf.json", ".tofu", ".tf"} {
		if strings.HasSuffix(lower, extension) {
			return extension
		}
	}
	return ""
}

func configFileKind(path string) (kind, base string, ok bool) {
	extension := configExtension(path)
	if extension == "" {
		return "", "", false
	}
	name := filepath.Base(path)
	if strings.HasSuffix(extension, ".json") {
		return "json", name[:len(name)-len(extension)], true
	}
	return "native", name[:len(name)-len(extension)], true
}

func forbiddenSourcePath(rel string, directory bool) bool {
	clean := filepath.ToSlash(rel)
	base := strings.ToLower(filepath.Base(clean))
	parts := strings.Split(clean, "/")
	for _, part := range parts {
		switch strings.ToLower(part) {
		case ".terraform", ".git", ".hg", ".svn":
			return true
		}
		if strings.HasPrefix(strings.ToLower(part), ".liftr") || strings.HasPrefix(strings.ToLower(part), "liftr.generated") {
			return true
		}
	}
	if directory {
		return false
	}
	return strings.Contains(base, ".tfstate") || strings.HasSuffix(base, ".tfplan") || isOverrideConfig(base) ||
		base == "terraform.tfvars" || strings.HasSuffix(base, ".auto.tfvars") || strings.HasSuffix(base, ".auto.tfvars.json") || base == "errored.tfstate"
}

func isOverrideConfig(base string) bool {
	for _, extension := range []string{".tf", ".tofu", ".tf.json", ".tofu.json"} {
		if base == "override"+extension || strings.HasSuffix(base, "_override"+extension) {
			return true
		}
	}
	return false
}

func inspectConfig(path, extension string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var file *hcl.File
	var diagnostics hcl.Diagnostics
	if strings.HasSuffix(extension, ".json") {
		file, diagnostics = json.Parse(raw, path)
	} else {
		file, diagnostics = hclsyntax.ParseConfig(raw, path, hcl.Pos{Line: 1, Column: 1})
	}
	if diagnostics.HasErrors() {
		return fmt.Errorf("source contains malformed OpenTofu configuration")
	}
	return inspectBody(file.Body)
}

var rootConfigSchema = &hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{
	{Type: "terraform"}, {Type: "module", LabelNames: []string{"name"}},
	{Type: "resource", LabelNames: []string{"type", "name"}}, {Type: "data", LabelNames: []string{"type", "name"}},
	{Type: "variable", LabelNames: []string{"name"}}, {Type: "output", LabelNames: []string{"name"}},
	{Type: "ephemeral", LabelNames: []string{"type", "name"}}, {Type: "provider", LabelNames: []string{"name"}},
	{Type: "check", LabelNames: []string{"name"}},
	{Type: "import"}, {Type: "removed"}, {Type: "action", LabelNames: []string{"type", "name"}},
	{Type: "backend", LabelNames: []string{"type"}}, {Type: "provisioner", LabelNames: []string{"type"}},
}}

func inspectBody(body hcl.Body) error {
	content, _, diagnostics := body.PartialContent(rootConfigSchema)
	if diagnostics.HasErrors() {
		return fmt.Errorf("source contains invalid OpenTofu block structure")
	}
	for _, block := range content.Blocks {
		switch block.Type {
		case "provisioner", "import", "removed", "action", "backend", "check":
			return fmt.Errorf("source contains unsupported %s block", block.Type)
		case "module":
			moduleContent, _, moduleDiagnostics := block.Body.PartialContent(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "source", Required: true}}})
			if moduleDiagnostics.HasErrors() {
				return fmt.Errorf("module source is required")
			}
			attribute := moduleContent.Attributes["source"]
			value, diagnostics := attribute.Expr.Value(nil)
			if diagnostics.HasErrors() || value.Type() != cty.String || !isLocalModuleSource(value.AsString()) {
				return fmt.Errorf("remote or dynamic module source is unsupported")
			}
		case "resource":
			if hasNestedBlock(block.Body, "provisioner", []string{"type"}) {
				return fmt.Errorf("source contains unsupported provisioner block")
			}
		case "variable", "output":
			if ephemeral, invalid := ephemeralSetting(block.Body); ephemeral || invalid {
				return fmt.Errorf("source contains unsupported ephemeral %s", block.Type)
			}
		case "ephemeral":
			return fmt.Errorf("source contains unsupported ephemeral resource")
		case "terraform":
			if hasNestedBlock(block.Body, "backend", []string{"type"}) {
				return fmt.Errorf("source contains unsupported backend block")
			}
			if hasNestedBlock(block.Body, "encryption", nil) {
				return fmt.Errorf("source contains unsupported encryption block")
			}
		}
	}
	return nil
}

func hasNestedBlock(body hcl.Body, blockType string, labels []string) bool {
	content, _, diagnostics := body.PartialContent(&hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: blockType, LabelNames: labels}}})
	return diagnostics.HasErrors() || len(content.Blocks) != 0
}

func ephemeralSetting(body hcl.Body) (enabled, invalid bool) {
	content, _, diagnostics := body.PartialContent(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "ephemeral"}}})
	if diagnostics.HasErrors() {
		return false, true
	}
	attribute, found := content.Attributes["ephemeral"]
	if !found {
		return false, false
	}
	value, valueDiagnostics := attribute.Expr.Value(nil)
	if valueDiagnostics.HasErrors() || !value.IsKnown() || value.IsNull() || value.Type() != cty.Bool {
		return false, true
	}
	return value.True(), false
}

func isLocalModuleSource(source string) bool {
	if !strings.HasPrefix(source, "./") {
		return false
	}
	clean := filepath.Clean(source)
	return clean != "." && !filepath.IsAbs(clean) && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func validateDependencySupplyChain(program Program) error {
	lockPath := filepath.Join(program.SourceDir, ".terraform.lock.hcl")
	if program.BuiltInOnly {
		if len(program.ProviderConstraints) != 0 || len(program.ProviderPackages) != 0 || program.ProviderMirror != "" {
			return fmt.Errorf("built-in-only OpenTofu program cannot register external providers")
		}
		if _, err := os.Lstat(lockPath); err == nil {
			return fmt.Errorf("built-in-only OpenTofu program must not carry a dependency lock")
		}
		if err := validateBuiltInOnlySource(program.SourceDir); err != nil {
			return err
		}
		return nil
	}
	if len(program.ProviderConstraints) == 0 || !filepath.IsAbs(program.ProviderMirror) {
		return fmt.Errorf("external providers require exact constraints and an offline mirror")
	}
	mirrorInfo, err := os.Lstat(program.ProviderMirror)
	if err != nil || !mirrorInfo.IsDir() || mirrorInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("external provider mirror is unavailable")
	}
	declared, err := declaredProviderConstraints(program.SourceDir)
	if err != nil || len(declared) != len(program.ProviderConstraints) {
		return fmt.Errorf("source provider constraints do not match registration")
	}
	for address, version := range program.ProviderConstraints {
		if declared[address] != version {
			return fmt.Errorf("source provider constraints do not match registration")
		}
	}
	lockInfo, err := os.Lstat(lockPath)
	if err != nil || !lockInfo.Mode().IsRegular() || lockInfo.Mode()&os.ModeSymlink != 0 || lockInfo.Mode().Perm()&0o222 != 0 {
		return fmt.Errorf("external providers require an immutable dependency lock")
	}
	locked, err := parseDependencyLock(lockPath)
	if err != nil {
		return err
	}
	if len(locked) != len(program.ProviderConstraints) {
		return fmt.Errorf("dependency lock provider set does not match registration")
	}
	for address, exact := range program.ProviderConstraints {
		if strings.TrimSpace(exact) == "" || strings.ContainsAny(exact, "<>=~! ,") || locked[address].Version != exact {
			return fmt.Errorf("dependency lock selection does not match exact registration constraint")
		}
	}
	if len(program.ProviderPackages) != len(program.ProviderConstraints) {
		return fmt.Errorf("every external provider requires one verified mirror package")
	}
	seenPackages := map[string]bool{}
	for _, pkg := range program.ProviderPackages {
		selection, selected := locked[pkg.Address]
		parts := strings.Split(pkg.Address, "/")
		if len(parts) != 3 {
			return fmt.Errorf("provider package registration is invalid")
		}
		expectedPath := filepath.Join(program.ProviderMirror, parts[0], parts[1], parts[2], "terraform-provider-"+parts[2]+"_"+pkg.Version+"_"+runtime.GOOS+"_"+runtime.GOARCH+".zip")
		if !selected || selection.Version != pkg.Version || seenPackages[pkg.Address] || pkg.Path != expectedPath || !validSHA256(pkg.SHA256) {
			return fmt.Errorf("provider package registration is invalid")
		}
		seenPackages[pkg.Address] = true
		if err := validateRealPath(program.ProviderMirror, pkg.Path); err != nil {
			return fmt.Errorf("provider package is unavailable")
		}
		digest, err := digestFile(pkg.Path, maxProviderPackageBytes)
		if err != nil || !strings.EqualFold(digest, pkg.SHA256) {
			return fmt.Errorf("provider package digest mismatch")
		}
		if !selection.Hashes["zh:"+strings.ToLower(pkg.SHA256)] {
			return fmt.Errorf("provider package digest is absent from dependency lock")
		}
	}
	return nil
}

func digestFile(path string, maxBytes int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxBytes {
		return "", fmt.Errorf("file exceeds digest bound")
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, maxBytes+1))
	if err != nil || written != info.Size() || written > maxBytes {
		return "", fmt.Errorf("file changed or exceeds digest bound")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func validateRealPath(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes mirror")
	}
	current := root
	parts := strings.Split(rel, string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || index < len(parts)-1 && !info.IsDir() || index == len(parts)-1 && !info.Mode().IsRegular() {
			return fmt.Errorf("mirror path is not real")
		}
	}
	return nil
}

type dependencySelection struct {
	Version string
	Hashes  map[string]bool
}

func parseDependencyLock(path string) (map[string]dependencySelection, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("dependency lock is unavailable")
	}
	file, diagnostics := hclsyntax.ParseConfig(raw, path, hcl.Pos{Line: 1, Column: 1})
	if diagnostics.HasErrors() {
		return nil, fmt.Errorf("dependency lock is malformed")
	}
	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("dependency lock is malformed")
	}
	result := map[string]dependencySelection{}
	for _, block := range body.Blocks {
		if block.Type != "provider" || len(block.Labels) != 1 {
			return nil, fmt.Errorf("dependency lock contains unsupported content")
		}
		versionAttr, ok := block.Body.Attributes["version"]
		if !ok {
			return nil, fmt.Errorf("dependency lock selection is incomplete")
		}
		value, diagnostics := versionAttr.Expr.Value(nil)
		if diagnostics.HasErrors() || value.Type() != cty.String {
			return nil, fmt.Errorf("dependency lock selection is invalid")
		}
		if _, duplicate := result[block.Labels[0]]; duplicate {
			return nil, fmt.Errorf("dependency lock selection is invalid")
		}
		hashesAttr, ok := block.Body.Attributes["hashes"]
		if !ok {
			return nil, fmt.Errorf("dependency lock checksum is required")
		}
		hashValues, diagnostics := hashesAttr.Expr.Value(nil)
		if diagnostics.HasErrors() || !hashValues.CanIterateElements() {
			return nil, fmt.Errorf("dependency lock checksum is invalid")
		}
		hashes := map[string]bool{}
		iterator := hashValues.ElementIterator()
		for iterator.Next() {
			_, item := iterator.Element()
			if item.Type() != cty.String {
				return nil, fmt.Errorf("dependency lock checksum is invalid")
			}
			hashes[strings.ToLower(item.AsString())] = true
		}
		if len(hashes) == 0 {
			return nil, fmt.Errorf("dependency lock checksum is required")
		}
		result[block.Labels[0]] = dependencySelection{Version: value.AsString(), Hashes: hashes}
	}
	return result, nil
}

func validateBuiltInOnlySource(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || configExtension(path) == "" {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		file, diagnostics := parseConfig(raw, path)
		if diagnostics.HasErrors() {
			return fmt.Errorf("source contains malformed OpenTofu configuration")
		}
		return walkBuiltInBody(file.Body)
	})
}

func declaredProviderConstraints(root string) (map[string]string, error) {
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || configExtension(path) == "" {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		file, diagnostics := parseConfig(raw, path)
		if diagnostics.HasErrors() {
			return fmt.Errorf("source contains malformed OpenTofu configuration")
		}
		content, _, contentDiagnostics := file.Body.PartialContent(&hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{{Type: "terraform"}}})
		if contentDiagnostics.HasErrors() {
			return fmt.Errorf("source contains invalid terraform block")
		}
		for _, block := range content.Blocks {
			if block.Type != "terraform" {
				continue
			}
			terraformContent, _, terraformDiagnostics := block.Body.PartialContent(&hcl.BodySchema{Attributes: []hcl.AttributeSchema{{Name: "required_providers"}}})
			if terraformDiagnostics.HasErrors() {
				return fmt.Errorf("required_providers is invalid")
			}
			attribute, found := terraformContent.Attributes["required_providers"]
			if !found {
				continue
			}
			declarations, valueDiagnostics := attribute.Expr.Value(nil)
			if valueDiagnostics.HasErrors() || !declarations.IsKnown() || declarations.IsNull() || !(declarations.Type().IsObjectType() || declarations.Type().IsMapType()) {
				return fmt.Errorf("provider constraints must be literal")
			}
			iterator := declarations.ElementIterator()
			for iterator.Next() {
				aliasValue, declaration := iterator.Element()
				alias := aliasValue.AsString()
				source, ok := ctyStringField(declaration, "source")
				if !ok {
					source = "hashicorp/" + alias
				}
				version, ok := ctyStringField(declaration, "version")
				if !ok && declaration.Type() == cty.String {
					version, ok = declaration.AsString(), true
				}
				if !ok {
					return fmt.Errorf("provider version constraint is required")
				}
				version = strings.TrimSpace(version)
				if strings.HasPrefix(version, "=") {
					version = strings.TrimSpace(strings.TrimPrefix(version, "="))
				}
				if version == "" || strings.ContainsAny(version, "<>=~! ,") {
					return fmt.Errorf("provider version constraint is not exact")
				}
				address := normalizeProviderAddress(source)
				if address == "" || result[address] != "" && result[address] != version {
					return fmt.Errorf("provider source is invalid or has conflicting constraints")
				}
				result[address] = version
			}
		}
		return nil
	})
	return result, err
}

func parseConfig(raw []byte, path string) (*hcl.File, hcl.Diagnostics) {
	if strings.HasSuffix(configExtension(path), ".json") {
		return json.Parse(raw, path)
	}
	return hclsyntax.ParseConfig(raw, path, hcl.Pos{Line: 1, Column: 1})
}

func ctyStringField(value cty.Value, name string) (string, bool) {
	if !value.IsKnown() || value.IsNull() {
		return "", false
	}
	var field cty.Value
	switch {
	case value.Type().IsObjectType() && value.Type().HasAttribute(name):
		field = value.GetAttr(name)
	case value.Type().IsMapType():
		key := cty.StringVal(name)
		hasIndex := value.HasIndex(key)
		if !hasIndex.IsKnown() || !hasIndex.True() {
			return "", false
		}
		field = value.Index(key)
	default:
		return "", false
	}
	if !field.IsKnown() || field.IsNull() || field.Type() != cty.String {
		return "", false
	}
	return field.AsString(), true
}

func normalizeProviderAddress(source string) string {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(source)), "/")
	if len(parts) == 2 {
		parts = append([]string{"registry.opentofu.org"}, parts...)
	}
	if len(parts) != 3 || !validProviderHostname(parts[0]) || !providerAddressSegment.MatchString(parts[1]) || !providerAddressSegment.MatchString(parts[2]) {
		return ""
	}
	return strings.Join(parts, "/")
}

func validProviderHostname(host string) bool {
	if host == "" || strings.Contains(host, "..") {
		return false
	}
	for _, part := range strings.Split(host, ".") {
		if !providerAddressSegment.MatchString(part) {
			return false
		}
	}
	return true
}

func walkBuiltInBody(body hcl.Body) error {
	content, _, diagnostics := body.PartialContent(&hcl.BodySchema{Blocks: []hcl.BlockHeaderSchema{
		{Type: "provider", LabelNames: []string{"name"}}, {Type: "data", LabelNames: []string{"type", "name"}},
		{Type: "resource", LabelNames: []string{"type", "name"}}, {Type: "terraform"},
	}})
	if diagnostics.HasErrors() {
		return fmt.Errorf("source contains invalid provider configuration")
	}
	for _, block := range content.Blocks {
		if block.Type == "provider" || block.Type == "data" || block.Type == "resource" && block.Labels[0] != "terraform_data" {
			return fmt.Errorf("built-in-only OpenTofu source references an external provider")
		}
		if block.Type == "terraform" {
			terraformContent, _, terraformDiagnostics := block.Body.PartialContent(&hcl.BodySchema{
				Attributes: []hcl.AttributeSchema{{Name: "required_providers"}},
				Blocks:     []hcl.BlockHeaderSchema{{Type: "encryption"}},
			})
			if terraformDiagnostics.HasErrors() {
				return fmt.Errorf("built-in-only OpenTofu provider declaration is invalid")
			}
			if _, found := terraformContent.Attributes["required_providers"]; found {
				return fmt.Errorf("built-in-only OpenTofu source declares external providers")
			}
			if len(terraformContent.Blocks) != 0 {
				return fmt.Errorf("built-in-only OpenTofu source configures encryption")
			}
		}
	}
	return nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && !filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
