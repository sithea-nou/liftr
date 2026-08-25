// SPDX-License-Identifier: Apache-2.0

package opentofu

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
)

const (
	EngineVersion        = "1.12.6"
	StateKeyVersionV1    = "v1"
	maxExecutableBytes   = 512 << 20
	defaultMaxOutput     = 4 << 20
	defaultMaxState      = 16 << 20
	defaultMaxSourceFile = 1 << 20
	defaultMaxSourceAll  = 8 << 20
	defaultMaxFiles      = 256
	defaultMaxPath       = 512
)

type EnvironmentProvider func(context.Context) (map[string]string, error)

type Input struct {
	OperationID      domain.OperationID
	AttemptNumber    uint64
	ResourceID       domain.ResourceID
	ResourceType     domain.ResourceTypeRef
	Capability       domain.Capability
	Spec             domain.ResourceSpec
	TargetGeneration uint64
	DesiredPresent   bool
}

type InputEncoder func(Input) (map[string]any, error)

// OutputMapping freezes the one private output envelope selected for an
// execution. Fields maps developer-contract field names to envelope field
// names; both sets must be unique.
type OutputMapping struct {
	Ref                        string
	EnvelopeName               string
	Fields                     map[string]string
	CompatibleSourceMappingRef string
}

type ProviderPackage struct {
	Address string
	Version string
	Path    string
	SHA256  string
}

type Program struct {
	Ref                      string
	ResourceType             domain.ResourceTypeRef
	Capabilities             []domain.Capability
	SourceDir                string
	SourceDigest             string
	BuiltInOnly              bool
	ProviderConstraints      map[string]string
	ProviderPackages         []ProviderPackage
	ProviderMirror           string
	EncodeInput              InputEncoder
	RequiredEnvironment      []string
	ControlMarkerAddress     string
	ManagedWorkloadAddresses []string
	OutputMappings           []OutputMapping
	CurrentOutputMappingRef  string
	MaxSourceFiles           int
	MaxSourceFileBytes       int64
	MaxSourceBytes           int64
	MaxSourcePathBytes       int
}

type BackendProfile struct {
	Ref                 string
	StateURL            string
	LockURL             string
	UnlockURL           string
	RequiredEnvironment []string
	DevelopmentLocal    bool
	LocalStateRoot      string
}

type Registration struct {
	ProvisionerRef  string
	Identity        string
	StateKeyVersion string
	Program         Program
	Backend         BackendProfile
	Environment     EnvironmentProvider
}

type Config struct {
	Executable                string
	ExecutableSHA256          string
	WorkRoot                  string
	QuarantineRoot            string
	Registration              Registration
	Evidence                  EvidenceStore
	Runner                    CommandRunner
	LockTimeout               time.Duration
	MaxCommandOutput          int64
	MaxStateBytes             int64
	AllowInsecureHTTPForTests bool
	// BeforeApply is a test-only crash injection point after ApplyMayStart is
	// durable and immediately before process spawn.
	BeforeApply func() error
}

type validatedConfig struct {
	Config
	program      Program
	capabilities []provisioning.ProvisionerCapability
	mappings     map[string]OutputMapping
}

func (c Config) validate(ctx context.Context) (validatedConfig, error) {
	if runtime.GOOS == "windows" {
		return validatedConfig{}, fmt.Errorf("OpenTofu startup requires safe process-tree cancellation")
	}
	if c.Evidence == nil {
		return validatedConfig{}, fmt.Errorf("OpenTofu evidence store is required")
	}
	if !filepath.IsAbs(c.Executable) {
		return validatedConfig{}, fmt.Errorf("OpenTofu executable must be absolute")
	}
	baseName := strings.ToLower(filepath.Base(c.Executable))
	if baseName != "tofu" && baseName != "tofu.exe" && baseName != "opentofu" && baseName != "opentofu.exe" {
		return validatedConfig{}, fmt.Errorf("configured executable is not an OpenTofu binary")
	}
	info, err := os.Lstat(c.Executable)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 {
		return validatedConfig{}, fmt.Errorf("OpenTofu executable is unavailable or not executable")
	}
	if !validSHA256(c.ExecutableSHA256) {
		return validatedConfig{}, fmt.Errorf("OpenTofu executable SHA-256 digest is required")
	}
	actualDigest, digestErr := digestFile(c.Executable, maxExecutableBytes)
	if digestErr != nil || !strings.EqualFold(actualDigest, c.ExecutableSHA256) {
		return validatedConfig{}, fmt.Errorf("OpenTofu executable digest mismatch")
	}
	if err := validatePrivateRoots(c.WorkRoot, c.QuarantineRoot); err != nil {
		return validatedConfig{}, err
	}
	if c.Runner == nil {
		c.Executable, err = pinExecutable(c.Executable, c.WorkRoot, c.ExecutableSHA256)
		if err != nil {
			return validatedConfig{}, err
		}
		identity, err := buildinfo.ReadFile(c.Executable)
		if err != nil || identity.Main.Path != "github.com/opentofu/opentofu" {
			return validatedConfig{}, fmt.Errorf("configured executable is not an official OpenTofu build")
		}
	}
	if c.Runner == nil {
		c.Runner = OSCommandRunner{}
	}
	if c.MaxCommandOutput <= 0 {
		c.MaxCommandOutput = defaultMaxOutput
	}
	if c.MaxStateBytes <= 0 {
		c.MaxStateBytes = defaultMaxState
	}
	if c.LockTimeout <= 0 {
		c.LockTimeout = 30 * time.Second
	}
	if c.LockTimeout > 10*time.Minute {
		return validatedConfig{}, fmt.Errorf("OpenTofu lock timeout exceeds the bound")
	}
	if err := validateRegistration(c.Registration, c.AllowInsecureHTTPForTests); err != nil {
		return validatedConfig{}, err
	}
	program := cloneProgram(c.Registration.Program)
	applySourceDefaults(&program)
	digest, err := SourceDigest(program.SourceDir, sourceLimits(program))
	if err != nil {
		return validatedConfig{}, fmt.Errorf("validate OpenTofu source: %w", err)
	}
	if !strings.EqualFold(digest, program.SourceDigest) {
		return validatedConfig{}, fmt.Errorf("OpenTofu source digest does not match registration")
	}
	if err := validateDependencySupplyChain(program); err != nil {
		return validatedConfig{}, err
	}
	c.Registration.Program = program
	c.Registration.Backend.RequiredEnvironment = append([]string(nil), c.Registration.Backend.RequiredEnvironment...)
	result := validatedConfig{Config: c, program: program, mappings: make(map[string]OutputMapping)}
	for _, capability := range program.Capabilities {
		result.capabilities = append(result.capabilities, provisioning.ProvisionerCapability{ResourceType: program.ResourceType, Capability: capability})
	}
	sort.Slice(result.capabilities, func(i, j int) bool { return result.capabilities[i].Capability < result.capabilities[j].Capability })
	for _, mapping := range program.OutputMappings {
		result.mappings[mapping.Ref] = cloneMapping(mapping)
	}
	version, err := c.Runner.Run(ctx, Command{Path: c.Executable, Args: []string{"version", "-json"}, Env: baseEnvironment(c.Executable, "", ""), MaxOutputBytes: c.MaxCommandOutput})
	if err != nil || version.ExitCode != 0 || version.Overflow {
		return validatedConfig{}, fmt.Errorf("validate OpenTofu executable identity")
	}
	var identity struct {
		Version string `json:"terraform_version"`
	}
	if json.Unmarshal(version.Stdout, &identity) != nil || identity.Version != EngineVersion {
		return validatedConfig{}, fmt.Errorf("OpenTofu executable must be exactly version %s", EngineVersion)
	}
	if err := scanOrphanWorkspaces(c.WorkRoot, c.QuarantineRoot); err != nil {
		return validatedConfig{}, err
	}
	return result, nil
}

func validateRegistration(reg Registration, allowInsecure bool) error {
	if strings.TrimSpace(reg.ProvisionerRef) == "" || strings.TrimSpace(reg.Identity) == "" {
		return fmt.Errorf("OpenTofu registration identity and provisioner reference are required")
	}
	if reg.StateKeyVersion != StateKeyVersionV1 {
		return fmt.Errorf("unsupported OpenTofu state key version %q", reg.StateKeyVersion)
	}
	p := reg.Program
	if strings.TrimSpace(p.Ref) == "" || strings.TrimSpace(p.ResourceType.Name) == "" || strings.TrimSpace(p.ResourceType.Version) == "" ||
		!filepath.IsAbs(p.SourceDir) || strings.TrimSpace(p.SourceDigest) == "" || p.EncodeInput == nil || strings.TrimSpace(p.ControlMarkerAddress) == "" {
		return fmt.Errorf("OpenTofu program registration is incomplete")
	}
	if len(p.Capabilities) == 0 {
		return fmt.Errorf("OpenTofu program capabilities are required")
	}
	addresses := map[string]bool{}
	for _, address := range append([]string{p.ControlMarkerAddress}, p.ManagedWorkloadAddresses...) {
		if strings.TrimSpace(address) == "" || addresses[address] {
			return fmt.Errorf("OpenTofu managed address registration is invalid")
		}
		addresses[address] = true
	}
	seen := map[domain.Capability]bool{}
	for _, capability := range p.Capabilities {
		if capability != domain.CapabilityCreate && capability != domain.CapabilityUpdate && capability != domain.CapabilityDelete || seen[capability] {
			return fmt.Errorf("OpenTofu program has an unsupported or duplicate capability")
		}
		seen[capability] = true
	}
	if err := validateProgramEnvironmentNames(p.RequiredEnvironment); err != nil {
		return err
	}
	if err := validateOutputMappings(p); err != nil {
		return err
	}
	b := reg.Backend
	if strings.TrimSpace(b.Ref) == "" {
		return fmt.Errorf("OpenTofu backend profile reference is required")
	}
	if err := validateBackendEnvironmentNames(b.RequiredEnvironment); err != nil {
		return err
	}
	if b.DevelopmentLocal {
		if !filepath.IsAbs(b.LocalStateRoot) {
			return fmt.Errorf("development local backend root must be absolute")
		}
		return validatePrivateDirectory(b.LocalStateRoot)
	}
	for name, raw := range map[string]string{"state": b.StateURL, "lock": b.LockURL, "unlock": b.UnlockURL} {
		u, err := url.Parse(raw)
		if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(allowInsecure && u.Scheme == "http")) {
			return fmt.Errorf("OpenTofu HTTP backend %s URL is invalid", name)
		}
	}
	if b.StateURL == b.LockURL || b.StateURL == b.UnlockURL || b.LockURL == b.UnlockURL {
		return fmt.Errorf("OpenTofu HTTP backend state and locking URLs must be distinct")
	}
	return nil
}

func validateOutputMappings(program Program) error {
	refs := map[string]bool{}
	compatible := map[string]bool{}
	for _, mapping := range program.OutputMappings {
		if strings.TrimSpace(mapping.Ref) == "" || strings.TrimSpace(mapping.EnvelopeName) == "" || len(mapping.Fields) == 0 || refs[mapping.Ref] {
			return fmt.Errorf("OpenTofu output mapping is incomplete or duplicated")
		}
		refs[mapping.Ref] = true
		values := map[string]bool{}
		for target, source := range mapping.Fields {
			if strings.TrimSpace(target) == "" || strings.TrimSpace(source) == "" || values[source] {
				return fmt.Errorf("OpenTofu output mapping fields are invalid")
			}
			values[source] = true
		}
		if mapping.CompatibleSourceMappingRef != "" {
			if mapping.CompatibleSourceMappingRef == mapping.Ref || compatible[mapping.CompatibleSourceMappingRef] {
				return fmt.Errorf("OpenTofu output recovery mapping is invalid")
			}
			compatible[mapping.CompatibleSourceMappingRef] = true
		}
	}
	if program.CurrentOutputMappingRef != "" && !refs[program.CurrentOutputMappingRef] {
		return fmt.Errorf("current OpenTofu output mapping is not registered")
	}
	if len(program.OutputMappings) > 0 && program.CurrentOutputMappingRef == "" {
		return fmt.Errorf("current OpenTofu output mapping is required")
	}
	return nil
}

func validateProgramEnvironmentNames(names []string) error {
	if err := validateNames(names); err != nil {
		return err
	}
	for _, name := range names {
		if strings.HasPrefix(name, "TF_") || strings.HasPrefix(name, "TOFU_") {
			return fmt.Errorf("OpenTofu program environment allowlist is invalid")
		}
	}
	return nil
}

func validateBackendEnvironmentNames(names []string) error {
	if err := validateNames(names); err != nil {
		return err
	}
	allowed := map[string]bool{
		"TF_HTTP_USERNAME": true, "TF_HTTP_PASSWORD": true,
		"TF_HTTP_CLIENT_CERTIFICATE_PEM": true, "TF_HTTP_CLIENT_PRIVATE_KEY_PEM": true,
		"TF_HTTP_CLIENT_CA_CERTIFICATE_PEM": true,
	}
	for _, name := range names {
		if !allowed[name] {
			return fmt.Errorf("OpenTofu backend environment allowlist is invalid")
		}
	}
	return nil
}

func validateNames(names []string) error {
	seen := map[string]bool{}
	reserved := map[string]bool{
		"PATH": true, "HOME": true, "TMPDIR": true, "TMP": true, "TEMP": true, "USER": true, "LOGNAME": true,
		"TF_IN_AUTOMATION": true, "TF_INPUT": true, "TF_CLI_CONFIG_FILE": true, "TF_DATA_DIR": true, "TF_ENCRYPTION": true, "CHECKPOINT_DISABLE": true,
	}
	for _, name := range names {
		if name == "" || seen[name] || reserved[name] || strings.ContainsAny(name, "=\x00") || strings.HasPrefix(name, "TF_CLI_ARGS") || strings.HasPrefix(name, "LIFTR_") {
			return fmt.Errorf("OpenTofu environment allowlist is invalid")
		}
		seen[name] = true
	}
	return nil
}

func applySourceDefaults(p *Program) {
	if p.MaxSourceFiles <= 0 {
		p.MaxSourceFiles = defaultMaxFiles
	}
	if p.MaxSourceFileBytes <= 0 {
		p.MaxSourceFileBytes = defaultMaxSourceFile
	}
	if p.MaxSourceBytes <= 0 {
		p.MaxSourceBytes = defaultMaxSourceAll
	}
	if p.MaxSourcePathBytes <= 0 {
		p.MaxSourcePathBytes = defaultMaxPath
	}
}

func sourceLimits(p Program) SourceLimits {
	return SourceLimits{MaxFiles: p.MaxSourceFiles, MaxFileBytes: p.MaxSourceFileBytes, MaxTotalBytes: p.MaxSourceBytes, MaxPathBytes: p.MaxSourcePathBytes}
}

func cloneMapping(mapping OutputMapping) OutputMapping {
	cloned := mapping
	cloned.Fields = make(map[string]string, len(mapping.Fields))
	for key, value := range mapping.Fields {
		cloned.Fields[key] = value
	}
	return cloned
}

func cloneProgram(program Program) Program {
	cloned := program
	cloned.Capabilities = append([]domain.Capability(nil), program.Capabilities...)
	cloned.RequiredEnvironment = append([]string(nil), program.RequiredEnvironment...)
	cloned.ManagedWorkloadAddresses = append([]string(nil), program.ManagedWorkloadAddresses...)
	cloned.ProviderPackages = append([]ProviderPackage(nil), program.ProviderPackages...)
	cloned.ProviderConstraints = make(map[string]string, len(program.ProviderConstraints))
	for address, version := range program.ProviderConstraints {
		cloned.ProviderConstraints[address] = version
	}
	cloned.OutputMappings = make([]OutputMapping, len(program.OutputMappings))
	for i, mapping := range program.OutputMappings {
		cloned.OutputMappings[i] = cloneMapping(mapping)
	}
	return cloned
}

// StateKey derives one stable resource state key. Length framing prevents
// delimiter ambiguity and the function deliberately has no operation-shaped
// inputs.
func StateKey(identity, provisionerRef string, resourceType domain.ResourceTypeRef, resourceID domain.ResourceID) string {
	hash := sha256.New()
	for _, value := range []string{StateKeyVersionV1, identity, provisionerRef, resourceType.Name, resourceType.Version, string(resourceID)} {
		writeFrame(hash, []byte(value))
	}
	return StateKeyVersionV1 + "/" + hex.EncodeToString(hash.Sum(nil))
}
