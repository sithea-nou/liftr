// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sithea-nou/liftr/internal/application"
	"github.com/sithea-nou/liftr/internal/domain"
	"github.com/sithea-nou/liftr/internal/provisioning"
	"github.com/sithea-nou/liftr/internal/provisioning/opentofu"
	"github.com/sithea-nou/liftr/internal/resourcecontract"
)

const maxOpenTofuConfigBytes = 256 << 10

type openTofuConfigSet struct {
	Registrations []openTofuFileConfig  `json:"registrations"`
	Routes        []openTofuRouteConfig `json:"routes"`
}

type openTofuRouteConfig struct {
	ResourceType   domain.ResourceTypeRef `json:"resourceType"`
	ProvisionerRef string                 `json:"provisionerRef"`
}

type openTofuFileConfig struct {
	ProvisionerRef  string                   `json:"provisionerRef"`
	Identity        string                   `json:"identity"`
	Executable      openTofuExecutableConfig `json:"executable"`
	WorkRoot        string                   `json:"workRoot"`
	QuarantineRoot  string                   `json:"quarantineRoot"`
	LockTimeout     string                   `json:"lockTimeout"`
	StateKeyVersion string                   `json:"stateKeyVersion"`
	Program         openTofuProgramConfig    `json:"program"`
	Backend         openTofuBackendConfig    `json:"backend"`

	lockTimeout time.Duration
	ref         application.ProvisionerRef
}

type openTofuExecutableConfig struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type openTofuProgramConfig struct {
	Ref                      string                            `json:"ref"`
	ResourceType             domain.ResourceTypeRef            `json:"resourceType"`
	Capabilities             []domain.Capability               `json:"capabilities"`
	SourceDir                string                            `json:"sourceDir"`
	SourceDigest             string                            `json:"sourceDigest"`
	BuiltInOnly              bool                              `json:"builtInOnly"`
	ProviderConstraints      map[string]string                 `json:"providerConstraints,omitempty"`
	ProviderPackages         []openTofuProviderPackageConfig   `json:"providerPackages,omitempty"`
	ProviderMirror           string                            `json:"providerMirror,omitempty"`
	RequiredEnvironment      []string                          `json:"requiredEnvironment,omitempty"`
	ControlMarkerAddress     string                            `json:"controlMarkerAddress"`
	ManagedWorkloadAddresses []string                          `json:"managedWorkloadAddresses"`
	OutputMappings           []openTofuOutputMappingFileConfig `json:"outputMappings,omitempty"`
	CurrentOutputMappingRef  string                            `json:"currentOutputMappingRef,omitempty"`
}

type openTofuProviderPackageConfig struct {
	Address string `json:"address"`
	Version string `json:"version"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
}

type openTofuOutputMappingFileConfig struct {
	Ref                        string            `json:"ref"`
	EnvelopeName               string            `json:"envelopeName"`
	Fields                     map[string]string `json:"fields"`
	CompatibleSourceMappingRef string            `json:"compatibleSourceMappingRef,omitempty"`
}

type openTofuBackendConfig struct {
	Type                string   `json:"type"`
	Ref                 string   `json:"ref"`
	StateURL            string   `json:"stateURL"`
	LockURL             string   `json:"lockURL"`
	UnlockURL           string   `json:"unlockURL"`
	RequiredEnvironment []string `json:"requiredEnvironment,omitempty"`
}

func loadOpenTofuConfigFile(ctx context.Context, path string, catalog application.ResourceTypeCatalog) (openTofuConfigSet, error) {
	if strings.TrimSpace(path) == "" {
		return openTofuConfigSet{}, errors.New("OpenTofu config file path is required")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return openTofuConfigSet{}, errors.New("OpenTofu config file must be a regular non-symlink file without group or world write access")
	}
	if info.Size() < 0 || info.Size() > maxOpenTofuConfigBytes {
		return openTofuConfigSet{}, fmt.Errorf("OpenTofu config file exceeds %d bytes", maxOpenTofuConfigBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return openTofuConfigSet{}, fmt.Errorf("open OpenTofu config file: %w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxOpenTofuConfigBytes+1))
	if err != nil {
		return openTofuConfigSet{}, fmt.Errorf("read OpenTofu config file: %w", err)
	}
	if len(raw) > maxOpenTofuConfigBytes {
		return openTofuConfigSet{}, fmt.Errorf("OpenTofu config file exceeds %d bytes", maxOpenTofuConfigBytes)
	}
	if err := validateUniqueOpenTofuJSON(raw); err != nil {
		return openTofuConfigSet{}, fmt.Errorf("invalid OpenTofu config JSON: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var config openTofuConfigSet
	if err := decoder.Decode(&config); err != nil {
		return openTofuConfigSet{}, fmt.Errorf("decode OpenTofu config file: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return openTofuConfigSet{}, errors.New("OpenTofu config file must contain exactly one JSON document")
	}
	if err := config.validate(ctx, catalog); err != nil {
		return openTofuConfigSet{}, fmt.Errorf("validate OpenTofu config file: %w", err)
	}
	return config, nil
}

func (c *openTofuConfigSet) validate(ctx context.Context, catalog application.ResourceTypeCatalog) error {
	if len(c.Registrations) == 0 || len(c.Routes) == 0 {
		return errors.New("OpenTofu registrations and routes are required")
	}
	registrations := make(map[string]*openTofuFileConfig, len(c.Registrations))
	for i := range c.Registrations {
		registration := &c.Registrations[i]
		if _, duplicate := registrations[registration.ProvisionerRef]; duplicate {
			return fmt.Errorf("OpenTofu provisioner reference %q is duplicated", registration.ProvisionerRef)
		}
		if err := registration.validate(ctx, catalog); err != nil {
			return fmt.Errorf("registration %q: %w", registration.ProvisionerRef, err)
		}
		registrations[registration.ProvisionerRef] = registration
	}
	routedTypes := make(map[domain.ResourceTypeRef]struct{}, len(c.Routes))
	for _, route := range c.Routes {
		registration, found := registrations[route.ProvisionerRef]
		if !found || !canonicalConfigValue(route.ResourceType.Name) || !canonicalConfigValue(route.ResourceType.Version) || registration.Program.ResourceType != route.ResourceType {
			return fmt.Errorf("OpenTofu route for %s/%s does not match a registration", route.ResourceType.Name, route.ResourceType.Version)
		}
		if _, duplicate := routedTypes[route.ResourceType]; duplicate {
			return fmt.Errorf("OpenTofu resource type route %s/%s is duplicated", route.ResourceType.Name, route.ResourceType.Version)
		}
		routedTypes[route.ResourceType] = struct{}{}
	}
	return nil
}

func (c *openTofuFileConfig) validate(ctx context.Context, catalog application.ResourceTypeCatalog) error {
	if catalog == nil {
		return errors.New("resource type catalog is required")
	}
	var err error
	c.ref, err = application.NewProvisionerRef(c.ProvisionerRef)
	if err != nil {
		return err
	}
	if !canonicalConfigValue(c.ProvisionerRef) || !canonicalConfigValue(c.Identity) {
		return errors.New("provisionerRef and identity must be canonical non-empty strings")
	}
	if !filepath.IsAbs(c.Executable.Path) || !validConfigSHA256(c.Executable.SHA256) {
		return errors.New("executable requires an absolute path and exact SHA-256")
	}
	if !filepath.IsAbs(c.WorkRoot) || !filepath.IsAbs(c.QuarantineRoot) || filepath.Clean(c.WorkRoot) == filepath.Clean(c.QuarantineRoot) {
		return errors.New("workRoot and quarantineRoot must be distinct absolute paths")
	}
	c.lockTimeout, err = time.ParseDuration(c.LockTimeout)
	if err != nil || c.lockTimeout <= 0 || c.lockTimeout > 10*time.Minute {
		return errors.New("lockTimeout must be a positive duration no greater than 10m")
	}
	if c.StateKeyVersion != opentofu.StateKeyVersionV1 {
		return fmt.Errorf("stateKeyVersion must be %q", opentofu.StateKeyVersionV1)
	}
	if !canonicalConfigValue(c.Program.Ref) || !filepath.IsAbs(c.Program.SourceDir) || !validConfigSHA256(c.Program.SourceDigest) {
		return errors.New("program requires a canonical ref, absolute sourceDir, and exact sourceDigest")
	}
	if !canonicalConfigValue(c.Program.ResourceType.Name) || !canonicalConfigValue(c.Program.ResourceType.Version) {
		return errors.New("program resourceType is required")
	}
	contract, err := catalog.Get(ctx, c.Program.ResourceType)
	if err != nil || contract == nil {
		return fmt.Errorf("resource type %s/%s is not registered", c.Program.ResourceType.Name, c.Program.ResourceType.Version)
	}
	if err := validateOpenTofuCapabilities(c.Program.Capabilities, contract.Domain()); err != nil {
		return err
	}
	if err := validateOpenTofuSupplyConfig(c.Program); err != nil {
		return err
	}
	if !canonicalConfigValue(c.Program.ControlMarkerAddress) || len(c.Program.ManagedWorkloadAddresses) == 0 {
		return errors.New("control marker and managed workload addresses are required")
	}
	if err := validateConfigStrings(c.Program.ManagedWorkloadAddresses, "managed workload address"); err != nil {
		return err
	}
	if err := validateOpenTofuProgramEnvironmentNames(c.Program.RequiredEnvironment); err != nil {
		return err
	}
	if err := validateOpenTofuOutputMappings(c.Program, contract.OutputContract()); err != nil {
		return err
	}
	if c.Backend.Type != "http" || !canonicalConfigValue(c.Backend.Ref) {
		return errors.New("backend must be a named HTTP profile")
	}
	for name, raw := range map[string]string{"state": c.Backend.StateURL, "lock": c.Backend.LockURL, "unlock": c.Backend.UnlockURL} {
		parsed, parseErr := url.Parse(raw)
		if parseErr != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("backend %s URL must be an absolute credential-free HTTPS URL without query or fragment", name)
		}
	}
	if c.Backend.StateURL == c.Backend.LockURL || c.Backend.StateURL == c.Backend.UnlockURL || c.Backend.LockURL == c.Backend.UnlockURL {
		return errors.New("backend state, lock, and unlock URLs must be distinct")
	}
	return validateOpenTofuBackendEnvironmentNames(c.Backend.RequiredEnvironment)
}

func validateOpenTofuCapabilities(capabilities []domain.Capability, resourceType domain.ResourceType) error {
	if len(capabilities) == 0 {
		return errors.New("program capabilities are required")
	}
	seen := make(map[domain.Capability]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if capability != domain.CapabilityCreate && capability != domain.CapabilityUpdate && capability != domain.CapabilityDelete {
			return fmt.Errorf("OpenTofu capability %q is unsupported", capability)
		}
		if _, duplicate := seen[capability]; duplicate {
			return fmt.Errorf("OpenTofu capability %q is duplicated", capability)
		}
		if !resourceType.Supports(capability) {
			return fmt.Errorf("resource type does not support capability %q", capability)
		}
		seen[capability] = struct{}{}
	}
	if _, creates := seen[domain.CapabilityCreate]; !creates {
		return errors.New("a routed OpenTofu registration must support create")
	}
	return nil
}

func validateOpenTofuSupplyConfig(program openTofuProgramConfig) error {
	if program.BuiltInOnly {
		if len(program.ProviderConstraints) != 0 || len(program.ProviderPackages) != 0 || program.ProviderMirror != "" {
			return errors.New("built-in-only program cannot declare external provider configuration")
		}
		return nil
	}
	if len(program.ProviderConstraints) == 0 || len(program.ProviderPackages) != len(program.ProviderConstraints) || !filepath.IsAbs(program.ProviderMirror) {
		return errors.New("external providers require exact constraints, one package each, and an absolute offline mirror")
	}
	seen := make(map[string]struct{}, len(program.ProviderPackages))
	for address, version := range program.ProviderConstraints {
		if !canonicalConfigValue(address) || !canonicalConfigValue(version) || strings.ContainsAny(version, "<>=~! ,") {
			return errors.New("external provider constraints must be exact")
		}
	}
	for _, pkg := range program.ProviderPackages {
		version, registered := program.ProviderConstraints[pkg.Address]
		if !registered || version != pkg.Version || !filepath.IsAbs(pkg.Path) || !validConfigSHA256(pkg.SHA256) {
			return errors.New("external provider package does not match its exact constraint")
		}
		if _, duplicate := seen[pkg.Address]; duplicate {
			return errors.New("external provider package is duplicated")
		}
		seen[pkg.Address] = struct{}{}
	}
	return nil
}

func validateOpenTofuOutputMappings(program openTofuProgramConfig, outputContract *resourcecontract.OutputContract) error {
	if outputContract == nil {
		if len(program.OutputMappings) != 0 || program.CurrentOutputMappingRef != "" {
			return errors.New("resource type without outputs cannot register OpenTofu output mappings")
		}
		return nil
	}
	if len(program.OutputMappings) == 0 || !canonicalConfigValue(program.CurrentOutputMappingRef) {
		return errors.New("resource type outputs require mappings and a current mapping ref")
	}
	declared := make(map[string]struct{})
	required := make(map[string]struct{})
	for _, field := range outputContract.Fields() {
		declared[field.Name] = struct{}{}
		if field.RequiredWhenReady {
			required[field.Name] = struct{}{}
		}
	}
	refs := make(map[string]struct{}, len(program.OutputMappings))
	for _, mapping := range program.OutputMappings {
		if !canonicalConfigValue(mapping.Ref) || !canonicalConfigValue(mapping.EnvelopeName) {
			return errors.New("OpenTofu output mapping ref and envelopeName are required")
		}
		if _, duplicate := refs[mapping.Ref]; duplicate {
			return errors.New("OpenTofu output mapping ref is duplicated")
		}
		refs[mapping.Ref] = struct{}{}
		if len(mapping.Fields) == 0 || len(mapping.Fields) > len(declared) {
			return errors.New("OpenTofu output mapping targets must be allowed by the resource type output contract")
		}
		sources := make(map[string]struct{}, len(mapping.Fields))
		for target, source := range mapping.Fields {
			if _, allowed := declared[target]; !allowed || !canonicalConfigValue(source) {
				return errors.New("OpenTofu output mapping targets must be allowed by the resource type output contract")
			}
			if _, duplicate := sources[source]; duplicate {
				return errors.New("OpenTofu output mapping source field is duplicated")
			}
			sources[source] = struct{}{}
		}
		for name := range required {
			if _, mapped := mapping.Fields[name]; !mapped {
				return errors.New("OpenTofu output mapping omits a required resource type output field")
			}
		}
		if mapping.CompatibleSourceMappingRef != "" && !canonicalConfigValue(mapping.CompatibleSourceMappingRef) {
			return errors.New("compatible source mapping ref must be canonical")
		}
	}
	if _, found := refs[program.CurrentOutputMappingRef]; !found {
		return errors.New("current OpenTofu output mapping ref is not registered")
	}
	return nil
}

func (c openTofuFileConfig) adapterConfig(evidence opentofu.EvidenceStore) opentofu.Config {
	packages := make([]opentofu.ProviderPackage, len(c.Program.ProviderPackages))
	for i, pkg := range c.Program.ProviderPackages {
		packages[i] = opentofu.ProviderPackage{Address: pkg.Address, Version: pkg.Version, Path: pkg.Path, SHA256: pkg.SHA256}
	}
	mappings := make([]opentofu.OutputMapping, len(c.Program.OutputMappings))
	for i, mapping := range c.Program.OutputMappings {
		fields := make(map[string]string, len(mapping.Fields))
		for target, source := range mapping.Fields {
			fields[target] = source
		}
		mappings[i] = opentofu.OutputMapping{Ref: mapping.Ref, EnvelopeName: mapping.EnvelopeName, Fields: fields, CompatibleSourceMappingRef: mapping.CompatibleSourceMappingRef}
	}
	environmentNames := append([]string(nil), c.Program.RequiredEnvironment...)
	environmentNames = append(environmentNames, c.Backend.RequiredEnvironment...)
	return opentofu.Config{
		Executable: c.Executable.Path, ExecutableSHA256: c.Executable.SHA256,
		WorkRoot: c.WorkRoot, QuarantineRoot: c.QuarantineRoot, LockTimeout: c.lockTimeout, Evidence: evidence,
		Registration: opentofu.Registration{
			ProvisionerRef: c.ProvisionerRef, Identity: c.Identity, StateKeyVersion: c.StateKeyVersion,
			Environment: openTofuEnvironmentProvider(environmentNames),
			Program: opentofu.Program{
				Ref: c.Program.Ref, ResourceType: c.Program.ResourceType, Capabilities: append([]domain.Capability(nil), c.Program.Capabilities...),
				SourceDir: c.Program.SourceDir, SourceDigest: c.Program.SourceDigest, BuiltInOnly: c.Program.BuiltInOnly,
				ProviderConstraints: cloneStringMap(c.Program.ProviderConstraints), ProviderPackages: packages, ProviderMirror: c.Program.ProviderMirror,
				EncodeInput: genericOpenTofuInputEncoder, RequiredEnvironment: append([]string(nil), c.Program.RequiredEnvironment...),
				ControlMarkerAddress: c.Program.ControlMarkerAddress, ManagedWorkloadAddresses: append([]string(nil), c.Program.ManagedWorkloadAddresses...),
				OutputMappings: mappings, CurrentOutputMappingRef: c.Program.CurrentOutputMappingRef,
			},
			Backend: opentofu.BackendProfile{
				Ref: c.Backend.Ref, StateURL: c.Backend.StateURL, LockURL: c.Backend.LockURL, UnlockURL: c.Backend.UnlockURL,
				RequiredEnvironment: append([]string(nil), c.Backend.RequiredEnvironment...),
			},
		},
	}
}

func composeOpenTofuProvisioners(ctx context.Context, path string, catalog application.ResourceTypeCatalog, evidence opentofu.EvidenceStore) (map[application.ProvisionerRef]provisioning.Provisioner, map[domain.ResourceTypeRef]application.ProvisionerRef, error) {
	config, err := loadOpenTofuConfigFile(ctx, path, catalog)
	if err != nil {
		return nil, nil, err
	}
	providers := make(map[application.ProvisionerRef]provisioning.Provisioner, len(config.Registrations))
	for _, registration := range config.Registrations {
		provider, err := opentofu.New(registration.adapterConfig(evidence))
		if err != nil {
			return nil, nil, fmt.Errorf("compose OpenTofu provisioner %q: %w", registration.ProvisionerRef, err)
		}
		providers[registration.ref] = provider
	}
	routes := make(map[domain.ResourceTypeRef]application.ProvisionerRef, len(config.Routes))
	for _, route := range config.Routes {
		ref, _ := application.NewProvisionerRef(route.ProvisionerRef)
		routes[route.ResourceType] = ref
	}
	return providers, routes, nil
}

func genericOpenTofuInputEncoder(input opentofu.Input) (map[string]any, error) {
	return map[string]any{"spec": input.Spec.Values(), "desired_present": input.DesiredPresent}, nil
}

func openTofuEnvironmentProvider(names []string) opentofu.EnvironmentProvider {
	declared := append([]string(nil), names...)
	return func(context.Context) (map[string]string, error) {
		values := make(map[string]string, len(declared))
		for _, name := range declared {
			if value, present := os.LookupEnv(name); present {
				values[name] = value
			}
		}
		return values, nil
	}
}

func validateOpenTofuProgramEnvironmentNames(names []string) error {
	if err := validateOpenTofuEnvironmentNames(names); err != nil {
		return err
	}
	for _, name := range names {
		if strings.HasPrefix(name, "TF_") || strings.HasPrefix(name, "TOFU_") {
			return errors.New("OpenTofu program environment names are invalid")
		}
	}
	return nil
}

func validateOpenTofuBackendEnvironmentNames(names []string) error {
	if err := validateOpenTofuEnvironmentNames(names); err != nil {
		return err
	}
	allowed := map[string]struct{}{
		"TF_HTTP_USERNAME": {}, "TF_HTTP_PASSWORD": {},
		"TF_HTTP_CLIENT_CERTIFICATE_PEM": {}, "TF_HTTP_CLIENT_PRIVATE_KEY_PEM": {},
		"TF_HTTP_CLIENT_CA_CERTIFICATE_PEM": {},
	}
	for _, name := range names {
		if _, ok := allowed[name]; !ok {
			return errors.New("OpenTofu backend environment names are invalid")
		}
	}
	return nil
}

func validateOpenTofuEnvironmentNames(names []string) error {
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if !canonicalConfigValue(name) || strings.ContainsAny(name, "=\x00") || strings.HasPrefix(name, "LIFTR_") || strings.HasPrefix(name, "TF_CLI_ARGS") {
			return errors.New("OpenTofu environment names are invalid")
		}
		if _, duplicate := seen[name]; duplicate {
			return errors.New("OpenTofu environment name is duplicated")
		}
		seen[name] = struct{}{}
	}
	return nil
}

func validateConfigStrings(values []string, label string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !canonicalConfigValue(value) {
			return fmt.Errorf("%s must be canonical and non-empty", label)
		}
		if _, duplicate := seen[value]; duplicate {
			return fmt.Errorf("%s is duplicated", label)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func canonicalConfigValue(value string) bool {
	return value != "" && value == strings.TrimSpace(value)
}

func validConfigSHA256(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func cloneStringMap(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func validateUniqueOpenTofuJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := scanOpenTofuJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func scanOpenTofuJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("nesting exceeds 32 levels")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("object member name is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate object member %q", key)
			}
			seen[key] = struct{}{}
			if err := scanOpenTofuJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("unterminated object")
		}
	case '[':
		for decoder.More() {
			if err := scanOpenTofuJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("unterminated array")
		}
	default:
		return fmt.Errorf("unexpected delimiter %q", delimiter)
	}
	return nil
}
